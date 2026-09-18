#!/bin/sh
set -eu
umask 077

fail() {
  printf '%s\n' "$*" >&2
  exit 1
}

public_key() {
  key=$(cat "$1") || return 1
  case "$key" in
    ''|*[!0-9a-f]*) fail "Invalid public key in $1" ;;
  esac
  [ "${#key}" -eq 64 ] || fail "Invalid public key length in $1"
  printf '%s' "$key"
}

prepare_identity() {
  config_dir=/home/arc/.config/arc
  if [ ! -f "$config_dir/default.key" ] && [ ! -f "$config_dir/default_key" ]; then
    # Never silently replace an existing identity after partial state loss.
    for key_file in "$config_dir"/keys/*.toml; do
      [ ! -e "$key_file" ] || fail "Keys exist without a default; restore $config_dir/default.key."
    done
    arc keys gen >/dev/null
  fi
  identity=$(arc keys show)
  identity_name=$(printf '%s\n' "$identity" | awk '$1 == "name:" {print $2}')
  identity_public=$(printf '%s\n' "$identity" | awk '$1 == "public_key:" {print $2}')
  case "$identity_name" in ''|*[!a-z0-9-]*) fail "Could not read active identity name." ;; esac
  printf '%s\n' "$identity_public" > /home/arc/public/public_key.tmp
  public_key /home/arc/public/public_key.tmp >/dev/null
  mv /home/arc/public/public_key.tmp /home/arc/public/public_key
}

configure_relay() {
  export ARC_RELAY=relay:7331
  ARC_RELAY_PUBKEY=$(public_key /run/arc/relay/public_key)
  export ARC_RELAY_PUBKEY
}

case "${1:-}" in
  relay)
    prepare_identity
    exec arc relay --key "$identity_name" --port 7331
    ;;
  journal|dm|agora|releases)
    provider=$1
    prepare_identity
    configure_relay
    exec arc serve "/opt/arc-providers/$provider"
    ;;
  client)
    shift
    configure_relay
    exec arc "$@"
    ;;
  info)
    port=${ARC_LOCAL_PORT:-7331}
    case "$port" in ''|*[!0-9]*) fail "ARC_LOCAL_PORT must be a port number." ;; esac
    relay_public=$(public_key /run/arc/relay/public_key)
    journal_public=$(public_key /run/arc/journal/public_key)
    dm_public=$(public_key /run/arc/dm/public_key)
    agora_public=$(public_key /run/arc/agora/public_key)
    # All values are validated public metadata, safe to source in a host shell.
    printf 'export ARC_RELAY=127.0.0.1:%s\n' "$port"
    printf 'export ARC_RELAY_PUBKEY=%s\n' "$relay_public"
    printf 'export ARC_JOURNAL_PROVIDER=%s\n' "$journal_public"
    printf 'export ARC_DM_PROVIDER=%s\n' "$dm_public"
    printf 'export ARC_AGORA_PROVIDER=%s\n' "$agora_public"
    ;;
  *) fail "Usage: entrypoint.sh relay|journal|dm|agora|releases|client|info" ;;
esac
