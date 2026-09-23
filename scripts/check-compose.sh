#!/bin/bash
# The proof of HTTP over ARC, and of a provider that calls a provider. One
# notes service runs twice: in the provider program, served by provider.HTTP,
# and as a plain HTTP server with no ARC library, served by http-provider.
# Each keeps its notes in SQLite over ARC. Only the notes services hold a
# grant on the databases.
#
#     mise run compose
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
keep="${ARC_KEEP_WORK:-}"

cleanup() {
  for pid in "${relay_pid:-}" "${sqlite_pid:-}" "${notes_pid:-}" "${server_pid:-}"; do
    [ -n "$pid" ] && kill "$pid" 2>/dev/null || true
  done
  # A process started from the work directory must not outlive it.
  pkill -f "$work/" 2>/dev/null || true
  sleep 0.2
  pkill -9 -f "$work/" 2>/dev/null || true
  [ -n "$keep" ] && printf 'work: %s\n' "$work" || rm -rf "$work"
}
trap cleanup EXIT

say() { printf 'ok   %s\n' "$1"; }
fail() { printf 'FAIL %s\n' "$1"; exit 1; }
as() { local who="$1"; shift; "$work/arc" --home "$work/$who" "$@"; }
key() { as "$1" whoami | sed -n 2p; }

# serves waits until a provider says that it serves its capability.
serves() {
  for _ in $(seq 1 100); do grep "serves" "$work/$1.log" > /dev/null 2>&1 && break; sleep 0.1; done
  grep "serves $2" "$work/$1.log" > /dev/null || fail "$1 did not serve $2: $(cat "$work/$1.log")"
}

cd "$root"
go build -o "$work/arc" ./cmd/arc
go build -o "$work/sqlite-provider" ./cmd/sqlite-provider
go build -o "$work/http-provider" ./cmd/http-provider
go build -o "$work/notes-example" ./examples/notes
go build -o "$work/notes-server" ./examples/notes-server
say "arc, the sqlite and http providers, and the two notes examples build"

"$work/arc" --home "$work/relay" relay serve --listen 127.0.0.1:0 > "$work/relay.log" 2>&1 &
relay_pid=$!
for _ in $(seq 1 50); do grep "listens on" "$work/relay.log" > /dev/null 2>&1 && break; sleep 0.1; done
url="$(sed -n 's/^relay listens on //p' "$work/relay.log")"
[ -n "$url" ] || fail "the relay did not start: $(cat "$work/relay.log")"
say "a relay listens on $url"

for who in sqlite notes server alice bob; do
  as "$who" keys gen > /dev/null
  as "$who" relay add "$url"
done
sqlite_key="$(key sqlite)"
notes_key="$(key notes)"
server_key="$(key server)"

# Each notes service has its own database, and no caller of notes has a grant.
cat > "$work/sqlite.json" <<JSON
{"databases": {"main": {"path": "$work/main.db", "grants": {"$notes_key": "write"}},
               "plain": {"path": "$work/plain.db", "grants": {"$server_key": "write"}}}}
JSON
SQLITE_CONFIG="$work/sqlite.json" as sqlite serve \
  "exec://$work/sqlite-provider?manifest=$root/cmd/sqlite-provider/manifest.json" > "$work/sqlite.log" 2>&1 &
sqlite_pid=$!
serves sqlite sqlite

for service in notes server; do
  as "$service" install "$sqlite_key" --yes > /dev/null
done
NOTES_DB="sqlite+arc://$sqlite_key/main" as notes serve \
  "exec://$work/notes-example?manifest=$root/examples/notes/manifest.json" > "$work/notes.log" 2>&1 &
notes_pid=$!
NOTES_DB="sqlite+arc://$sqlite_key/plain" as server serve \
  "exec://$work/http-provider?manifest=$root/examples/notes/manifest.json&args=$work/notes-server" > "$work/server.log" 2>&1 &
server_pid=$!
serves notes http
serves server http
say "sqlite serves, and both notes services serve http with sqlite installed"

# prove runs the same checks against one notes service: its installed name,
# its key, and a label.
prove() {
  local name="$1" provider="$2" label="$3" out status
  for who in alice bob; do
    as "$who" install "$provider" --as "$name" --yes > /dev/null
  done

  out="$(as alice "$name" add hello from alice)" || fail "$label: add failed: $out"
  case "$out" in 201\ *) ;; *) fail "$label: add gave $out, want 201" ;; esac
  say "$label: alice keeps a note with an installed command: $out"

  out="$(as alice call --method POST "http+arc://$provider/notes" '{"body":"second note"}' 2> "$work/rtt.txt")" ||
    fail "$label: the call by address failed: $out $(cat "$work/rtt.txt")"
  case "$out" in 201\ *) ;; *) fail "$label: the call by address gave $out, want 201" ;; esac
  say "$label: alice keeps a note with arc call http+arc://<notes>/notes ($(cat "$work/rtt.txt"))"

  out="$(as alice "$name" list)" || fail "$label: list failed: $out"
  [ "$out" = '200 [{"body":"hello from alice","id":1},{"body":"second note","id":2}]' ] ||
    fail "$label: alice's list is $out"
  say "$label: alice reads her two notes back: $out"

  out="$(as bob "$name" list)" || fail "$label: bob's list failed: $out"
  [ "$out" = "200 []" ] || fail "$label: bob's list is $out, want 200 []"
  say "$label: bob sees none of alice's notes: the key of the caller is the login"

  set +e
  out="$(as alice call --method POST "http+arc://$provider/notes" '{}' 2> /dev/null)"
  status=$?
  set -e
  [ "$status" = 22 ] || fail "$label: an empty note exited $status, want 22"
  case "$out" in 400\ *) ;; *) fail "$label: an empty note gave $out, want 400" ;; esac
  say "$label: an empty note is 400, and arc exits 22, as curl --fail does"
}

prove notes "$notes_key" "provider.HTTP"
prove webnotes "$server_key" "http-provider"

as bob install "$sqlite_key" --yes > /dev/null
if out="$(as bob call "sqlite+arc://$sqlite_key/main" '{"sql":"select body from notes"}' 2>&1)"; then
  fail "bob read the database of notes directly: $out"
fi
case "$out" in *"refused: unauthorized"*) ;; *) fail "bob's query failed for another reason: $out" ;; esac
say "bob cannot read the database: the provider refuses him, because only notes holds a grant"

echo "HTTP over ARC works in process and behind http-provider, and each notes service keeps its state in SQLite over ARC"
