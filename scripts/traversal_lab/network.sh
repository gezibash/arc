#!/usr/bin/env bash
# Controlled router for disposable ARC test hosts. Never run on a working server.
set -euo pipefail
[[ $EUID == 0 && -f /etc/arc-traversal-lab/ready-for-router-setup ]] || { echo 'Dedicated lab host required' >&2; exit 1; }
action=${1:?setup|profile|evidence}
ns=arc-endpoint
host_if=arc-host
client_if=arc-peer
state=/var/lib/arc-traversal-network
valid_ip() { [[ $1 =~ ^10\.88\.0\.(10|11|12)$ ]]; }
case "$action" in
  setup)
    host_ip=${2:?host IP}; peer_ip=${3:?peer IP}; relay_ip=${4:?relay IP}
    valid_ip "$host_ip" && valid_ip "$peer_ip" && valid_ip "$relay_ip"
    [[ $host_ip != "$peer_ip" && $host_ip != "$relay_ip" ]]
    [[ ! -e $state ]] || { echo 'Network already configured' >&2; exit 1; }
    mkdir -p "$state"
    printf '%s\n%s\n%s\n' "$host_ip" "$peer_ip" "$relay_ip" > "$state/addresses"
    ip netns add "$ns"
    ip link add "$host_if" type veth peer name "$client_if"
    ip link set "$client_if" netns "$ns"
    ip addr add 10.200.0.1/24 dev "$host_if"
    ip link set "$host_if" up
    ip -n "$ns" addr add 10.200.0.2/24 dev "$client_if"
    ip -n "$ns" link set lo up
    ip -n "$ns" link set "$client_if" up
    ip -n "$ns" route add default via 10.200.0.1
    sysctl -w net.ipv4.ip_forward=1
    sysctl -w net.ipv4.conf.all.rp_filter=0
    sysctl -w net.ipv4.conf.default.rp_filter=0
    sysctl -w net.ipv4.conf."$host_if".rp_filter=0
    # These chains affect only synthetic test traffic. Administration stays outside.
    iptables -N ARC_LAB_INPUT
    iptables -I INPUT 1 -s "$peer_ip" -p tcp -j ARC_LAB_INPUT
    iptables -N ARC_LAB_FORWARD
    iptables -I FORWARD 1 -j ARC_LAB_FORWARD
    iptables -t nat -N ARC_LAB_SNAT
    iptables -t nat -A POSTROUTING -s 10.200.0.2/32 -j ARC_LAB_SNAT
    iptables -t nat -N ARC_LAB_DNAT
    iptables -t nat -I PREROUTING 1 -s "$peer_ip" -d "$host_ip" -p tcp -j ARC_LAB_DNAT
    "$0" profile stable
    ;;
  profile)
    profile=${2:?stable|changed|blocked|impaired|listener}
    case "$profile" in stable|changed|blocked|impaired|listener) ;; *) exit 2;; esac
    mapfile -t addresses < "$state/addresses"
    host_ip=${addresses[0]}; peer_ip=${addresses[1]}; relay_ip=${addresses[2]}
    # Call only between trials, after old endpoint processes have stopped.
    iptables -F ARC_LAB_INPUT
    iptables -A ARC_LAB_INPUT -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
    iptables -A ARC_LAB_INPUT -j DROP
    iptables -F ARC_LAB_FORWARD
    if [[ $profile == blocked ]]; then
      iptables -A ARC_LAB_FORWARD -s 10.200.0.2/32 -d "$peer_ip" -j DROP
      iptables -A ARC_LAB_FORWARD -s "$peer_ip" -d 10.200.0.2/32 -j DROP
    fi
    iptables -A ARC_LAB_FORWARD -i "$host_if" -s 10.200.0.2/32 -d "$relay_ip" -p tcp -j ACCEPT
    iptables -A ARC_LAB_FORWARD -i "$host_if" -s 10.200.0.2/32 -d "$peer_ip" -p tcp -j ACCEPT
    iptables -A ARC_LAB_FORWARD -o "$host_if" -d 10.200.0.2/32 -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
    iptables -t nat -F ARC_LAB_DNAT
    if [[ $profile == listener ]]; then
      iptables -t nat -A ARC_LAB_DNAT -p tcp --dport 4444 -j DNAT --to-destination 10.200.0.2:4444
      iptables -A ARC_LAB_FORWARD -s "$peer_ip" -d 10.200.0.2/32 -p tcp --dport 4444 -j ACCEPT
    fi
    iptables -A ARC_LAB_FORWARD -i "$host_if" -j DROP
    iptables -A ARC_LAB_FORWARD -o "$host_if" -j DROP
    iptables -t nat -F ARC_LAB_SNAT
    if [[ $profile == changed ]]; then
      iptables -t nat -A ARC_LAB_SNAT -d "$peer_ip" -p tcp -j SNAT --to-source "$host_ip:45000"
    fi
    iptables -t nat -A ARC_LAB_SNAT -j SNAT --to-source "$host_ip"
    if tc qdisc show dev "$host_if" | grep -q 'netem'; then tc qdisc del dev "$host_if" root; fi
    if [[ $profile == impaired ]]; then tc qdisc add dev "$host_if" root netem delay 40ms loss 2%; fi
    # Only remove connection tracking entries for the dedicated endpoint address.
    conntrack -D -s 10.200.0.2 >/dev/null 2>&1 || [[ $? == 1 ]]
    conntrack -D -d 10.200.0.2 >/dev/null 2>&1 || [[ $? == 1 ]]
    conntrack -D --reply-src 10.200.0.2 >/dev/null 2>&1 || [[ $? == 1 ]]
    conntrack -D --reply-dst 10.200.0.2 >/dev/null 2>&1 || [[ $? == 1 ]]
    printf '%s\n' "$profile" > "$state/profile"
    ;;
  evidence)
    cat "$state/profile"
    ip -n "$ns" route show
    iptables -nvxL ARC_LAB_INPUT
    iptables -nvxL ARC_LAB_FORWARD
    iptables -t nat -nvxL ARC_LAB_SNAT
    iptables -t nat -nvxL ARC_LAB_DNAT
    conntrack -L -s 10.200.0.2 -o extended || [[ $? == 1 ]]
    conntrack -L -d 10.200.0.2 -o extended || [[ $? == 1 ]]
    conntrack -L --reply-src 10.200.0.2 -o extended || [[ $? == 1 ]]
    conntrack -L --reply-dst 10.200.0.2 -o extended || [[ $? == 1 ]]
    tc qdisc show dev "$host_if"
    ;;
  *) exit 2 ;;
esac
