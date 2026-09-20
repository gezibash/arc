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
say "the three binaries build"

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

arc keys use "$provider_name" > /dev/null
say "the machine holds three identities"

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

printf '\nARC runs end to end in Go\n'
