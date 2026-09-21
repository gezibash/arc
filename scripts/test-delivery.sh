#!/bin/bash
# The proofs of phases 1, 2 and 3 of docs/delivery/SPEC.md. In phase 1, two homes
# stand in for two machines of one citizen: they hold the same key and
# separate stores. In phase 2, three citizens hold three keys.
#
#     mise run delivery
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
keep="${ARC_KEEP_WORK:-}"

cleanup() {
  [ -n "${relay_pid:-}" ] && kill "$relay_pid" 2>/dev/null || true
  [ -n "${tail_pid:-}" ] && kill "$tail_pid" 2>/dev/null || true
  [ -n "${serve_pid:-}" ] && kill "$serve_pid" 2>/dev/null || true
  # A process started from the work directory must not outlive it. A shell
  # function started with & gives $! as a subshell, and kill then misses the
  # real process, so stop everything that runs from the work directory.
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
say "arcn and the exec provider build"

a() { "$work/arcn" --home "$work/laptop" "$@"; }
b() { "$work/arcn" --home "$work/desktop" "$@"; }

"$work/arcn" --home "$work/relay" relay serve --listen 127.0.0.1:0 > "$work/relay.log" 2>&1 &
relay_pid=$!
for _ in $(seq 1 50); do grep -q "listens on" "$work/relay.log" 2>/dev/null && break; sleep 0.1; done
url="$(sed -n 's/^relay listens on //p' "$work/relay.log")"
[ -n "$url" ] || fail "the relay did not start: $(cat "$work/relay.log")"
say "a relay listens on $url"

a key new > "$work/key.txt"
mkdir -p "$work/desktop"
cp "$work/laptop/key" "$work/desktop/key"
[ "$(a key show)" = "$(b key show)" ] || fail "the two machines hold different keys"
say "two machines hold one key, and separate stores"

a relay add "$url"
b relay add "$url"

# Through a relay.
printf 'auc 0.871\nnext: try warmup\n' | a journal write hrs/ablations/lr-sweep --title "LR sweep" > /dev/null
b sync > /dev/null
[ "$(b journal read hrs/ablations/lr-sweep)" = "$(printf 'auc 0.871\nnext: try warmup')" ] ||
  fail "the desktop read $(b journal read hrs/ablations/lr-sweep)"
say "a page crosses the relay"

# The relay holds ciphertext only.
grep -a -q "try warmup" "$work/relay/relay.db" && fail "the relay holds the page text"
grep -a -q "lr-sweep" "$work/relay/relay.db" && fail "the relay holds the page address"
say "the relay holds neither the text nor the address"

# Through a USB stick, with no relay.
a relay rm "$url"
b relay rm "$url"
printf 'the stick carried this\n' | a journal write hrs/ablations/offline > /dev/null
a sync --dir "$work/stick" > /dev/null
b sync --dir "$work/stick" > /dev/null
[ "$(b journal read hrs/ablations/offline)" = "the stick carried this" ] ||
  fail "the desktop did not read the page from the stick"
say "a page crosses a USB stick"

# A changed event is refused. The stick first gets every earlier event, so
# the files that appear after the next write belong to the new page alone.
a sync --dir "$work/stick2" > /dev/null
ls "$work/stick2/events" > "$work/before.txt"
printf 'the real text\n' | a journal write hrs/ablations/tamper > /dev/null
a sync --dir "$work/stick2" > /dev/null
changed=0
for name in $(ls "$work/stick2/events" | grep -vxF -f "$work/before.txt"); do
  f="$work/stick2/events/$name"
  if grep '"kind":3275' "$f" > /dev/null; then
    sed -i.bak 's/"content":"\([^"]\)/"content":"X\1/' "$f"
    rm -f "$f.bak"
    changed=1
  fi
done
[ "$changed" = 1 ] || fail "found no part of the new page to change"
b sync --dir "$work/stick2" > /dev/null 2> "$work/refused.txt" || true
grep -q "refused" "$work/refused.txt" || fail "the changed event was not refused: $(cat "$work/refused.txt")"
if b journal read hrs/ablations/tamper > /dev/null 2>&1; then
  fail "the desktop read a page with a changed part"
fi
say "a changed event is refused, and the page does not read"

# A large page streams in parts, and a range reads only its parts.
a relay add "$url"
b relay add "$url"
for i in $(seq 1 3000); do printf 'line %05d %090d\n' "$i" 0; done > "$work/big.txt"
a journal write hrs/data/big < "$work/big.txt" | grep "parts" > /dev/null || fail "the large page was not written"
[ "$(b journal read hrs/data/big | wc -l | tr -d ' ')" = 3000 ] || fail "the large page did not read back whole"
b journal read hrs/data/big > "$work/big-back.txt"
cmp -s "$work/big.txt" "$work/big-back.txt" || fail "the large page came back changed"
say "a large page of $(wc -c < "$work/big.txt" | tr -d ' ') bytes crosses the relay in parts"

"$work/arcn" --home "$work/phone" key new > /dev/null
rm "$work/phone/key"
cp "$work/laptop/key" "$work/phone/key"
"$work/arcn" --home "$work/phone" relay add "$url"
got="$("$work/arcn" --home "$work/phone" journal read hrs/data/big --lines 1500:1501)"
[ "$got" = "$(sed -n '1500,1501p' "$work/big.txt")" ] || fail "the range read gave $got"
say "a range of two lines reads on a machine that held nothing"

# Tail streams what is appended.
"$work/arcn" --home "$work/desktop" journal tail hrs/log/live > "$work/tail.txt" 2>&1 &
tail_pid=$!
sleep 0.5
a journal append hrs/log/live first note > /dev/null
a journal append hrs/log/live second note > /dev/null
for _ in $(seq 1 50); do grep -q "second note" "$work/tail.txt" 2>/dev/null && break; sleep 0.1; done
[ "$(cat "$work/tail.txt")" = "$(printf 'first note\nsecond note')" ] || fail "tail wrote $(cat "$work/tail.txt")"
say "tail streams each note as it is appended"

printf 'phase 1 holds: relay, USB stick, refusal, parts, and tail\n\n'

# Phase 2: a message reaches an offline recipient through a third machine
# that carries a USB stick. Nobody has a relay.
alice() { "$work/arcn" --home "$work/alice" "$@"; }
carol() { "$work/arcn" --home "$work/carol" "$@"; }
bob() { "$work/arcn" --home "$work/bob" "$@"; }

alice key new > /dev/null
carol key new > /dev/null
bob key new > "$work/bob.txt"
bob_key="$(tail -1 "$work/bob.txt")"
alice_key="$(alice key show | tail -1)"
say "three citizens hold three keys"

alice message send "$bob_key" "meet at the river at noon" | grep "queued" > /dev/null || fail "alice could not queue the message"
alice sync --dir "$work/stick-a" > /dev/null
carol sync --dir "$work/stick-a" | grep "carried 1" > /dev/null || fail "carol did not carry the message"
carol message inbox | grep "no messages" > /dev/null || fail "carol could read mail that was not hers"
carol sync --dir "$work/stick-b" > /dev/null
say "carol carries a sealed message that she cannot read"

grep -r -l -e "$bob_key" -e "$alice_key" -e "river" "$work/stick-a" "$work/stick-b" > /dev/null &&
  fail "a stick names a citizen or holds the text"
say "the sticks name neither citizen and hold no text"

bob sync --dir "$work/stick-b" | grep "mail received 1" > /dev/null || fail "bob did not receive the message"
bob message inbox | grep "meet at the river at noon" > /dev/null || fail "bob's inbox: $(bob message inbox)"
bob message inbox | grep "$(alice key show | head -1)" > /dev/null || fail "the message does not name alice"
say "bob receives it, and the seal proves that alice wrote it"

bob sync --dir "$work/stick-b" > /dev/null
carol sync --dir "$work/stick-b" > /dev/null
carol sync --dir "$work/stick-a" > /dev/null
alice sync --dir "$work/stick-a" | grep "delivered 1" > /dev/null || fail "the acknowledgement did not come back"
alice message outbox | grep "delivered" > /dev/null || fail "alice's outbox: $(alice message outbox)"
say "bob's acknowledgement comes back the same way, and clears alice's outbox"

# The same message over a relay.
alice relay add "$url"
bob relay add "$url"
alice message send "$bob_key" "and over the relay" > /dev/null
bob sync | grep "mail received 1" > /dev/null || fail "bob did not receive over the relay"
alice sync | grep "delivered 1" > /dev/null || fail "the acknowledgement did not cross the relay"
say "a message and its acknowledgement cross a relay"

printf 'phase 2 holds: couriers, route tags, acknowledgements, and the outbox\n\n'

# Phase 3: capabilities. A provider serves exec; a caller finds it, installs
# it, and calls it live over the relay and by hand through a courier.
provider() { "$work/arcn" --home "$work/exec" "$@"; }
caller() { "$work/arcn" --home "$work/caller" "$@"; }

provider key new > /dev/null
provider relay add "$url"
provider_key="$(provider key show | tail -1)"
provider_name="$(provider key show | head -1)"
caller key new > /dev/null
caller relay add "$url"
caller_key="$(caller key show | tail -1)"

mkdir -p "$work/jobs" "$work/stick-p"
cat > "$work/exec.json" <<JSON
{"grants": ["$caller_key"], "cwd": "$work", "jobs_dir": "$work/jobs"}
JSON

EXEC_CONFIG="$work/exec.json" "$work/arcn" --home "$work/exec" serve \
  "exec://$work/exec-provider?manifest=$root/cmd/exec-provider/manifest.json" \
  --sync-dir "$work/stick-p" --interval 1s > "$work/serve.log" 2>&1 &
serve_pid=$!
for _ in $(seq 1 50); do grep "serves" "$work/serve.log" > /dev/null 2>&1 && break; sleep 0.1; done
grep "serves" "$work/serve.log" > /dev/null || fail "the provider did not serve: $(cat "$work/serve.log")"
say "a provider serves exec through arcn"

caller discover exec | grep "$provider_key" > /dev/null || fail "discover did not find the provider"
caller install "$provider_key" --yes | grep "installed" > /dev/null || fail "the install failed"
say "the caller discovers the capability, and installs it"

sleep 0.5
caller call "$provider_name" '{"argv":["echo","hello live"]}' > "$work/live.txt" 2> "$work/live.err" ||
  fail "the live call failed: $(cat "$work/live.err")"
grep "hello live" "$work/live.txt" > /dev/null || fail "the live call answered $(cat "$work/live.txt")"
rtt="$(sed -n 's/^round trip \([^ ]*\) via.*/\1/p' "$work/live.err")"
[ -n "$rtt" ] || fail "the live call recorded no round trip"
say "a live call to exec crosses the relay; round trip $rtt"

"$work/arcn" --home "$work/stranger" key new > /dev/null
"$work/arcn" --home "$work/stranger" relay add "$url"
"$work/arcn" --home "$work/stranger" install "$provider_key" --yes > /dev/null
if "$work/arcn" --home "$work/stranger" call "$provider_name" '{"argv":["echo","x"]}' > /dev/null 2> "$work/denied.txt"; then
  fail "a caller without a grant ran a command"
fi
grep "access_denied" "$work/denied.txt" > /dev/null || fail "the refusal is not access_denied: $(cat "$work/denied.txt")"
say "a caller without a grant is refused"

# Store and forward: the caller has no relay now, and Carol carries the call.
caller relay rm "$url"
caller call "$provider_name" '{"argv":["echo","carried by hand"]}' | grep "queued" > /dev/null ||
  fail "the call was not queued"
caller sync --dir "$work/stick-c" > /dev/null
carol sync --dir "$work/stick-c" > /dev/null
carol sync --dir "$work/stick-p" > /dev/null
for _ in $(seq 1 30); do
  grep "answered store-and-forward calls" "$work/serve.log" > /dev/null && break
  sleep 0.2
done
grep "answered store-and-forward calls" "$work/serve.log" > /dev/null || fail "the provider did not answer the carried call"
sleep 1.5
carol sync --dir "$work/stick-p" > /dev/null
carol sync --dir "$work/stick-c" > /dev/null
caller sync --dir "$work/stick-c" > /dev/null
caller call results | grep "reply:.*carried by hand" > /dev/null || fail "the reply did not come back: $(caller call results)"
say "a store-and-forward call crosses the courier path, and its reply comes back"

go test -count=1 -run 'NIP17' ./delivery/mail/ > "$work/nip17.txt" 2>&1 || fail "NIP-17: $(cat "$work/nip17.txt")"
say "an ARC direct message opens in a NIP-17 client, and a NIP-17 message opens in ARC"

printf 'phase 3 holds: announcements, install, live and carried calls, and NIP-17\n'
