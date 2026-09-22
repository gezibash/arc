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
# keyfile names the key file of the one identity of a home.
keyfile() { echo "$work/$1"/citizens/*/key; }

cd "$root"
go build -o "$work/arcn" ./cmd/arcn
go build -o "$work/exec-provider" ./cmd/exec-provider
say "arcn and the exec provider build"

a() { "$work/arcn" --home "$work/laptop" "$@"; }
b() { "$work/arcn" --home "$work/desktop" "$@"; }

# The relay keeps the limits of the public deploy that bear on this proof:
# the size of an event, and authentication before a gift wrap.
"$work/arcn" --home "$work/relay" relay serve --listen 127.0.0.1:0 \
  --max-event-bytes 262144 --wrap-auth > "$work/relay.log" 2>&1 &
relay_pid=$!
for _ in $(seq 1 50); do grep -q "listens on" "$work/relay.log" 2>/dev/null && break; sleep 0.1; done
url="$(sed -n 's/^relay listens on //p' "$work/relay.log")"
[ -n "$url" ] || fail "the relay did not start: $(cat "$work/relay.log")"
say "a relay listens on $url"

a keys gen > "$work/key.txt"
b keys add < "$(keyfile laptop)" > /dev/null
[ "$(a whoami | head -2)" = "$(b whoami | head -2)" ] || fail "the two machines hold different keys"
say "two machines hold one key, and separate stores"

a relay add "$url"
b relay add "$url"

# The journal is a manifest of interface version 1. Its author announces it,
# and each machine installs it by trusting that author.
a announce "$root/manifests/journal.json" > /dev/null
author="$(a whoami | sed -n 2p)"
a install "$author" journal --yes > /dev/null
b install "$author" journal --yes > /dev/null
say "both machines install the journal"

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
  if grep '"kind":31234' "$f" > /dev/null; then
    sed -i.bak 's/"content":"\([^"]\)/"content":"X\1/' "$f"
    rm -f "$f.bak"
    changed=1
  fi
done
[ "$changed" = 1 ] || fail "found no draft of the new page to change"
b sync --dir "$work/stick2" > /dev/null 2> "$work/refused.txt" || true
grep -q "refused" "$work/refused.txt" || fail "the changed event was not refused: $(cat "$work/refused.txt")"
if b journal read hrs/ablations/tamper 2> /dev/null | grep "the real text" > /dev/null; then
  fail "the desktop read a changed page"
fi
say "a changed event is refused, and the page does not read"

# A large page streams in parts, and a range reads only its parts.
a relay add "$url"
b relay add "$url"
for i in $(seq 1 3000); do printf 'line %05d %090d\n' "$i" 0; done > "$work/big.txt"
a journal write hrs/data/big < "$work/big.txt" || fail "the large page was not written"
[ "$(b journal read hrs/data/big | wc -l | tr -d ' ')" = 3000 ] || fail "the large page did not read back whole"
b journal read hrs/data/big > "$work/big-back.txt"
cmp -s "$work/big.txt" "$work/big-back.txt" || fail "the large page came back changed"
say "a large page of $(wc -c < "$work/big.txt" | tr -d ' ') bytes crosses the relay in parts"

"$work/arcn" --home "$work/phone" keys add < "$(keyfile laptop)" > /dev/null
"$work/arcn" --home "$work/phone" relay add "$url"
"$work/arcn" --home "$work/phone" install "$author" journal --yes > /dev/null
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

alice keys gen > /dev/null
carol keys gen > /dev/null
bob keys gen > "$work/bob.txt"
bob_key="$(tail -1 "$work/bob.txt")"
alice_key="$(alice whoami | sed -n 2p)"
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
bob message inbox | grep "$(alice whoami | head -1)" > /dev/null || fail "the message does not name alice"
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

provider keys gen > /dev/null
provider relay add "$url"
provider_key="$(provider whoami | sed -n 2p)"
provider_name="$(provider whoami | head -1)"
caller keys gen > /dev/null
caller relay add "$url"
caller_key="$(caller whoami | sed -n 2p)"

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

"$work/arcn" --home "$work/stranger" keys gen > /dev/null
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

# A provider whose machine paused. The caller keeps a wake hook for it, and
# arcn runs the hook before the live call, the way ssh runs a ProxyCommand.
kill "$serve_pid"
wait "$serve_pid" 2>/dev/null || true
caller relay add "$url"

cat > "$work/wake-exec" <<SCRIPT
#!/bin/sh
# The start script of the provider: serve again, and exit 0 when it listens.
echo woke >> "$work/woke"
EXEC_CONFIG="$work/exec.json" "$work/arcn" --home "$work/exec" serve \\
  "exec://$work/exec-provider?manifest=$root/cmd/exec-provider/manifest.json" \\
  > "$work/serve-woken.log" 2>&1 < /dev/null &
echo \$! > "$work/serve-woken.pid"
for _ in \$(seq 1 50); do
  grep -q "serves" "$work/serve-woken.log" 2>/dev/null && exit 0
  sleep 0.1
done
exit 1
SCRIPT
chmod +x "$work/wake-exec"
cat > "$work/caller/wake.toml" <<TOML
[wake."$provider_key"]
kind = "command"
argv = ["$work/wake-exec"]
TOML

caller call "$provider_name" '{"argv":["echo","woken"]}' > "$work/woken.txt" 2> "$work/woken.err" ||
  fail "the call did not wake the provider: $(cat "$work/woken.err")"
serve_pid="$(cat "$work/serve-woken.pid")"
grep "woken" "$work/woken.txt" > /dev/null || fail "the woken provider answered $(cat "$work/woken.txt")"
[ "$(wc -l < "$work/woke" | tr -d ' ')" = 1 ] || fail "the hook ran $(wc -l < "$work/woke") times"
say "arcn runs the wake hook, and the woken provider answers"

caller call "$provider_name" '{"argv":["echo","again"]}' > /dev/null 2>&1 || fail "the second call failed"
[ "$(wc -l < "$work/woke" | tr -d ' ')" = 1 ] || fail "the hook ran again for a provider that answered a moment ago"
say "arcn skips the hook of a provider that answered a moment ago"

go test -count=1 -run 'NIP17' ./delivery/mail/ > "$work/nip17.txt" 2>&1 || fail "NIP-17: $(cat "$work/nip17.txt")"
say "an ARC direct message opens in a NIP-17 client, and a NIP-17 message opens in ARC"

# Updates: a publisher signs a channel with a Nostr key, a releases provider
# serves it, and an older arcn replaces itself with the newer build.
publisher() { "$work/arcn" --home "$work/publisher" "$@"; }
publisher keys gen > /dev/null
publisher_key="$(publisher whoami | sed -n 2p)"
releases="$work/releases"
mkdir -p "$releases/channels" "$releases/blobs" "$work/new/arc/bin" "$work/old"
go build -ldflags "-X main.version=0.9.0" -o "$work/old/arcn" ./cmd/arcn
go build -ldflags "-X main.version=9.9.9" -o "$work/new/arc/bin/arcn" ./cmd/arcn
go build -o "$work/releases-provider" ./cmd/releases-provider
tar -czf "$work/new.tar.gz" -C "$work/new" arc
digest="$(shasum -a 256 "$work/new.tar.gz" | cut -d' ' -f1)"
size="$(wc -c < "$work/new.tar.gz" | tr -d ' ')"
cp "$work/new.tar.gz" "$releases/blobs/$digest.tar.gz"
cat > "$work/unsigned.json" <<JSON
{"schema_version": 3, "channel": "stable", "publisher": "$publisher_key", "sequence": 1,
 "expires_at": $(( $(date +%s) + 86400 )),
 "releases": [{"version": "9.9.9", "build": "9.9.9+test", "runtime": "go",
   "platform": {"os": "$(go env GOOS)", "arch": "$(go env GOARCH)"},
   "size": $size, "sha256": "$digest", "sources": [], "restart_required": true,
   "withdrawn": false, "eligible": true, "install": {"size": $size, "sha256": "$digest"}}]}
JSON
publisher release sign --root "$releases" "$work/unsigned.json" > /dev/null || fail "the channel was not signed"
if publisher release sign --root "$releases" "$work/unsigned.json" > /dev/null 2>&1; then
  fail "the same sequence was signed twice"
fi
say "a publisher signs a channel with a Nostr key, and never the same sequence twice"

"$work/arcn" --home "$work/rel" keys gen > /dev/null
"$work/arcn" --home "$work/rel" relay add "$url"
rel_key="$("$work/arcn" --home "$work/rel" whoami | sed -n 2p)"
RELEASES_ROOT="$releases" "$work/arcn" --home "$work/rel" serve \
  "exec://$work/releases-provider?manifest=$root/cmd/releases-provider/manifest.json" > "$work/rel.log" 2>&1 &
rel_pid=$!
for _ in $(seq 1 50); do grep "serves" "$work/rel.log" > /dev/null 2>&1 && break; sleep 0.1; done
grep "serves" "$work/rel.log" > /dev/null || fail "the releases provider did not serve: $(cat "$work/rel.log")"

old() { "$work/old/arcn" --home "$work/caller" "$@"; }
old update check --provider "$rel_key" --publisher "$publisher_key" | grep "names arcn 9.9.9" > /dev/null ||
  fail "update check: $(old update check --provider "$rel_key" --publisher "$publisher_key" 2>&1)"
old update apply --provider "$rel_key" --publisher "$publisher_key" > "$work/apply.txt" 2>&1 ||
  fail "update apply: $(cat "$work/apply.txt")"
"$work/old/arcn" --version | grep "9.9.9" > /dev/null || fail "the program is $("$work/old/arcn" --version)"
"$work/old/arcn.previous" --version | grep "0.9.0" > /dev/null || fail "the previous program is gone"
say "an older arcn reads the channel over the relay, and replaces itself"

if old update check --provider "$rel_key" --publisher "$caller_key" > /dev/null 2> "$work/wrongpub.txt"; then
  fail "a channel of another publisher passed"
fi
grep "another publisher" "$work/wrongpub.txt" > /dev/null || fail "the refusal was $(cat "$work/wrongpub.txt")"
say "a channel that another key signed is refused"
kill "$rel_pid" 2>/dev/null || true

printf 'phase 3 holds: announcements, install, live and carried calls, and NIP-17\n'
