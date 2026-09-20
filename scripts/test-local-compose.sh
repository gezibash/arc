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
preview_image="${ARC_LOCAL_PREVIEW_IMAGE:-arc-local-preview:0.6.0}"
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

field() {
  local output=$1
  local name=$2
  local value

  value="$(awk -v name="$name" '$1 == name { print $2; exit }' <<<"$output")"
  [[ -n $value ]] || fail "missing $name from command output"
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
  "${compose[@]}" run --rm -T --no-deps -e "ARC_KEY=$key_name" client "$@"
}

agora_runtime() {
  local code=$1
  shift
  "${compose[@]}" run --rm -T --no-deps --entrypoint /app/bin/arc_runtime "$@" client eval "$code"
}

agora_post_id() {
  local output=$1
  local author=$2
  local board=$3
  local body=$4
  local parent=$5

  printf '%s' "$output" |
    agora_runtime '
      post = case :json.decode(IO.read(:stdio, :eof)) do
        %{"post" => value} when is_map(value) -> value
        _ -> System.halt(1)
      end
      parent = if System.get_env("EXPECTED_PARENT") == "root", do: :null, else: System.get_env("EXPECTED_PARENT")
      valid = is_binary(post["id"]) and Regex.match?(~r/\A[0-9a-f]{64}\z/, post["id"]) and
        post["author"] == System.get_env("EXPECTED_AUTHOR") and
        post["board"] == System.get_env("EXPECTED_BOARD") and
        post["body"] == System.get_env("EXPECTED_BODY") and post["parent"] == parent
      if valid, do: IO.puts(post["id"]), else: System.halt(1)
    ' \
      -e "EXPECTED_AUTHOR=$author" \
      -e "EXPECTED_BOARD=$board" \
      -e "EXPECTED_BODY=$body" \
      -e "EXPECTED_PARENT=$parent"
}

agora_assert_feed() {
  local output=$1
  local id=$2
  local author=$3
  local board=$4
  local body=$5

  printf '%s' "$output" |
    agora_runtime '
      result = :json.decode(IO.read(:stdio, :eof))
      posts = result["posts"]
      valid = is_list(posts) and Map.has_key?(result, "next") and
        Enum.any?(posts, fn post ->
          post["id"] == System.get_env("EXPECTED_ID") and
            post["author"] == System.get_env("EXPECTED_AUTHOR") and
            post["board"] == System.get_env("EXPECTED_BOARD") and
            post["body"] == System.get_env("EXPECTED_BODY") and post["parent"] == :null
        end)
      if valid, do: :ok, else: System.halt(1)
    ' \
      -e "EXPECTED_ID=$id" \
      -e "EXPECTED_AUTHOR=$author" \
      -e "EXPECTED_BOARD=$board" \
      -e "EXPECTED_BODY=$body"
}

agora_assert_thread() {
  local output=$1
  local parent_id=$2
  local parent_author=$3
  local parent_body=$4
  local reply_id=$5
  local reply_author=$6
  local reply_body=$7
  local board=$8

  printf '%s' "$output" |
    agora_runtime '
      result = :json.decode(IO.read(:stdio, :eof))
      parent = result["post"]
      posts = result["posts"]
      valid = is_map(parent) and is_list(posts) and Map.has_key?(result, "next") and
        parent["id"] == System.get_env("PARENT_ID") and
        parent["author"] == System.get_env("PARENT_AUTHOR") and
        parent["board"] == System.get_env("BOARD") and
        parent["body"] == System.get_env("PARENT_BODY") and parent["parent"] == :null and
        Enum.any?(posts, fn reply ->
          reply["id"] == System.get_env("REPLY_ID") and
            reply["author"] == System.get_env("REPLY_AUTHOR") and
            reply["board"] == System.get_env("BOARD") and
            reply["body"] == System.get_env("REPLY_BODY") and reply["parent"] == System.get_env("PARENT_ID")
        end)
      if valid, do: :ok, else: System.halt(1)
    ' \
      -e "PARENT_ID=$parent_id" \
      -e "PARENT_AUTHOR=$parent_author" \
      -e "PARENT_BODY=$parent_body" \
      -e "REPLY_ID=$reply_id" \
      -e "REPLY_AUTHOR=$reply_author" \
      -e "REPLY_BODY=$reply_body" \
      -e "BOARD=$board"
}

install_provider() {
  local agent_key=$1
  local provider_key=$2
  local namespace=$3
  local attempt output

  for attempt in $(seq 1 20); do
    if output="$(client_as "$agent_key" install "$provider_key" primary --trust 2>&1)"; then
      grep -Eq "^Installed ${namespace} " <<<"$output" ||
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
  local namespace=$2
  shift 2
  local attempt output

  for attempt in $(seq 1 20); do
    if output="$(client_as "$agent_key" "$namespace" "$@")"; then
      printf '%s\n' "$output"
      return
    fi

    if (( attempt == 20 )); then
      fail "${namespace} did not recover after restart"
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

# Build the unreleased core once from this checkout. The provider package then
# uses that exact image through its build argument; no unpublished GHCR tag is
# implied. The service image remains project-scoped for isolated cleanup.
docker build --tag "$preview_image" --file "$repo_root/Dockerfile" "$repo_root"
export ARC_IMAGE="$preview_image"
export ARC_LOCAL_IMAGE="${project_name}-services:0.6.0"

# The port is unused by this test: clients address the relay through the
# Compose network. An ephemeral host port prevents collisions with a local relay.
export ARC_LOCAL_PORT=0

"${compose[@]}" up -d --build --wait

# New service volumes must use the current selector spelling. Do this before
# any conversion so a fresh v0.6.0 service cannot silently retain old state.
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

if unexpected_client="$(client keys show 2>&1)"; then
  fail 'client unexpectedly had an identity before key generation'
fi
require_match "$unexpected_client" 'No active key' 'client did not report its missing identity'

agent_a_generated="$(client keys gen)"
agent_a_name="$(field "$agent_a_generated" name:)"
agent_a_public_key="$(field "$agent_a_generated" public_key:)"
agent_b_generated="$(client keys gen)"
agent_b_name="$(field "$agent_b_generated" name:)"
agent_b_public_key="$(field "$agent_b_generated" public_key:)"

require_match "$agent_a_public_key" '^[0-9a-f]{64}$' 'invalid first agent public key'
require_match "$agent_b_public_key" '^[0-9a-f]{64}$' 'invalid second agent public key'
[[ $agent_a_public_key != "$agent_b_public_key" ]] || fail 'agent keys must be distinct'

agent_a_show="$(client_as "$agent_a_name" keys show)"
agent_b_show="$(client_as "$agent_b_name" keys show)"
require_match "$agent_a_show" "$agent_a_public_key" 'first generated key cannot be selected'
require_match "$agent_b_show" "$agent_b_public_key" 'second generated key cannot be selected'

agent_a_publish="$(client_as "$agent_a_name" publish)"
agent_b_publish="$(client_as "$agent_b_name" publish)"
require_match "$agent_a_publish" 'Published to control plane' 'first agent did not publish'
require_match "$agent_a_publish" 'keyex:      published' 'first agent key exchange was not published'
require_match "$agent_b_publish" 'Published to control plane' 'second agent did not publish'
require_match "$agent_b_publish" 'keyex:      published' 'second agent key exchange was not published'

# Each install opens a pinned relay session, publishes the caller and key
# exchange material to its directory, and verifies the provider package.
for agent_key in "$agent_a_name" "$agent_b_name"; do
  install_provider "$agent_key" "$journal_provider_key" journal
  install_provider "$agent_key" "$dm_provider_key" dm
  install_provider "$agent_key" "$agora_provider_key" agora
done

journal_body="ARC compose journal smoke ${project_name}"
journal_write="$(printf '%s\n' "$journal_body" | client_as "$agent_a_name" journal write smoke/private/entry --title 'Compose smoke')"
require_match "$journal_write" '^(written|rev:|ok|created)' 'journal write did not report success'

journal_read_a="$(client_as "$agent_a_name" journal read smoke/private/entry)"
require_match "$journal_read_a" "$journal_body" 'journal owner could not read the written page'

if denied_read="$(client_as "$agent_b_name" journal read smoke/private/entry 2>&1)"; then
  fail 'journal read succeeded before the owner granted access'
fi
require_match "$denied_read" '[Ff]orbidden' 'journal denied read did not report forbidden'

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

agora_post_body="ARC compose Agora post ${project_name}"
agora_post_output="$(client_as "$agent_a_name" agora post "$agora_post_body")"
agora_post_id="$(agora_post_id "$agora_post_output" "$agent_a_public_key" "$agora_provider_key" "$agora_post_body" root)"
require_match "$agora_post_id" '^[0-9a-f]{64}$' 'Agora post did not return a valid id'

agora_feed_b="$(client_as "$agent_b_name" agora feed)"
agora_assert_feed "$agora_feed_b" "$agora_post_id" "$agent_a_public_key" "$agora_provider_key" "$agora_post_body"

agora_read_b="$(client_as "$agent_b_name" agora read "$agora_post_id")"
agora_read_id="$(agora_post_id "$agora_read_b" "$agent_a_public_key" "$agora_provider_key" "$agora_post_body" root)"
[[ $agora_read_id == "$agora_post_id" ]] || fail 'Agora read returned a different post'

agora_reply_body="ARC compose Agora reply ${project_name}"
agora_reply_output="$(client_as "$agent_b_name" agora reply "$agora_post_id" "$agora_reply_body")"
agora_reply_id="$(agora_post_id "$agora_reply_output" "$agent_b_public_key" "$agora_provider_key" "$agora_reply_body" "$agora_post_id")"
require_match "$agora_reply_id" '^[0-9a-f]{64}$' 'Agora reply did not return a valid id'

agora_thread_a="$(client_as "$agent_a_name" agora thread "$agora_post_id")"
agora_assert_thread "$agora_thread_a" "$agora_post_id" "$agent_a_public_key" "$agora_post_body" "$agora_reply_id" "$agent_b_public_key" "$agora_reply_body" "$agora_provider_key"

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

# Existing service volumes from v0.3.0 use default_key. Simulate that layout
# only inside this disposable project and require every provider to preserve
# its identity when v0.6.0 starts through the legacy fallback.
for service in relay journal dm agora; do
  "${compose[@]}" exec -T "$service" sh -ec '
    test -f /home/arc/.config/arc/default.key
    test ! -e /home/arc/.config/arc/default_key
    mv /home/arc/.config/arc/default.key /home/arc/.config/arc/default_key
  ' || fail "could not create legacy selector for ${service}"
done

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

agora_thread_after_restart="$(read_after_restart "$agent_a_name" agora thread "$agora_post_id")"
agora_assert_thread "$agora_thread_after_restart" "$agora_post_id" "$agent_a_public_key" "$agora_post_body" "$agora_reply_id" "$agent_b_public_key" "$agora_reply_body" "$agora_provider_key"
