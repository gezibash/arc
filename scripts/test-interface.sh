#!/bin/bash
# The proofs of phases A to D of docs/interface/SPEC.md. Two providers announce
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
  [ -n "${board_pid:-}" ] && kill "$board_pid" 2>/dev/null || true
  [ -n "${bunker_pid:-}" ] && kill "$bunker_pid" 2>/dev/null || true
  # A process started from the work directory must not outlive it.
  pkill -f "$work/" 2>/dev/null || true
  sleep 0.2
  pkill -9 -f "$work/" 2>/dev/null || true
  [ -n "$keep" ] && printf 'work: %s\n' "$work" || rm -rf "$work"
}
trap cleanup EXIT

say() { printf 'ok   %s\n' "$1"; }
fail() { printf 'FAIL %s\n' "$1"; exit 1; }
# keyfile names the key file of the one identity of a home.
keyfile() { echo "$work/$1"/citizens/*/key; }

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
caller keys gen > /dev/null
caller relay add "$url"
caller_key="$(caller whoami | sed -n 2p)"

for name in exec sqlite; do
  "$work/arcn" --home "$work/$name" keys gen > /dev/null
  "$work/arcn" --home "$work/$name" relay add "$url"
done
exec_key="$("$work/arcn" --home "$work/exec" whoami | sed -n 2p)"
sqlite_key="$("$work/arcn" --home "$work/sqlite" whoami | sed -n 2p)"

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

"$work/arcn" --home "$work/stranger" keys gen > /dev/null
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
desktop keys add < "$(keyfile caller)" > /dev/null
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

printf 'phase B holds: drafts, checkpoints, parts, delete, the journal and the files\n\n'

# Phase C: private kinds through the mail layer, and a NIP-29 group on a
# relay that enforces it. Direct messages and Agora are manifests.
laptop relay add "$url"
bob() { "$work/arcn" --home "$work/bob" "$@"; }
moderator() { "$work/arcn" --home "$work/moderator" "$@"; }
bob keys gen > /dev/null
bob relay add "$url"
bob_key="$(bob whoami | sed -n 2p)"
moderator keys gen > /dev/null
moderator relay add "$url"
moderator_key="$(moderator whoami | sed -n 2p)"

"$work/arcn" --home "$work/board" relay serve --listen 127.0.0.1:0 \
  --group agora --admin "$moderator_key" > "$work/board.log" 2>&1 &
board_pid=$!
for _ in $(seq 1 50); do grep "listens on" "$work/board.log" > /dev/null 2>&1 && break; sleep 0.1; done
board="$(sed -n 's/^relay listens on //p' "$work/board.log")"
[ -n "$board" ] || fail "the board relay did not start: $(cat "$work/board.log")"
grep "hosts groups agora" "$work/board.log" > /dev/null || fail "the board hosts no group"
say "a relay hosts the NIP-29 group agora, with one admin"

# The author of Agora names the board relay in the manifest.
sed "s#wss://board.example#$board#" "$root/manifests/agora.json" > "$work/agora.json"
laptop announce "$root/manifests/dm.json" > /dev/null
laptop announce "$work/agora.json" > /dev/null
for machine in laptop bob moderator; do
  $machine install "$caller_key" dm --yes > /dev/null || fail "$machine did not install dm"
  $machine install "$caller_key" agora --yes > /dev/null || fail "$machine did not install agora"
done
say "three citizens install dm and agora"

laptop dm send "$bob_key" meet at noon 2> /dev/null
bob dm inbox | grep "meet at noon" > /dev/null || fail "bob's inbox: $(bob dm inbox)"
bob dm send "$caller_key" see you there 2> /dev/null
laptop dm open "$bob_key" > "$work/conversation.txt"
grep "meet at noon" "$work/conversation.txt" > /dev/null && grep "see you there" "$work/conversation.txt" > /dev/null ||
  fail "the conversation is $(cat "$work/conversation.txt")"
go test -count=1 -run 'NIP17' ./delivery/mail/ > "$work/nip17.txt" 2>&1 || fail "NIP-17: $(cat "$work/nip17.txt")"
say "a direct message crosses the relay, both ways, and opens in a NIP-17 client"

# A list stands for its members where a command takes a key.
if laptop lists add dm pals not-a-key > /dev/null 2>&1; then fail "a list took a member that is not a key"; fi
if laptop lists add nothing pals "$bob_key" > /dev/null 2>&1; then fail "a list took a command that is not installed"; fi
laptop lists add dm pals "$bob_key" "$(moderator whoami | sed -n 2p)" > /dev/null || fail "the list was not saved"
[ "$(laptop lists ls dm pals | wc -l | tr -d ' ')" = 2 ] || fail "the list holds $(laptop lists ls dm pals)"
laptop dm send pals hello to the list 2> /dev/null || fail "the message to the list failed"
bob dm inbox | grep "hello to the list" > /dev/null || fail "bob did not receive the message to the list"
moderator dm inbox | grep "hello to the list" > /dev/null || fail "the moderator did not receive the message to the list"
laptop lists rm dm pals > /dev/null
laptop lists ls dm | grep "no lists" > /dev/null || fail "the removed list still shows: $(laptop lists ls dm)"
say "a list of two citizens sends one message to each"

post="$(laptop agora post --title Hello first post 2>&1 > /dev/null | tail -1)"
case "$post" in nevent1*) ;; *) fail "the post printed $post" ;; esac
bob agora feed | grep "Hello" > /dev/null || fail "bob's feed: $(bob agora feed)"
bob agora reply "$post" welcome 2> /dev/null
laptop agora thread "$post" | grep "welcome" > /dev/null || fail "the thread: $(laptop agora thread "$post")"
say "a post and a reply cross the board relay"

if bob agora remove "$post" > /dev/null 2> "$work/remove.txt"; then fail "a citizen who is not an admin removed the post"; fi
grep "only an admin" "$work/remove.txt" > /dev/null || fail "the refusal was $(cat "$work/remove.txt")"
moderator agora remove "$post" 2> "$work/admin.txt" || fail "the admin could not remove the post: $(cat "$work/admin.txt")"
bob agora feed | grep "no posts" > /dev/null || fail "the removed post still shows: $(bob agora feed)"
go test -count=1 -run 'NIP29' ./delivery/groups/ > "$work/nip29.txt" 2>&1 || fail "NIP-29: $(cat "$work/nip29.txt")"
say "only the admin removes the post, and the post opens in a NIP-29 client"

printf 'phase C holds: private kinds, NIP-29 groups, direct messages and Agora\n\n'

# Phase D: what a capability can do stays what the citizen agreed to, and a
# key can be sealed, or held by a remote signer.
sed 's/"kind": 14, "visibility": "private"}/"kind": 14, "visibility": "private"}, "profile": {"kind": 0, "visibility": "public"}/' \
  "$root/manifests/dm.json" > "$work/dm-profile.json"
if laptop announce "$work/dm-profile.json" > /dev/null 2> "$work/reserved.txt"; then fail "a manifest named a reserved kind"; fi
grep "reserved" "$work/reserved.txt" > /dev/null || fail "the refusal was $(cat "$work/reserved.txt")"
say "a manifest that names a reserved kind does not announce"

sed 's/"kind": 14, "visibility": "private"}/"kind": 14, "visibility": "private"}, "note": {"kind": 1, "visibility": "public"}/' \
  "$root/manifests/dm.json" > "$work/dm-note.json"
sleep 1
laptop announce "$work/dm-note.json" > /dev/null
if bob dm inbox > /dev/null 2> "$work/consent.txt"; then fail "a new kind ran without consent"; fi
grep "changed what dm can do" "$work/consent.txt" > /dev/null && grep "kind 1 (note)" "$work/consent.txt" > /dev/null ||
  fail "the refusal was $(cat "$work/consent.txt")"
bob install "$caller_key" dm --yes > "$work/reinstall.txt"
grep "posted in public, signed by you" "$work/reinstall.txt" > /dev/null || fail "install did not show the new kind: $(cat "$work/reinstall.txt")"
bob dm inbox | grep "meet at noon" > /dev/null || fail "the inbox after consent: $(bob dm inbox)"
say "a new version that adds a public kind stops until the citizen installs again"

laptop journal write hrs/notes/draft --dry-run < /dev/null > "$work/dry.txt" 2> /dev/null
grep '"kind": 30023' "$work/dry.txt" > /dev/null || fail "the dry run showed $(cat "$work/dry.txt")"
[ -z "$(laptop journal read hrs/notes/draft)" ] || fail "a dry run wrote the page"
say "--dry-run shows the event, and signs nothing"

ARCN_PASSPHRASE="correct horse" "$work/arcn" --home "$work/sealed" keys gen --encrypt > "$work/sealed.txt"
head -c 10 "$(keyfile sealed)" | grep "ncryptsec1" > /dev/null || fail "the key file is not an ncryptsec"
[ "$("$work/arcn" --home "$work/sealed" whoami | head -2)" = "$(cat "$work/sealed.txt")" ] || fail "the sealed key is another identity"
ARCN_PASSPHRASE="correct horse" "$work/arcn" --home "$work/sealed" message outbox > /dev/null || fail "the passphrase did not open the key"
if ARCN_PASSPHRASE=wrong "$work/arcn" --home "$work/sealed" message outbox > /dev/null 2>&1; then fail "a wrong passphrase opened the key"; fi
say "a key sealed with a passphrase opens with it, and not without it"

# An agent signs through its owner's bunker, and holds no secret key.
owner() { "$work/arcn" --home "$work/owner" "$@"; }
agent() { "$work/arcn" --home "$work/agent" "$@"; }
owner keys gen > /dev/null
owner_key="$(owner whoami | sed -n 2p)"
# A background process starts directly, not through a shell function, so $!
# names it and kill reaches it. The owner allows posts, drafts and their parts, seals, and relay lists;
# not replies.
"$work/arcn" --home "$work/owner" keys bunker --relay "$url" --allow-kind 11 --allow-kind 31234 --allow-kind 1234 --allow-kind 3275 \
  --allow-kind 13 --allow-kind 10050 --allow-kind 10013 > "$work/bunker.log" 2>&1 &
bunker_pid=$!
for _ in $(seq 1 50); do grep "bunker://" "$work/bunker.log" > /dev/null 2>&1 && break; sleep 0.1; done
uri="$(grep "^bunker://" "$work/bunker.log")"
[ -n "$uri" ] || fail "the bunker did not start: $(cat "$work/bunker.log")"
agent keys add "$uri" > "$work/agent.txt" || fail "the agent could not use the bunker: $(cat "$work/agent.txt")"
[ "$(tail -1 "$work/agent.txt")" = "$owner_key" ] || fail "the agent is $(cat "$work/agent.txt"), not the owner"
grep -r "$(cat "$(keyfile owner)")" "$work/agent" > /dev/null 2>&1 && fail "the agent holds the owner's secret key"
agent relay add "$url" 2> /dev/null
for capability in agora journal dm; do
  agent install "$caller_key" $capability --yes > /dev/null || fail "the agent did not install $capability"
done
say "an agent signs as its owner through a bunker, and holds no secret key"

agent agora post --title "From the agent" signed remotely 2> /dev/null || fail "the agent could not post"
bob agora feed | grep "From the agent" > /dev/null || fail "the agent's post is not on the board: $(bob agora feed)"
bob agora feed | grep "$(owner whoami | head -1)" > /dev/null || fail "the post does not name the owner"
agent_post="$(bob agora feed | grep -o 'nevent1[a-z0-9]*' | head -1)"
if agent agora reply "$agent_post" not allowed > /dev/null 2> "$work/refused-kind.txt"; then fail "the bunker signed a kind it does not allow"; fi
grep "does not sign kind 1111" "$work/refused-kind.txt" > /dev/null || fail "the refusal was $(cat "$work/refused-kind.txt")"
say "the bunker signs the kinds its owner allows, and refuses the rest"

printf 'written by the agent\n' | agent journal write ops/agent/notes 2> "$work/agent-journal.txt" ||
  fail "the agent could not write a page: $(cat "$work/agent-journal.txt")"
[ "$(agent journal read ops/agent/notes)" = "written by the agent" ] || fail "the agent read $(agent journal read ops/agent/notes)"
owner relay add "$url" 2> /dev/null
owner install "$caller_key" journal --yes > /dev/null
[ "$(owner journal read ops/agent/notes)" = "written by the agent" ] ||
  fail "the owner, with the key, read $(owner journal read ops/agent/notes)"
say "an agent keeps a journal through the bunker, and its owner reads the same page with the key"

# By default the bunker decrypts only what its owner sealed to themselves:
# the journal works, and incoming mail stays shut.
agent dm send "$bob_key" hello from the agent 2> /dev/null || fail "the agent could not send a message"
bob dm inbox | grep "hello from the agent" > /dev/null || fail "bob's inbox: $(bob dm inbox)"
bob dm inbox | grep "$(owner whoami | head -1)" > /dev/null || fail "the message does not come from the owner"
bob dm send "$owner_key" hello agent 2> /dev/null
agent dm inbox > "$work/shut.txt" 2> "$work/shut.err"
grep "hello agent" "$work/shut.txt" > /dev/null && fail "the bunker opened mail by default"
grep "decrypts only what its owner sealed" "$work/shut.err" > /dev/null || fail "the agent was not told why: $(cat "$work/shut.err")"
say "by default the bunker opens the owner's own drafts, and not their mail"

kill "$bunker_pid"
wait "$bunker_pid" 2> /dev/null || true
"$work/arcn" --home "$work/owner" keys bunker --relay "$url" --decrypt all --allow-kind 13 > "$work/bunker2.log" 2>&1 &
bunker_pid=$!
for _ in $(seq 1 50); do grep "bunker://" "$work/bunker2.log" > /dev/null 2>&1 && break; sleep 0.1; done
[ "$(grep "^bunker://" "$work/bunker2.log")" = "$uri" ] || fail "the bunker URI changed on restart"
agent dm inbox | grep "hello agent" > /dev/null || fail "the agent's inbox: $(agent dm inbox 2>&1)"
say "with --decrypt all, the agent reads its owner's mail, and the URI stays the same"

printf 'phase D holds: reserved kinds, consent, dry runs, sealed keys, and remote signers\n'
