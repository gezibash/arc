#!/bin/bash
# Runs the whole of ARC in Go: the relay, the citizen, the provider and the
# caller.
#
#     mise run cli
set -euo pipefail

# The test stands on its own: the relay of the machine, the key of the shell
# and the pin of the shell must not reach it.
unset ARC_RELAY ARC_RELAY_PUBKEY ARC_LEGACY_KEY

root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
export ARC_STORE="$work"

keep="${ARC_KEEP_WORK:-}"
cleanup() {
  for held in "${relay_pid:-}" "${serve_pid:-}" "${echo_pid:-}" "${dm_pid:-}" "${listen_pid:-}" "${app_pid:-}"; do
    [ -n "$held" ] && kill "$held" 2>/dev/null || true
  done
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
go build -o "$work/arc-legacy" ./cmd/arc-legacy
go build -o "$work/arc-relay" ./cmd/arc-relay
go build -o "$work/exec-provider" ./cmd/exec-provider
go build -o "$work/echo-provider" ./citizen/testdata/echo
go build -o "$work/dm-provider" ./cmd/dm-provider
say "the binaries build"
"$work/arc-legacy" version 2>&1 > /dev/null | grep "arc-legacy is deprecated" > /dev/null ||
  fail "arc-legacy wrote no deprecation notice"
say "arc-legacy says that it is deprecated"

arc() { "$work/arc-legacy" --store "$work" "$@"; }

# The relay runs on a free port, and prints its key.
# --generate makes the first key of the store, and the relay keeps it.
"$work/arc-relay" --address 127.0.0.1:0 --store "$work" --generate > "$work/relay.log" 2>"$work/relay.err" &
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

arc join "$relay_address" --relay-pubkey "$relay_key" > /dev/null
say "the citizen joined the relay and pinned its key"

# The relay may still be registering the join.
for _ in $(seq 1 25); do
  arc status | grep "state    running" > /dev/null && break
  sleep 0.2
done
arc status | grep "state    running" > /dev/null || fail "the relay does not report that it runs: $(arc status)"
say "arc status reads the relay"

# Status asks with a temporary identity, so it needs no key of its own.
mkdir -p "$work/empty"
"$work/arc-legacy" --store "$work/empty" status --relay "$relay_address" --relay-pubkey "$relay_key" |
  grep "state    running" > /dev/null || fail "arc status needs a key"
say "arc status needs no key"

# Join shows the key of a new relay, and pins it only after a yes. On a
# machine with no key, it makes the first one.
mkdir -p "$work/fresh"
if printf 'no\n' | "$work/arc-legacy" --store "$work/fresh" join "$relay_address" > "$work/join-no.txt" 2>&1; then
  fail "join pinned a relay that was not trusted"
fi
[ -e "$work/fresh/relays.json" ] && fail "a refused join saved the relay"
printf 'yes\n' | "$work/arc-legacy" --store "$work/fresh" join "$relay_address" > "$work/join-yes.txt" 2>&1 ||
  fail "join failed: $(cat "$work/join-yes.txt")"
grep -q "$relay_key" "$work/join-yes.txt" || fail "join did not show the key: $(cat "$work/join-yes.txt")"
grep -q "made the identity" "$work/join-yes.txt" || fail "join made no identity: $(cat "$work/join-yes.txt")"
"$work/arc-legacy" --store "$work/fresh" whoami | grep "chosen by default.key" > /dev/null ||
  fail "the new identity is not the default"
say "arc join asks before it pins a relay, and makes the first key"

if arc keys show > /dev/null 2>&1; then
  fail "arc keys show succeeded, and that command does not exist"
fi
arc --key "$caller_name" whoami | grep "chosen by --key" > /dev/null || fail "whoami does not name --key"
say "an unknown subcommand fails, and whoami names --key"

# The provider grants the caller, and nobody else.
cat > "$work/exec.json" <<JSON
{"grants": ["$caller_key"], "cwd": "$work", "jobs_dir": "$work/jobs"}
JSON
mkdir -p "$work/jobs"

EXEC_CONFIG="$work/exec.json" "$work/arc-legacy" --store "$work" serve \
  "exec://$work/exec-provider?manifest=$root/cmd/exec-provider/manifest.json" \
  > "$work/serve.log" 2>"$work/serve.err" &
serve_pid=$!

for _ in $(seq 1 50); do
  grep -q "serves on" "$work/serve.log" 2>/dev/null && break
  sleep 0.1
done
grep -q "serves on" "$work/serve.log" || fail "the citizen did not serve: $(cat "$work/serve.err")"
say "the citizen serves the exec provider"

# The citizen serves as the default key. Status asks with a temporary
# identity, so the relay keeps the route of the citizen.
arc status > /dev/null || fail "arc status failed"
sleep 0.5
kill -0 "$serve_pid" 2>/dev/null || fail "arc status stopped the citizen: $(cat "$work/serve.err")"
say "arc status leaves the route of a running citizen alone"

# Every command below runs as another citizen on the same relay.
caller() { arc --key "$caller_name" "$@"; }

for _ in $(seq 1 50); do
  caller resolve "$provider_key" | grep "$provider_key" > /dev/null && break
  sleep 0.2
done
caller resolve "$provider_key" | grep "exec+arc://$provider_key" > /dev/null || fail "the directory does not hold the citizen"
say "arc resolve finds the citizen and its capability"

caller discover "command" | grep "$provider_key" > /dev/null || fail "the search found nothing"
say "arc discover finds the capability"

caller call "exec+arc://$provider_key/" --manifest | grep '"scheme": "exec"' > /dev/null || fail "the manifest is wrong"
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

# A citizen whose machine paused. The caller keeps a wake hook for it, and
# arc runs the hook before the request, the way ssh runs a ProxyCommand.
kill "$serve_pid"
wait "$serve_pid" 2>/dev/null || true

cat > "$work/wake-exec" <<SCRIPT
#!/bin/sh
# The start script of the citizen: serve again, and exit 0 when ready.
echo woke >> "$work/woke"
EXEC_CONFIG="$work/exec.json" "$work/arc-legacy" --store "$work" serve \\
  "exec://$work/exec-provider?manifest=$root/cmd/exec-provider/manifest.json" \\
  > "$work/serve-woken.log" 2>"$work/serve-woken.err" < /dev/null &
for _ in \$(seq 1 50); do
  grep -q "serves on" "$work/serve-woken.log" 2>/dev/null && exit 0
  sleep 0.1
done
exit 1
SCRIPT
chmod +x "$work/wake-exec"

sleeper_key="$(tail -1 "$work/stranger.txt")"
cat > "$work/wake.toml" <<TOML
[wake."$provider_key"]
kind = "command"
argv = ["$work/wake-exec"]

[wake."$sleeper_key"]
kind = "command"
argv = ["sh", "-c", "echo 'no such machine' >&2; exit 3"]
TOML

caller call "exec+arc://$provider_key/" '{"argv":["echo","woken"]}' > "$work/woken.json" 2>"$work/woken.err" ||
  fail "the call did not wake the citizen: $(cat "$work/woken.err")"
grep -q "woken" "$work/woken.json" || fail "the woken citizen answered $(cat "$work/woken.json")"
[ "$(wc -l < "$work/woke" | tr -d ' ')" = 1 ] || fail "the hook ran $(wc -l < "$work/woke") times"
say "arc runs the wake hook, and the woken citizen answers"

caller call "exec+arc://$provider_key/" '{"argv":["echo","again"]}' > /dev/null || fail "the second call failed"
[ "$(wc -l < "$work/woke" | tr -d ' ')" = 1 ] || fail "the hook ran again for a citizen that answered a moment ago"
say "arc skips the hook of a citizen that answered a moment ago"

if caller call "exec+arc://$sleeper_key/" '{"argv":["true"]}' 2>"$work/wake-failed.txt"; then
  fail "a call went out after its wake hook failed"
fi
grep -q "wake_failed" "$work/wake-failed.txt" || fail "the failure is not wake_failed: $(cat "$work/wake-failed.txt")"
grep -q "no such machine" "$work/wake-failed.txt" || fail "the failure hides the hook: $(cat "$work/wake-failed.txt")"
say "a wake hook that fails stops the call with wake_failed"

caller resolve "$provider_key" | grep -q "^  online$" || fail "the serving citizen is not online: $(caller resolve "$provider_key")"
caller resolve "$sleeper_key" | grep -q "^  asleep" || fail "a citizen with a hook and no announcement is not asleep: $(caller resolve "$sleeper_key")"
caller resolve --json "$sleeper_key" | grep -q '"state": "asleep"' || fail "the JSON has no state: $(caller resolve --json "$sleeper_key")"
say "arc resolve shows a citizen online, and a citizen with a hook asleep"

# Later calls reach the woken citizen without a hook.
rm "$work/wake.toml"

caller resolve "$sleeper_key" | grep -q "^  offline$" || fail "a citizen without a hook or an announcement is not offline: $(caller resolve "$sleeper_key")"
started=$SECONDS
if caller call "exec+arc://$sleeper_key/" '{"argv":["true"]}' 2>"$work/offline.txt"; then
  fail "a call to a citizen that is not there succeeded"
fi
grep -q "peer_offline" "$work/offline.txt" || fail "the failure is not peer_offline: $(cat "$work/offline.txt")"
[ $((SECONDS - started)) -lt 5 ] || fail "peer_offline took $((SECONDS - started)) seconds"
say "a call to a citizen that is not there fails at once with peer_offline"

# A capability with a command line becomes a command of arc.
caller keys gen > /dev/null 2>&1 || true

EXEC_CONFIG="$work/exec.json" "$work/arc-legacy" --store "$work" --key "$echo_name" serve \
  "exec://$work/echo-provider?manifest=$root/citizen/testdata/echo/cli-manifest.json" \
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

caller tool list | grep "echo ->" > /dev/null || fail "the command is not listed"
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

caller trust list | grep allowed > /dev/null || fail "the signer is not trusted"
caller tool remove echo | grep "removed echo" > /dev/null || fail "the command was not removed"
say "arc trust list and arc tool remove answer"

kill "$echo_pid" 2>/dev/null || true

# One citizen listens, and another sends it a message. A third identity
# takes this, because one identity holds one route at a time.
stranger_key="$(arc --key "$stranger_name" whoami | sed -n 2p)"
"$work/arc-legacy" --store "$work" --key "$stranger_name" listen > "$work/listen.log" 2>&1 &
listen_pid=$!

for _ in $(seq 1 50); do
  grep -q "listens on" "$work/listen.log" 2>/dev/null && break
  sleep 0.1
done
grep -q "listens on" "$work/listen.log" || fail "the citizen did not listen: $(cat "$work/listen.log")"

caller send "$stranger_key" "a message from the other citizen" | grep "sent to" > /dev/null ||
  fail "the message was not sent"

for _ in $(seq 1 50); do
  grep -q "a message from the other citizen" "$work/listen.log" && break
  sleep 0.1
done
grep -q "a message from the other citizen" "$work/listen.log" ||
  fail "the message did not arrive: $(cat "$work/listen.log")"
say "arc send and arc listen carry a message between two citizens"

kill "$listen_pid" 2>/dev/null || true

caller info "$provider_key" | grep "exec+arc://$provider_key" > /dev/null || fail "arc info is wrong"
say "arc info reads the signed capability of a citizen"

arc --key "$stranger_name" publish > /dev/null || fail "the identity did not publish"
caller resolve "$stranger_name" | grep "on this machine" > /dev/null ||
  fail "the identity of this machine does not resolve"
say "arc publish and arc resolve answer without the relay"

# The DM provider carries sealed messages between two citizens, through the
# command line that its capability declares.
arc keys gen > "$work/dm.txt"
dm_name="$(head -1 "$work/dm.txt")"
dm_key="$(tail -1 "$work/dm.txt")"

DM_ROOT="$work/dm" "$work/arc-legacy" --store "$work" --key "$dm_name" serve \
  "exec://$work/dm-provider?manifest=$root/cmd/dm-provider/manifest.json" \
  > "$work/dm.log" 2>"$work/dm.err" &
dm_pid=$!

for _ in $(seq 1 50); do
  grep -q "serves on" "$work/dm.log" 2>/dev/null && break
  sleep 0.1
done
grep -q "serves on" "$work/dm.log" || fail "the dm citizen did not serve: $(cat "$work/dm.err")"

caller install "$dm_key" --yes > /dev/null || fail "the dm install failed"
arc --key "$stranger_name" install "$dm_key" --yes > /dev/null || fail "the reader could not install dm"
say "two citizens install the direct message capability"

caller dm send "$stranger_key" "a sealed hello" > "$work/dm-send.txt" 2>&1 ||
  fail "the message did not send: $(cat "$work/dm-send.txt")"
grep -q "id: " "$work/dm-send.txt" || fail "the send gave $(cat "$work/dm-send.txt")"
say "arc dm send seals a message to its reader"

arc --key "$stranger_name" dm inbox > "$work/dm-inbox.txt" 2>&1 ||
  fail "the inbox failed: $(cat "$work/dm-inbox.txt")"
# The inbox names its senders by petname, because the capability asks for it.
grep -q "$caller_name" "$work/dm-inbox.txt" || fail "the inbox holds $(cat "$work/dm-inbox.txt")"
say "the reader sees the message in its inbox"

# The provider holds ciphertext only.
grep -rl "a sealed hello" "$work/dm" > /dev/null 2>&1 &&
  fail "the provider holds the plain text"
say "the provider holds ciphertext only"

# The filters of a capability run over its answer. The thread command of dm
# writes petnames, and arc opens each sealed body with the key of the reader.
arc --key "$stranger_name" dm open "$caller_key" > "$work/dm-open.txt" 2>&1 ||
  fail "the thread failed: $(cat "$work/dm-open.txt")"
grep -q "a sealed hello" "$work/dm-open.txt" ||
  fail "the sealed body did not open: $(cat "$work/dm-open.txt")"
grep -q "$caller_key" "$work/dm-open.txt" &&
  fail "the answer still holds a raw public key"
say "the filters open the body and write petnames"

arc --key "$stranger_name" dm open "$caller_key" --raw > "$work/dm-raw.txt" 2>&1 ||
  fail "the raw thread failed: $(cat "$work/dm-raw.txt")"
grep -q "sealed-v1:" "$work/dm-raw.txt" || fail "--raw opened the body anyway"
say "--raw prints the answer as the provider wrote it"

# The cache keeps a copy of each record, sealed to the reader.
arc --key "$stranger_name" cache on dm | grep "the cache of dm is on" > /dev/null || fail "the cache did not turn on"
arc --key "$stranger_name" dm open "$caller_key" > /dev/null 2>&1
arc --key "$stranger_name" cache status dm | grep "the cache of dm is on and holds 1 records" > /dev/null ||
  fail "the cache kept nothing: $(arc --key "$stranger_name" cache status dm)"

arc --key "$stranger_name" cache search dm "sealed hello" | grep "a sealed hello" > /dev/null ||
  fail "the cache did not answer the search"
grep -rl "a sealed hello" "$work/cache" > /dev/null 2>&1 &&
  fail "the cache holds the plain text"
say "arc cache keeps the records sealed, and searches them"

arc --key "$stranger_name" cache clear dm | grep "removed 1 records" > /dev/null || fail "the cache did not clear"
arc --key "$stranger_name" cache off dm | grep "the cache of dm is off" > /dev/null || fail "the cache did not turn off"
say "arc cache clears and turns off"

# A bundle is a provider that lives in a directory.
arc apps init "$work/hello-app" | grep "wrote a bundle" > /dev/null || fail "arc apps init wrote nothing"
[ -x "$work/hello-app/run.sh" ] || fail "the runtime is not executable"
say "arc apps init writes a bundle"

arc keys gen > "$work/app.txt"
app_name="$(head -1 "$work/app.txt")"
app_key="$(tail -1 "$work/app.txt")"

"$work/arc-legacy" --store "$work" --key "$app_name" serve "$work/hello-app" > "$work/app.log" 2>"$work/app.err" &
app_pid=$!

for _ in $(seq 1 50); do
  grep -q "serves on" "$work/app.log" 2>/dev/null && break
  sleep 0.1
done
grep -q "serves on" "$work/app.log" || fail "the bundle did not serve: $(cat "$work/app.err")"
say "arc serve runs a bundle directory"

caller install "$app_key" --yes > /dev/null 2>&1 || fail "the bundle install failed"
caller hello-app "from the bundle" > "$work/app-reply.txt" 2>&1 ||
  fail "the bundle command failed: $(cat "$work/app-reply.txt")"
grep -q "hello from hello-app: from the bundle" "$work/app-reply.txt" ||
  fail "the bundle answered $(cat "$work/app-reply.txt")"
say "the bundle answers through its own command"

kill "$app_pid" 2>/dev/null || true
kill "$dm_pid" 2>/dev/null || true

caller lists add dm friends "$provider_key" | grep "$provider_key" > /dev/null || fail "the list was not saved"
caller lists ls dm | grep friends > /dev/null || fail "the list is not shown"
caller lists rm dm friends | grep "removed dm/friends" > /dev/null || fail "the list was not removed"
say "arc lists keeps a set of peers"

printf '\nARC runs end to end in Go\n'
