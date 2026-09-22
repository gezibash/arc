#!/usr/bin/env bash
# Smoke test for the local Docker Compose relay, journal, DM, and Agora stack.
#
# This script creates an isolated Compose project and removes only that
# project's containers and volumes. On failure it saves service logs in a
# temporary directory without printing provider storage or key material.

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
compose_file="$repo_root/compose.yaml"
test_compose_file="$repo_root/docker/local/compose.test.yaml"
project_name="arc-local-smoke-${RANDOM}-${RANDOM}"
log_dir="$(mktemp -d "${TMPDIR:-/tmp}/arc-compose-smoke.XXXXXX")"
declare -a compose=(docker compose -p "$project_name" -f "$compose_file" -f "$test_compose_file")

fail() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

require_match() {
  local value=$1
  local pattern=$2
  local description=$3

  [[ $value =~ $pattern ]] || fail "$description"
}

# arc keys gen prints the key name on line 1 and the public key on line 2.
line() {
  local output=$1
  local number=$2
  local value

  value="$(sed -n "${number}p" <<<"$output")"
  [[ -n $value ]] || fail "missing line ${number} from command output"
  printf '%s\n' "$value"
}

info_field() {
  local output=$1
  local name=$2
  local value

  value="$(sed -n "s/^export ${name}=//p" <<<"$output" | head -n 1)"
  [[ -n $value ]] || fail "missing ${name} from compose info"
  printf '%s\n' "$value"
}

client() {
  "${compose[@]}" run --rm -T --no-deps client "$@"
}

client_as() {
  local key_name=$1
  shift
  "${compose[@]}" run --rm -T --no-deps -e "ARC_LEGACY_KEY=$key_name" client "$@"
}

install_provider() {
  local agent_key=$1
  local provider_key=$2
  local namespace=$3
  local attempt output

  for attempt in $(seq 1 20); do
    if output="$(client_as "$agent_key" install "$provider_key" primary --yes 2>&1)"; then
      grep -Eq "^${namespace} runs " <<<"$output" ||
        fail "${namespace} provider install did not report success"
      return
    fi

    if (( attempt == 20 )); then
      printf '%s\n' "$output" >&2
      fail "${namespace} provider did not become available"
    fi

    sleep 1
  done
}

read_after_restart() {
  local agent_key=$1
  local command=$2
  shift 2
  local attempt output

  # A provider that is not connected never answers, so each attempt waits
  # 5 seconds, not the default 30.
  for attempt in $(seq 1 20); do
    if output="$(client_as "$agent_key" "$command" "$@" --timeout 5)"; then
      printf '%s\n' "$output"
      return
    fi

    if (( attempt == 20 )); then
      fail "arc ${command} $1 did not answer after restart"
    fi

    sleep 1
  done
}

save_failure_logs() {
  "${compose[@]}" logs --no-color >"$log_dir/compose.log" 2>&1 || true
  printf 'Compose smoke test failed. Service logs: %s\n' "$log_dir/compose.log" >&2
}

assert_project_removed() {
  local kind=$1
  shift
  local ids

  if ! ids="$("$@")"; then
    printf 'error: could not enumerate %s for Compose project %s\n' "$kind" "$project_name" >&2
    return 1
  fi

  if [[ -n $ids ]]; then
    printf 'error: Compose project %s still has %s: %s\n' "$project_name" "$kind" "$ids" >&2
    return 1
  fi
}

cleanup() {
  local status=$?
  local cleanup_status=0
  local logs_saved=false

  if (( status != 0 )); then
    save_failure_logs
    logs_saved=true
  else
    rmdir "$log_dir" 2>/dev/null || true
  fi

  if ! "${compose[@]}" --profile tools down --volumes --remove-orphans >/dev/null 2>&1; then
    printf 'error: failed to remove Compose project %s\n' "$project_name" >&2
    cleanup_status=1
  fi

  assert_project_removed containers docker ps -aq --filter "label=com.docker.compose.project=$project_name" ||
    cleanup_status=1
  assert_project_removed volumes docker volume ls -q --filter "label=com.docker.compose.project=$project_name" ||
    cleanup_status=1
  assert_project_removed networks docker network ls -q --filter "label=com.docker.compose.project=$project_name" ||
    cleanup_status=1

  if (( status == 0 && cleanup_status != 0 )); then
    status=$cleanup_status
  fi

  if (( status != 0 )) && [[ $logs_saved != true ]]; then
    mkdir -p "$log_dir" 2>/dev/null || true
    save_failure_logs
  fi

  if (( status == 0 )); then
    printf 'Local Compose smoke test passed for %s.\n' "$project_name"
  fi

  exit "$status"
}

trap cleanup EXIT

[[ -f $compose_file ]] || fail "missing $compose_file"
[[ -f $test_compose_file ]] || fail "missing $test_compose_file"
command -v docker >/dev/null || fail "docker is required"
docker compose version >/dev/null || fail "Docker Compose v2 is required"

# The service image builds from this checkout. Its name is scoped to the
# project, so this test never replaces the image of your own stack.
export ARC_LOCAL_IMAGE="${project_name}-services:0.10.0"

# The port is unused by this test: clients address the relay through the
# Compose network. An ephemeral host port prevents collisions with a local relay.
export ARC_LOCAL_PORT=0

"${compose[@]}" up -d --build --wait

# Each service writes the current selector file, default.key.
for service in relay journal dm agora; do
  "${compose[@]}" exec -T "$service" test -f /home/arc/.config/arc/default.key ||
    fail "${service} did not create default.key"
  if "${compose[@]}" exec -T "$service" test -e /home/arc/.config/arc/default_key; then
    fail "${service} unexpectedly created legacy default_key"
  fi
done

info_before="$("${compose[@]}" run --rm -T --no-deps info)"
relay_address="$(info_field "$info_before" ARC_RELAY)"
relay_public_key="$(info_field "$info_before" ARC_RELAY_PUBKEY)"
journal_provider_key="$(info_field "$info_before" ARC_JOURNAL_PROVIDER)"
dm_provider_key="$(info_field "$info_before" ARC_DM_PROVIDER)"
agora_provider_key="$(info_field "$info_before" ARC_AGORA_PROVIDER)"
published_relay_address="$("${compose[@]}" port relay 7331)"

require_match "$relay_address" '^127\.0\.0\.1:0$' 'info did not preserve the requested ephemeral host port'
require_match "$published_relay_address" '^127\.0\.0\.1:[1-9][0-9]*$' 'relay did not publish a loopback ephemeral host port'
require_match "$relay_public_key" '^[0-9a-f]{64}$' 'invalid relay public key from info'
require_match "$journal_provider_key" '^[0-9a-f]{64}$' 'invalid journal provider key from info'
require_match "$dm_provider_key" '^[0-9a-f]{64}$' 'invalid DM provider key from info'
require_match "$agora_provider_key" '^[0-9a-f]{64}$' 'invalid Agora provider key from info'

if unexpected_client="$(client whoami 2>&1)"; then
  fail 'client unexpectedly had an identity before key generation'
fi
require_match "$unexpected_client" 'no identity' 'client did not report its missing identity'

agent_a_generated="$(client keys gen)"
agent_a_name="$(line "$agent_a_generated" 1)"
agent_a_public_key="$(line "$agent_a_generated" 2)"
agent_b_generated="$(client keys gen)"
agent_b_name="$(line "$agent_b_generated" 1)"
agent_b_public_key="$(line "$agent_b_generated" 2)"

require_match "$agent_a_public_key" '^[0-9a-f]{64}$' 'invalid first agent public key'
require_match "$agent_b_public_key" '^[0-9a-f]{64}$' 'invalid second agent public key'
[[ $agent_a_public_key != "$agent_b_public_key" ]] || fail 'agent keys must be distinct'

agent_a_whoami="$(client_as "$agent_a_name" whoami)"
agent_b_whoami="$(client_as "$agent_b_name" whoami)"
require_match "$agent_a_whoami" "$agent_a_public_key" 'first generated key cannot be selected'
require_match "$agent_b_whoami" "$agent_b_public_key" 'second generated key cannot be selected'
require_match "$agent_a_whoami" 'chosen by ARC_LEGACY_KEY' 'ARC_LEGACY_KEY did not select the first key'

relay_status="$(client_as "$agent_a_name" status)"
require_match "$relay_status" "key +${relay_public_key}" 'client could not reach the pinned relay'

# Each install verifies the signed package of the provider over the pinned
# relay and saves it as a command of the selected agent.
for agent_key in "$agent_a_name" "$agent_b_name"; do
  install_provider "$agent_key" "$journal_provider_key" journal
  install_provider "$agent_key" "$dm_provider_key" dm
  install_provider "$agent_key" "$agora_provider_key" agora
done

journal_body="ARC compose journal smoke ${project_name}"
journal_write="$(printf '%s\n' "$journal_body" | client_as "$agent_a_name" journal write smoke/private/entry --title 'Compose smoke')"
require_match "$journal_write" '^rev: [0-9a-f]+' 'journal write did not report success'

journal_read_a="$(client_as "$agent_a_name" journal read smoke/private/entry)"
require_match "$journal_read_a" "$journal_body" 'journal owner could not read the written page'

if denied_read="$(client_as "$agent_b_name" journal read smoke/private/entry 2>&1)"; then
  fail 'journal read succeeded before the owner granted access'
fi
require_match "$denied_read" 'forbidden' 'journal denied read did not report forbidden'

journal_acl="$(client_as "$agent_a_name" journal acl smoke add "$agent_b_public_key")"
require_match "$journal_acl" "$agent_b_public_key" 'journal ACL grant did not include the second agent'

journal_read_b="$(client_as "$agent_b_name" journal read smoke/private/entry)"
require_match "$journal_read_b" "$journal_body" 'granted agent could not read the journal page'

dm_body="ARC compose DM smoke ${project_name}"
dm_send="$(printf '%s\n' "$dm_body" | client_as "$agent_a_name" dm send "$agent_b_public_key")"
dm_id="$(sed -n 's/^id: \([0-9A-HJKMNP-TV-Z]\{26\}\)$/\1/p' <<<"$dm_send" | head -n 1)"
[[ -n $dm_id ]] || fail 'DM send did not return a message id'

dm_inbox="$(client_as "$agent_b_name" dm inbox --unread)"
require_match "$dm_inbox" "$dm_id" 'recipient inbox did not contain the sent DM'

dm_read="$(client_as "$agent_b_name" dm read "$dm_id")"
require_match "$dm_read" "$dm_body" 'recipient could not decrypt the sent DM'

# arc does not build Agora posts yet (CHANGELOG 0.10.0, known issues), so
# arc agora fails. The test reads the board through arc call instead.
agora_address="agora+arc://${agora_provider_key}/"
agora_feed="$(client_as "$agent_b_name" call "$agora_address" '{"op":"feed"}')"
require_match "$agora_feed" '"posts":\[' 'Agora feed did not return a post list'

# Search the provider storage only for the known synthetic plaintext. Do not
# print storage contents, which could contain unrelated encrypted messages.
"${compose[@]}" exec -T -e "SMOKE_PLAINTEXT=$dm_body" dm sh -ec '
  if grep -R -F -- "sealed-v1:" /home/arc/.arc/dm >/dev/null 2>&1; then
    :
  else
    status=$?
    if [ "$status" -eq 1 ]; then
      echo "error: DM provider did not store a sealed message" >&2
      exit 1
    fi
    echo "error: could not inspect DM provider storage" >&2
    exit "$status"
  fi

  if grep -R -F -- "$SMOKE_PLAINTEXT" /home/arc/.arc/dm >/dev/null 2>&1; then
    echo "error: DM provider stored plaintext" >&2
    exit 1
  else
    status=$?
    if [ "$status" -ne 1 ]; then
      echo "error: could not inspect DM provider storage" >&2
      exit "$status"
    fi
  fi
'

"${compose[@]}" restart relay journal dm agora
"${compose[@]}" up -d --wait

info_after="$("${compose[@]}" run --rm -T --no-deps info)"
[[ $(info_field "$info_after" ARC_RELAY_PUBKEY) == "$relay_public_key" ]] ||
  fail 'relay identity changed after restart'
[[ $(info_field "$info_after" ARC_JOURNAL_PROVIDER) == "$journal_provider_key" ]] ||
  fail 'journal provider identity changed after restart'
[[ $(info_field "$info_after" ARC_DM_PROVIDER) == "$dm_provider_key" ]] ||
  fail 'DM provider identity changed after restart'
[[ $(info_field "$info_after" ARC_AGORA_PROVIDER) == "$agora_provider_key" ]] ||
  fail 'Agora board identity changed after restart'

journal_read_after_restart="$(read_after_restart "$agent_b_name" journal read smoke/private/entry)"
require_match "$journal_read_after_restart" "$journal_body" 'journal data did not survive restart'

dm_read_after_restart="$(read_after_restart "$agent_b_name" dm read "$dm_id")"
require_match "$dm_read_after_restart" "$dm_body" 'DM data did not survive restart'

agora_feed_after_restart="$(read_after_restart "$agent_b_name" call "$agora_address" '{"op":"feed"}')"
require_match "$agora_feed_after_restart" '"posts":\[' 'Agora board did not answer after restart'
