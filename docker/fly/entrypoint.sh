#!/bin/sh
# Run a relay and the DM provider in one container.
#
# The relay listens on port 7331. The DM provider connects to it through the
# loopback interface, so it needs no port of its own. Both identities live in
# the volume at /home/arc/.config/arc.
#
# If either process stops, this script stops. The platform then restarts the
# machine.
set -eu
umask 077

config_dir=/home/arc/.config/arc
dm_name_file="$config_dir/dm_key_name"

key_public() {
  arc --key "$1" whoami | sed -n 2p
}

# The relay keeps the identity that its pin names. Generate it one time.
if [ -z "${ARC_RELAY_KEY:-}" ]; then
  echo "ARC_RELAY_KEY must name the relay identity" >&2
  exit 1
fi

if [ ! -f "$dm_name_file" ]; then
  arc keys gen | sed -n 1p > "$dm_name_file"
fi
dm_key=$(cat "$dm_name_file")

relay_public=$(key_public "$ARC_RELAY_KEY")
dm_public=$(key_public "$dm_key")
echo "relay $relay_public"
echo "dm $dm_public"

arc-relay --key "$ARC_RELAY_KEY" --address :7331 &
relay_pid=$!

# The provider fails to announce until the relay accepts connections.
sleep 3

ARC_KEY=$dm_key \
  ARC_RELAY=127.0.0.1:7331 \
  ARC_RELAY_PUBKEY=$relay_public \
  DM_ROOT="$config_dir/dm-data" \
  arc serve /opt/arc-providers/dm &
dm_pid=$!

# Stop the container when either process stops.
while kill -0 "$relay_pid" 2>/dev/null && kill -0 "$dm_pid" 2>/dev/null; do
  sleep 5
done

echo "relay or dm stopped; exiting" >&2
kill "$relay_pid" "$dm_pid" 2>/dev/null || true
exit 1
