# Direct request/reply connections

## Status

ARC remote traffic stays relay-backed by default. The first direct profile is
implemented as an explicit opt-in for bounded request/reply calls. It uses the
same identity-addressed URI, capability verification, grants, and application
frames as relay delivery. It does not turn an ARC URI into a raw TCP tunnel.

Both endpoints must start with a configured, pinned relay and a local direct-policy file.
The endpoints negotiate through that relay before either reveals a listed direct
address. A missing or nonmatching rule leaves the request on relays. An invalid
policy file is a local configuration error and the CLI rejects it before starting
the request or provider agent.

## Operator policy

Pass `--direct-policy PATH` to both `arc serve` and `arc request`, together with
`--relay` and `--relay-pubkey`. The option is unavailable without a configured,
pinned relay. The file is JSON and has this exact outer shape:

```json
{
  "version": 1,
  "rules": [
    {
      "peer": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
      "capability": "primary",
      "scheme": "sqlite",
      "path": "/main",
      "lease_ms": 30000,
      "dial": ["192.0.2.44"],
      "listen": {"bind": "0.0.0.0", "address": "192.0.2.44", "port": 0}
    }
  ]
}
```

The public key and addresses above are documentation placeholders. Replace them
with the other endpoint's full lowercase public key and an address that endpoint
is actually permitted to reach. The example address is from the documentation
range and cannot work on the public Internet.

Each rule grants exactly one peer, capability ID, application scheme, and URI
path. A file accepts at most 32 rules and cannot contain duplicate scopes.
`lease_ms` defaults to 30000 and must be from 1000 through 120000. `dial` holds
at most four literal IPv4 or IPv6 addresses; host names, wildcard addresses, and
link-local or multicast addresses are rejected. At least one of `dial` or
`listen` is required.

`listen` is optional. `bind` is the local literal address to bind, `address` is
the literal address advertised to the peer, and `port` may be 0 so the operating
system chooses a port. The advertised listener address must appear in the other
endpoint's `dial` list. The process does not configure a router, discover a
public mapping, or use STUN. The operator must arrange firewall rules and any
public address mapping.

For a provider that listens and a citizen that dials it, the provider's rule
has `listen` and the citizen's matching rule lists the provider address in
`dial`. To reverse the direction, give the citizen a listener and place the
citizen address in the provider's `dial` list. Both endpoints may have a
listener, but this profile admits one local candidate for each endpoint and
direction.

```sh
arc serve 'exec:///absolute/path/to/runtime?manifest=/absolute/path/to/capability.json' \
  --relay relay.example:7331 --relay-pubkey '<relay-key>' \
  --direct-policy /absolute/path/to/provider-direct.json

arc request 'sqlite+arc://<provider-public-key>/main' \
  --body '{"sql":"SELECT 1"}' \
  --relay relay.example:7331 --relay-pubkey '<relay-key>' \
  --direct-policy /absolute/path/to/citizen-direct.json
```

The one-shot `arc request` process exits after its reply, so it cannot retain a
direct route for later commands. A long-running `Arc.Data.Agent` that makes
repeated `Arc.Data.Protocol.request/4` calls can retain an admitted route until
its lease ends.

## What the connection proves

The relay offer binds both ARC identities, the selected capability package hash,
scheme, path, request/reply method, fresh ARC session material, route generation,
certificate fingerprints, and lease. Each promoted route has separate ARC crypto
contexts. The TLS connection pins the ephemeral certificate fingerprints and
requires an ARC identity proof bound to the negotiated context before ARC frames
are admitted. A reachable IP address or a successful TLS socket alone does not
authorize a request.

The carrier is Erlang/OTP TLS over TCP. It uses the existing Erlang runtime and
does not add a Rust toolchain or a WebRTC dependency. This first profile makes no
automatic latency ranking or speed claim; a matching policy is an explicit route
selection request.

## Lease, relay loss, and outcomes

The caller starts renewal through a fresh relay session. While renewal succeeds,
both endpoints renew their finite local lease. If relay access fails, the current
deadline remains unchanged: an already healthy direct route can still carry
in-scope request/reply traffic until that deadline, but it cannot renew itself,
change addresses, or create a replacement route. Restoring the same pinned relay
and reannouncing the agent permits a fresh-session renewal before expiry.

Withdrawal, expiry, direct failure, an identity or address change, and process
restart retire the generation. It cannot be revived by a late relay message or
an old lease. A request is never replayed onto another path. If a submitted
request loses its definitive reply, its outcome can be unknown even when a later
relay path is available.

## First-profile boundaries

- One reachable listener is required; there is no NAT hole punching.
- There are no configured backup relay sets, address-history scoring, or
  automatic route ranking.
- ARC does not resume a partially transferred byte stream.
- `git+arc://`, database-native sockets, browser proxies, and arbitrary TCP
  forwarding are outside this request/reply profile.

See [connection lifecycle](PROMOTION.md), [path selection](PATHS.md), and the
[carrier](CARRIER.md) for the broader design and future work.
