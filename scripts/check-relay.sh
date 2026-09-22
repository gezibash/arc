#!/bin/bash
# The proof of step 2 of the switchover, docs/delivery/SPEC.md section 15.1.
# It checks one running relay: a sealed page crosses it between two machines
# of one citizen, which needs NIP-42 authentication, and a live call to exec
# crosses it.
#
#     mise run check-relay -- wss://arc-nostr-gezim.fly.dev
set -euo pipefail

url="${1:?give the URL of the relay, for example wss://arc-nostr-gezim.fly.dev}"
root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"

cleanup() {
  [ -n "${serve_pid:-}" ] && kill "$serve_pid" 2>/dev/null || true
  pkill -f "$work/" 2>/dev/null || true
  rm -rf "$work"
}
trap cleanup EXIT

say() { printf 'ok   %s\n' "$1"; }
fail() { printf 'FAIL %s\n' "$1"; exit 1; }
# keyfile names the key file of the one identity of a home.
keyfile() { echo "$work/$1"/citizens/*/key; }

(cd "$root" && go build -o "$work/arc" ./cmd/arc && go build -o "$work/exec-provider" ./cmd/exec-provider)
a() { "$work/arc" --home "$work/laptop" "$@"; }
b() { "$work/arc" --home "$work/desktop" "$@"; }

a keys gen > /dev/null
b keys add < "$(keyfile laptop)" > /dev/null
a relay add "$url"
b relay add "$url"
say "two machines of one citizen use $url"

a announce "$root/manifests/journal.json" > /dev/null || fail "the relay did not take the announcement"
author="$(a whoami | sed -n 2p)"
a install "$author" journal --yes > /dev/null
b install "$author" journal --yes > /dev/null
page="check/relay/$(date +%s)"
printf 'sealed through %s\n' "$url" | a journal write "$page" > /dev/null || fail "the page was not written"
[ "$(b journal read "$page")" = "sealed through $url" ] || fail "the other machine read $(b journal read "$page" 2>&1)"
say "a sealed page crosses the relay, after NIP-42 authentication"

provider() { "$work/arc" --home "$work/exec" "$@"; }
caller() { "$work/arc" --home "$work/caller" "$@"; }
provider keys gen > /dev/null
provider relay add "$url"
caller keys gen > /dev/null
caller relay add "$url"
provider_key="$(provider whoami | sed -n 2p)"
caller_key="$(caller whoami | sed -n 2p)"

mkdir -p "$work/jobs"
printf '{"grants": ["%s"], "cwd": "%s", "jobs_dir": "%s/jobs"}\n' "$caller_key" "$work" "$work" > "$work/exec.json"
EXEC_CONFIG="$work/exec.json" "$work/arc" --home "$work/exec" serve \
  "exec://$work/exec-provider?manifest=$root/cmd/exec-provider/manifest.json" > "$work/serve.log" 2>&1 &
serve_pid=$!
for _ in $(seq 1 100); do grep "serves" "$work/serve.log" > /dev/null 2>&1 && break; sleep 0.1; done
grep "serves" "$work/serve.log" > /dev/null || fail "the provider did not serve: $(cat "$work/serve.log")"

caller install "$provider_key" --yes > /dev/null || fail "the caller did not install exec"
sleep 1
caller call "$provider_key" '{"argv":["echo","hello live"]}' > "$work/live.txt" 2> "$work/live.err" ||
  fail "the live call failed: $(cat "$work/live.err")"
grep "hello live" "$work/live.txt" > /dev/null || fail "the live call answered $(cat "$work/live.txt")"
say "a live call to exec crosses the relay; $(grep -o 'round trip.*' "$work/live.err")"

printf 'the relay at %s holds\n' "$url"
