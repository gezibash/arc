#!/bin/bash
# The proof of phase 1 of docs/delivery/SPEC.md. Two homes stand in for two
# machines of one citizen: they hold the same key and separate stores.
#
#     mise run delivery
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
keep="${ARC_KEEP_WORK:-}"

cleanup() {
  [ -n "${relay_pid:-}" ] && kill "$relay_pid" 2>/dev/null || true
  [ -n "${tail_pid:-}" ] && kill "$tail_pid" 2>/dev/null || true
  [ -n "$keep" ] && printf 'work: %s\n' "$work" || rm -rf "$work"
}
trap cleanup EXIT

say() { printf 'ok   %s\n' "$1"; }
fail() { printf 'FAIL %s\n' "$1"; exit 1; }

cd "$root"
go build -o "$work/arcn" ./cmd/arcn
say "arcn builds"

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
b journal tail hrs/log/live > "$work/tail.txt" 2>&1 &
tail_pid=$!
sleep 0.5
a journal append hrs/log/live first note > /dev/null
a journal append hrs/log/live second note > /dev/null
for _ in $(seq 1 50); do grep -q "second note" "$work/tail.txt" 2>/dev/null && break; sleep 0.1; done
[ "$(cat "$work/tail.txt")" = "$(printf 'first note\nsecond note')" ] || fail "tail wrote $(cat "$work/tail.txt")"
say "tail streams each note as it is appended"

printf '\nphase 1 holds: relay, USB stick, refusal, parts, and tail\n'
