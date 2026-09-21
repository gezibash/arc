#!/bin/bash
# The proofs of phases A and B of docs/interface/SPEC.md. Two providers announce
# manifests of interface version 1. A caller installs them, and runs their
# commands as commands of arcn, with no code for them in arcn.
#
#     mise run interface
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
keep="${ARC_KEEP_WORK:-}"

cleanup() {
  [ -n "${relay_pid:-}" ] && kill "$relay_pid" 2>/dev/null || true
  [ -n "${exec_pid:-}" ] && kill "$exec_pid" 2>/dev/null || true
  [ -n "${sqlite_pid:-}" ] && kill "$sqlite_pid" 2>/dev/null || true
  # A process started from the work directory must not outlive it.
  pkill -f "$work/" 2>/dev/null || true
  sleep 0.2
  pkill -9 -f "$work/" 2>/dev/null || true
  [ -n "$keep" ] && printf 'work: %s\n' "$work" || rm -rf "$work"
}
trap cleanup EXIT

say() { printf 'ok   %s\n' "$1"; }
fail() { printf 'FAIL %s\n' "$1"; exit 1; }

cd "$root"
go build -o "$work/arcn" ./cmd/arcn
go build -o "$work/exec-provider" ./cmd/exec-provider
go build -o "$work/sqlite-provider" ./cmd/sqlite-provider
say "arcn and two providers build"

"$work/arcn" --home "$work/relay" relay serve --listen 127.0.0.1:0 > "$work/relay.log" 2>&1 &
relay_pid=$!
for _ in $(seq 1 50); do grep "listens on" "$work/relay.log" > /dev/null 2>&1 && break; sleep 0.1; done
url="$(sed -n 's/^relay listens on //p' "$work/relay.log")"
[ -n "$url" ] || fail "the relay did not start: $(cat "$work/relay.log")"
say "a relay listens on $url"

caller() { "$work/arcn" --home "$work/caller" "$@"; }
caller key new > /dev/null
caller relay add "$url"
caller_key="$(caller key show | tail -1)"

for name in exec sqlite; do
  "$work/arcn" --home "$work/$name" key new > /dev/null
  "$work/arcn" --home "$work/$name" relay add "$url"
done
exec_key="$("$work/arcn" --home "$work/exec" key show | tail -1)"
sqlite_key="$("$work/arcn" --home "$work/sqlite" key show | tail -1)"

mkdir -p "$work/jobs"
cat > "$work/exec.json" <<JSON
{"grants": ["$caller_key"], "cwd": "$work", "jobs_dir": "$work/jobs"}
JSON
cat > "$work/sqlite.json" <<JSON
{"databases": {"main": {"path": "$work/main.db", "grants": {"$caller_key": "write"}}}}
JSON

# The manifest= of the URI names the older manifest; arcn announces the
# interface.json beside it.
EXEC_CONFIG="$work/exec.json" "$work/arcn" --home "$work/exec" serve \
  "exec://$work/exec-provider?manifest=$root/cmd/exec-provider/manifest.json" > "$work/exec.log" 2>&1 &
exec_pid=$!
SQLITE_CONFIG="$work/sqlite.json" "$work/arcn" --home "$work/sqlite" serve \
  "exec://$work/sqlite-provider?manifest=$root/cmd/sqlite-provider/manifest.json" > "$work/sqlite.log" 2>&1 &
sqlite_pid=$!
for log in exec sqlite; do
  for _ in $(seq 1 50); do grep "serves" "$work/$log.log" > /dev/null 2>&1 && break; sleep 0.1; done
  grep "serves $log" "$work/$log.log" > /dev/null || fail "the $log provider did not serve: $(cat "$work/$log.log")"
done
say "two providers announce manifests of interface version 1"

caller install "$exec_key" --yes > "$work/install.txt" || fail "the install failed: $(cat "$work/install.txt")"
grep "a service with 3 commands, interface version 1" "$work/install.txt" > /dev/null ||
  fail "the install did not show the manifest: $(cat "$work/install.txt")"
grep "installed exec" "$work/install.txt" > /dev/null || fail "exec did not install as exec"
caller install "$sqlite_key" --as db --yes | grep "installed db" > /dev/null || fail "sqlite did not install as db"
say "the caller installs exec as exec, and sqlite as db"

# The proof of phase A.
[ "$(caller exec run echo hello)" = "hello" ] || fail "arcn exec run echo hello answered $(caller exec run echo hello 2>&1)"
say "arcn exec run echo hello answers through the installed manifest"

caller help exec | grep "arcn exec start <script...>" > /dev/null || fail "help does not list the commands"
caller exec run --help | grep "usage: arcn exec run <argv...>" > /dev/null || fail "a command has no help"
say "help comes from the manifest"

caller exec run --json sh -c 'echo out; exit 3' > "$work/json.txt"
grep '"exit":3' "$work/json.txt" > /dev/null || fail "--json wrote $(cat "$work/json.txt")"
say "--json writes the record"

caller db "create table people (id integer, name text)" > /dev/null
caller db "insert into people values (1, 'ada'), (22, 'grace hopper')" > /dev/null
caller db select id, name from people order by id > "$work/table.txt"
[ "$(cat "$work/table.txt")" = "$(printf 'id  name\n1   ada\n22  grace hopper')" ] ||
  fail "the table is $(cat "$work/table.txt")"
say "arcn db shows rows as a table"

if caller exec run > /dev/null 2> "$work/missing.txt"; then fail "a missing argument ran"; fi
grep "missing <argv...>" "$work/missing.txt" > /dev/null || fail "the error was $(cat "$work/missing.txt")"
if caller exec fly > /dev/null 2> "$work/nope.txt"; then fail "an unknown command ran"; fi
grep 'no command "fly"' "$work/nope.txt" > /dev/null || fail "the error was $(cat "$work/nope.txt")"
say "arcn checks the arguments before it calls"

if caller install "$exec_key" --as relay --yes > /dev/null 2> "$work/as.txt"; then fail "a capability took the name of a command"; fi
grep "is a command of arcn" "$work/as.txt" > /dev/null || fail "the error was $(cat "$work/as.txt")"
say "a capability cannot take the name of a command of arcn"

"$work/arcn" --home "$work/stranger" key new > /dev/null
"$work/arcn" --home "$work/stranger" relay add "$url"
"$work/arcn" --home "$work/stranger" install "$exec_key" --yes > /dev/null
if "$work/arcn" --home "$work/stranger" exec run id > /dev/null 2> "$work/denied.txt"; then
  fail "a caller without a grant ran a command"
fi
grep "access_denied" "$work/denied.txt" > /dev/null || fail "the refusal was $(cat "$work/denied.txt")"
say "a caller without a grant is refused"

caller exec run --later echo later 2> "$work/later.txt"
grep "queued" "$work/later.txt" > /dev/null || fail "--later did not queue: $(cat "$work/later.txt")"
say "--later queues a live call in the outbox"

printf 'phase A holds: manifests, arguments, templates, call, format\n\n'

# Phase B: sealed data. The journal and the files are manifests, with no
# code for them in arcn. Two machines hold one key.
laptop() { caller "$@"; }
desktop() { "$work/arcn" --home "$work/desktop" "$@"; }
mkdir -p "$work/desktop"
cp "$work/caller/key" "$work/desktop/key"
desktop relay add "$url"

laptop announce "$root/manifests/journal.json" > /dev/null
laptop announce "$root/manifests/files.json" > /dev/null
for machine in laptop desktop; do
  $machine install "$caller_key" journal --yes > /dev/null || fail "$machine did not install the journal"
  $machine install "$caller_key" files --yes > /dev/null || fail "$machine did not install the files"
done
say "two machines install the journal and the files from their author"

printf 'auc 0.871\n' | laptop journal write hrs/ablations/lr-sweep --title "LR sweep"
laptop journal append hrs/ablations/lr-sweep next: try warmup
[ "$(desktop journal read hrs/ablations/lr-sweep)" = "$(printf 'auc 0.871\nnext: try warmup')" ] ||
  fail "the desktop read $(desktop journal read hrs/ablations/lr-sweep)"
desktop journal ls | grep "hrs/ablations/lr-sweep	LR sweep" > /dev/null || fail "ls shows $(desktop journal ls)"
say "a page that is written and appended on one machine reads on the other"

[ "$(desktop journal history hrs/ablations/lr-sweep | grep -c '^20')" = 2 ] ||
  fail "the history is $(desktop journal history hrs/ablations/lr-sweep)"
desktop journal search warmup | grep "^hrs/ablations/lr-sweep" > /dev/null || fail "search found nothing"
say "the page has two revisions, and search finds it"

laptop journal kpi set hrs auc 0.85
laptop journal kpi set hrs auc 0.87 --note warmup
desktop journal kpi latest hrs | grep "auc	0.87	warmup" > /dev/null || fail "kpi latest shows $(desktop journal kpi latest hrs)"
[ "$(desktop journal kpi log hrs auc | wc -l | tr -d ' ')" = 2 ] || fail "kpi log shows $(desktop journal kpi log hrs auc)"
say "a KPI keeps its last value and its log"

head -c 200000 /dev/urandom > "$work/weights.bin"
id="$(laptop files put "$work/weights.bin")"
desktop files list | grep "weights.bin	200000 bytes" > /dev/null || fail "files list shows $(desktop files list)"
desktop files get "$id" --output "$work/weights-back.bin" 2> /dev/null
cmp -s "$work/weights.bin" "$work/weights-back.bin" || fail "the file came back changed"
say "a binary file of 200000 bytes crosses the relay in parts, and its hash checks"

# A stick made before the delete must not bring the page back.
laptop sync --dir "$work/old-stick" > /dev/null
laptop journal delete hrs/ablations/lr-sweep
[ -z "$(desktop journal read hrs/ablations/lr-sweep)" ] || fail "the desktop still reads the deleted page"
desktop sync --dir "$work/old-stick" > /dev/null 2>&1
[ -z "$(desktop journal read hrs/ablations/lr-sweep)" ] || fail "an old stick brought the deleted page back"
desktop journal history hrs/ablations/lr-sweep | grep "no revisions" > /dev/null || fail "the history outlived the delete"
say "delete removes the page and its history, and an old stick does not bring it back"

# With no relay, the page crosses a stick.
laptop relay rm "$url"
desktop relay rm "$url"
printf 'carried by hand\n' | laptop journal write hrs/notes/offline
laptop sync --dir "$work/stick" > /dev/null
desktop sync --dir "$work/stick" > /dev/null
[ "$(desktop journal read hrs/notes/offline)" = "carried by hand" ] || fail "the stick did not carry the page"
say "with no relay, a page crosses a USB stick"

printf 'phase B holds: drafts, checkpoints, parts, delete, the journal and the files\n'
