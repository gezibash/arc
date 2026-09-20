#!/bin/bash
# Runs the whole of ARC in Go: the relay, the citizen, the provider and the
# caller. Nothing here needs Elixir.
#
#     mise run go.cli
set -euo pipefail

# The test stands on its own: the relay of the machine, the key of the shell
# and the pin of the shell must not reach it.
unset ARC_RELAY ARC_RELAY_PUBKEY ARC_KEY

root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
export ARC_STORE="$work"

keep="${ARC_KEEP_WORK:-}"
cleanup() {
  [ -n "${relay_pid:-}" ] && kill "$relay_pid" 2>/dev/null || true
  [ -n "${serve_pid:-}" ] && kill "$serve_pid" 2>/dev/null || true
  [ -n "$keep" ] && printf 'work: %s\n' "$work" || rm -rf "$work"
}
trap cleanup EXIT

say() { printf 'ok   %s\n' "$1"; }
fail() { printf 'FAIL %s\n' "$1"; exit 1; }

cd "$root/go"
go build -o "$work/arc" ./cmd/arc
go build -o "$work/arc-relay" ./cmd/arc-relay
go build -o "$work/exec-provider" ./cmd/exec-provider
go build -o "$work/echo-provider" ./citizen/testdata/echo
say "the binaries build"

arc() { "$work/arc" --store "$work" "$@"; }

# The relay runs on a free port, and prints its key.
"$work/arc-relay" --address 127.0.0.1:0 --store "$work" --key relay --generate > "$work/relay.log" 2>"$work/relay.err" &
relay_pid=$!

for _ in $(seq 1 50); do
  [ -s "$work/relay.log" ] && break
  sleep 0.1
done

read -r _ _ relay_key _ relay_address < "$work/relay.log"
[ -n "$relay_address" ] || fail "the relay did not start: $(cat "$work/relay.err")"
say "the relay listens on $relay_address"

arc keys gen > "$work/citizen.txt"
provider_name="$(head -1 "$work/citizen.txt")"
provider_key="$(tail -1 "$work/citizen.txt")"
say "the citizen $provider_name has a key"

arc keys gen > "$work/caller.txt"
caller_name="$(head -1 "$work/caller.txt")"
caller_key="$(tail -1 "$work/caller.txt")"

arc keys gen > "$work/stranger.txt"
stranger_name="$(head -1 "$work/stranger.txt")"

arc keys gen > "$work/echo.txt"
echo_name="$(head -1 "$work/echo.txt")"
echo_key="$(tail -1 "$work/echo.txt")"

arc keys use "$provider_name" > /dev/null
say "the machine holds four identities"

arc join "$relay_address" --pubkey "$relay_key" > /dev/null
say "the citizen joined the relay and pinned its key"

arc status | grep -q "state    running" || fail "the relay does not report that it runs"
say "arc status reads the relay"

# The provider grants the caller, and nobody else.
cat > "$work/exec.json" <<JSON
{"grants": ["$caller_key"], "cwd": "$work", "jobs_dir": "$work/jobs"}
JSON
mkdir -p "$work/jobs"

EXEC_CONFIG="$work/exec.json" arc serve \
  "exec://$work/exec-provider?manifest=$root/providers/exec/manifest.json" \
  > "$work/serve.log" 2>"$work/serve.err" &
serve_pid=$!

for _ in $(seq 1 50); do
  grep -q "serves on" "$work/serve.log" 2>/dev/null && break
  sleep 0.1
done
grep -q "serves on" "$work/serve.log" || fail "the citizen did not serve: $(cat "$work/serve.err")"
say "the citizen serves the exec provider"

# Every command below runs as another citizen on the same relay.
caller() { arc --key "$caller_name" "$@"; }

for _ in $(seq 1 50); do
  caller resolve "$provider_key" | grep -q "$provider_key" && break
  sleep 0.2
done
caller resolve "$provider_key" | grep -q "exec+arc://$provider_key" || fail "the directory does not hold the citizen"
say "arc resolve finds the citizen and its capability"

caller discover "command" | grep -q "$provider_key" || fail "the search found nothing"
say "arc discover finds the capability"

caller call "exec+arc://$provider_key/" --manifest | grep -q '"scheme": "exec"' || fail "the manifest is wrong"
say "arc call --manifest reads the signed capability"

caller call "exec+arc://$provider_key/" '{"argv":["echo","hello from go"]}' > "$work/reply.json"
grep -q "hello from go" "$work/reply.json" || fail "the command did not run: $(cat "$work/reply.json")"
say "arc call runs a command through the provider"

caller call "exec+arc://$provider_key/" '{"action":"start","script":"echo job output"}' > "$work/job.json"
job="$(sed -n 's/.*"job":"\([^"]*\)".*/\1/p' "$work/job.json")"
[ -n "$job" ] || fail "the job did not start: $(cat "$work/job.json")"

for _ in $(seq 1 50); do
  caller call "exec+arc://$provider_key/" "{\"action\":\"status\",\"job\":\"$job\"}" > "$work/status.json"
  grep -q '"state":"done"' "$work/status.json" && break
  sleep 0.2
done
grep -q "job output" "$work/status.json" || fail "the job has no output: $(cat "$work/status.json")"
say "a job runs, and its result comes back"

# The provider grants the caller only, so a third citizen is refused.
if arc --key "$stranger_name" call "exec+arc://$provider_key/" '{"argv":["echo","hello"]}' 2>"$work/denied.txt"; then
  fail "a citizen without a grant ran a command"
fi
grep -q "access_denied" "$work/denied.txt" || fail "the refusal is not access_denied: $(cat "$work/denied.txt")"
say "a citizen without a grant is refused"

# A capability with a command line becomes a command of arc.
caller keys gen > /dev/null 2>&1 || true

EXEC_CONFIG="$work/exec.json" arc --key "$echo_name" serve \
  "exec://$work/echo-provider?manifest=$root/go/citizen/testdata/echo/cli-manifest.json" \
  > "$work/echo.log" 2>"$work/echo.err" &
echo_pid=$!

for _ in $(seq 1 50); do
  grep -q "serves on" "$work/echo.log" 2>/dev/null && break
  sleep 0.1
done
grep -q "serves on" "$work/echo.log" || fail "the echo citizen did not serve: $(cat "$work/echo.err")"
say "a second citizen serves a capability with a command line"

caller install "$echo_key" --yes > "$work/install.txt" 2>&1 ||
  fail "the install failed: $(cat "$work/install.txt")"
grep -q "arc echo" "$work/install.txt" || fail "the install does not name the command: $(cat "$work/install.txt")"
say "arc install saves the signed capability as a command"

caller tool list | grep -q "echo ->" || fail "the command is not listed"
say "arc tool list names it"

caller echo hello world > "$work/echo-reply.txt" 2>&1 ||
  fail "the command failed: $(cat "$work/echo-reply.txt")"
grep -q "ECHO / hello world" "$work/echo-reply.txt" ||
  fail "the command answered $(cat "$work/echo-reply.txt")"
say "arc echo runs the capability of the other citizen"

caller echo twice --from ada "the words" > "$work/echo-twice.txt" 2>&1 ||
  fail "the subcommand failed: $(cat "$work/echo-twice.txt")"
grep -q "ada says the words" "$work/echo-twice.txt" ||
  fail "the subcommand answered $(cat "$work/echo-twice.txt")"
say "a subcommand renders its template"

caller trust list | grep -q allowed || fail "the signer is not trusted"
caller tool remove echo | grep -q "removed echo" || fail "the command was not removed"
say "arc trust list and arc tool remove answer"

kill "$echo_pid" 2>/dev/null || true

# One citizen listens, and another sends it a message. A third identity
# takes this, because one identity holds one route at a time.
stranger_key="$(arc --key "$stranger_name" whoami | sed -n 2p)"
arc --key "$stranger_name" listen > "$work/listen.log" 2>&1 &
listen_pid=$!

for _ in $(seq 1 50); do
  grep -q "listens on" "$work/listen.log" 2>/dev/null && break
  sleep 0.1
done
grep -q "listens on" "$work/listen.log" || fail "the citizen did not listen: $(cat "$work/listen.log")"

caller send "$stranger_key" "a message from the other citizen" | grep -q "sent to" ||
  fail "the message was not sent"

for _ in $(seq 1 50); do
  grep -q "a message from the other citizen" "$work/listen.log" && break
  sleep 0.1
done
grep -q "a message from the other citizen" "$work/listen.log" ||
  fail "the message did not arrive: $(cat "$work/listen.log")"
say "arc send and arc listen carry a message between two citizens"

kill "$listen_pid" 2>/dev/null || true

caller info "$provider_key" | grep -q "exec+arc://$provider_key" || fail "arc info is wrong"
say "arc info reads the signed capability of a citizen"

arc --key "$stranger_name" publish > /dev/null || fail "the identity did not publish"
caller resolve "$stranger_name" | grep -q "on this machine" ||
  fail "the identity of this machine does not resolve"
say "arc publish and arc resolve answer without the relay"

caller lists add dm friends "$provider_key" | grep -q "$provider_key" || fail "the list was not saved"
caller lists ls dm | grep -q friends || fail "the list is not shown"
caller lists rm dm friends | grep -q "removed dm/friends" || fail "the list was not removed"
say "arc lists keeps a set of peers"

printf '\nARC runs end to end in Go\n'
