# Relay federation

## Implemented scope

Operators connect explicitly approved relay partners. Citizens discover and use
services through those partners and, when intermediaries permit it, through
additional relays. Application traffic stays on ARC relay connections.

```text
citizen <-> relay A <-> relay B <-> relay C <-> relay D <-> provider
                         transit     transit
```

A does not need a direct partnership with C or D. Each adjacent pair approves
one another. B and C must enable onward forwarding. The provider must choose
network sharing. Existing direct-only and local-only publications keep their
original scope.

Relays maintain temporary local catalogs through background partner exchange.
Connecting a partner can expose services further into the connected network,
subject to sharing permissions, operator policy, and the limits below. Relays
do not automatically form new peer connections. Catalogs converge over time;
they are not a complete global registry.

## Operator setup

Run the updated source or a build containing network federation on participating
relays and clients. From this checkout, `mise run arc --` runs current source.
Generate a persistent identity on each relay machine with `keys gen`.
Exchange only public keys and relay addresses through a channel the operators
trust. Keep secret keys on their original machines.

Example chain A–B–C:

```bash
# A approves B.
arc relay --key <relay-A-key-name> --port 7331 \
  --peer <relay-B-public-key-hex>@relay-b.example.com:7331

# B approves A and C, and explicitly allows traffic between partners.
arc relay --key <relay-B-key-name> --port 7331 --transit \
  --peer <relay-A-public-key-hex>@relay-a.example.com:7331 \
  --peer <relay-C-public-key-hex>@relay-c.example.com:7331

# C approves B.
arc relay --key <relay-C-key-name> --port 7331 \
  --peer <relay-B-public-key-hex>@relay-b.example.com:7331
```

Replace the placeholders with real values. `--peer` is repeatable, up to
sixteen distinct peers. Self-peering and duplicate identities are rejected.
`ARC_RELAY_KEY` can supply the persistent identity instead of `--key`.

`--transit` defaults to off. It permits onward discovery and application
traffic between configured partners for network-shared publishers. It does
not widen any publisher's signed permission. Endpoint relays need not enable
transit to let their own citizens use distant services or publish them.
There are no per-partner transit filters or usage billing in this release.

The relay with the lower public key initiates each connection; its partner
must be reachable from it. Both endpoints then send over the same authenticated
link. Citizens and providers need no incoming public port. Federation uses the
existing relay TCP listener.

To remove a partner or revoke transit, change the startup configuration and
restart that relay. Live configuration reload is not implemented.

## Provider and citizen setup

A provider on C chooses its publication scope:

| Provider flags | Visibility |
| --- | --- |
| No federation flag | Its own relay |
| `--federate` | Its own relay and direct partners |
| `--federate-network` | Its own relay and the permitted wider network |

The federation flags are mutually exclusive. Both require a configured relay
and its pinned public key. They apply only to `serve` and `listen`.
A listener publishes an identity, not a storage capability.

```bash
# Provider on C:
export ARC_RELAY='relay-c.example.com:7331'
export ARC_RELAY_PUBKEY='<relay-C-public-key-hex>'
arc serve cmd/files-provider --federate-network

# Citizen on A, in a separate shell or machine:
export ARC_RELAY='relay-a.example.com:7331'
export ARC_RELAY_PUBKEY='<relay-A-public-key-hex>'
arc discover files
arc info <provider-public-key-hex> primary
arc install <provider-public-key-hex> primary
arc files put ./report.pdf
arc files get <file-id> --output ./restored.pdf
```

Citizens need no publication opt-in to call a shared service or receive its
replies. Existing signed-package installation and signer-trust checks still
apply. File contents and names are encrypted on the citizen's machine; see
[private files](../files/SPEC.md).

## Signed publication authority

The publisher signs the sharing scope and its home relay identity:

| Version | Scope | Signature domain |
| --- | --- | --- |
| 1 | Local; no home field | `arc-relay-announcement-v1\n` |
| 2 | `"federation":"direct"` | `arc-relay-announcement-v2\n` |
| 3 | `"federation":"network"` | `arc-relay-announcement-v3\n` |

Versions 2 and 3 include `relay_public_key`, the home relay's lowercase
public-key hex. All other fields, limits, and canonical signing rules follow
[relay discovery](../discovery/SPEC.md).

A home relay accepts a publication only from its authenticated publisher and
rejects a mismatched home identity. Version 1 never leaves that relay.
Version 2 can cross only the direct partner connection. Only version 3 can
be re-exported and carried over an onward route. Relays cannot upgrade these
signed scopes. Automatic refresh preserves the publisher's chosen scope.

## Authenticated links

`ARC_FEDERATION_V1` frames use the existing length framing. The client
challenge proves the connecting relay's identity; a fresh challenge and signed
server response prove the other side and bind its X25519 key. Each side
computes that key from the Ed25519 public key of the other side.

All later controls and forwarded traffic use signed, encrypted ARC packets,
bound to the particular connection with increasing sequence numbers. Direct
forwarding remains wire kind 3. Routed forwarding uses kind 5. Older relays
remain useful as direct partners but cannot act as network intermediaries.

## Cached discovery

Each relay keeps an in-memory catalog of verified signed announcements and
routes learned from approved partners. A ready authenticated link starts
synchronization immediately. Relays poll partners every two seconds for changes,
with at most four catalog requests running at once and one per partner.
Catalog work uses the existing encrypted federation request/reply channel.
It never performs a recursive search inside a catalog request.

After a partner view is synchronized, ordinary searches read local records and
the cached partner views. Empty searches and pagination also stay local.
A complete public-key lookup can use a fresh cached record directly. A cold
catalog, missing identity, or name/prefix lookup retains the bounded live lookup
below. Search replies identify cached results with `cached: true`.

Only signed, unexpired publications enter the catalog. Local-only publications
stay at home; direct sharing reaches direct partners. Re-exporting a network
publication requires transit consent. An advertised route begins with the
sending partner, ends at the publisher's signed home, contains no repeated or
receiving relay, and crosses at most eight connections. A relay does not export
an entry to a partner already present in that entry's path.

Providers refresh their signed records as before. Changed records and
withdrawals propagate during background synchronization, without citizen
searches. Connection replacement, publisher departure, expiry, and partner loss
invalidate the affected cached routes. A late live-search response cannot
reinstall routes after the catalog has changed. Existing conversations retain
their original route and reply permissions.

The catalog is temporary and resynchronizes after restart. A relay identifies
its catalog incarnation with a fresh random epoch. Immutable, paginated
snapshots establish a partner view atomically; revisioned deltas update it.
An epoch change or unavailable delta history causes a fresh snapshot. Invalid
or failed exchanges remove that partner's imported view. The catalog never
stores private files or application payloads.

Catalog requests use `type: "catalog"`, `version: 1`, and `mode: "snapshot"`
or `"delta"`. They carry an epoch and per-partner revision; snapshot continuation
also carries a token and public-key `after` cursor. Epochs and tokens are random
16-byte values encoded as lowercase hex. Absent wire fields use JSON null.
Replies use `type: "catalog_reply"` and echo their mode, epoch, and revision.
Snapshot pages contain signed `records`, a `routes` map, a token, `next`, and
`truncated`. Deltas contain `base_revision` and a contiguous sequence of
`upsert` or `withdraw` events. A snapshot with `reset: true` can replace an
outdated delta request. Each partner has its own export revision and bounded
history, so changes for another partner cannot create gaps.

The catalog retains at most 10,000 imported candidates and a separate shared
pool of at most 10,000 staged snapshot candidates. Duplicate routes count
against these limits. A snapshot page contains at most 50 records, with a
220 KiB record/route budget; delta replies also stay within 220 KiB. History
retains the latest 256 events. Older revisions and oversized deltas restart
with bounded snapshot pages.

Partner views expire after 30 seconds without a successful refresh. A snapshot
token expires after 60 seconds without a valid continuation. Signed record
expiry still applies during transfer. Capacity omissions remain marked partial;
affected source views retry a full snapshot after 30 seconds so unchanged
listings can return when space becomes available. A failed or expired snapshot
restarts from the beginning.

Known missing or truncated partner views mark local search results partial.
Downstream synchronization status is not recursively treated as a global
completeness claim: that would never converge around cycles. Even a currently
synchronized cache can lag a distant update. `total` counts the available page
candidates; neither it nor `partial: false` proves network-wide completeness.

## Bounded live lookup

A cold search or unresolved lookup creates a random query identifier.
The request's `network` object carries:

| Field | Meaning |
| --- | --- |
| `id` | 16 random bytes encoded as lowercase hex |
| `path` | Unique relay public keys already visited, ending in the sender |
| `budget` | Remaining query allocations, including the receiving relay |

The origin starts with a budget of 64 including itself. Each forwarding relay
spends its own allocation and divides the remainder among unvisited partners.
The allocations across branches cannot exceed the incoming budget.
A path may cross at most 8 relay connections. A relay validates the last
visited identity against the authenticated sender, rejects paths containing
itself, and suppresses repeated query identifiers for 12 seconds.

Replies include signed provider records and a `routes` map from provider
public key to the ordered path from the responding peer to the provider's home.
The path must begin at the authenticated partner and end at the signed home.
It cannot overlap the request's visited path or exceed the hop bound.
Only network records may have onward paths. These routes are statements by
the immediate authenticated partner, not independent cryptographic proof of
every intermediate operator. A partner can omit or misroute results; it cannot
forge provider publications or decrypt the application payload.

Search aggregates concurrent replies, chooses shorter available paths for
duplicate identities, and returns a bounded page in public-key order.
Each uncached page is a fresh query. `total` counts available candidates, not
every provider in the network.

Unavailable branches, invalid replies, duplicate-query suppression, and
exhausted budgets mark the result partial. Downstream partial status propagates
to the citizen. A relay with transit disabled defines a policy boundary and
returns only its own eligible publishers.

A partial lookup by name or prefix fails with `federation_unavailable`,
because an unseen branch could contain another match. A lookup for a complete
public key may succeed when that exact signed identity was returned, even in
a partial search. Missing exact identities still fail when results are partial.
Use complete public keys from discovery when calling services across a partial
network.

Searches are asynchronous at each relay. They have at most 32 concurrent local
jobs, at most 16 peer branches per job, and decreasing per-hop deadlines.
The client directory timeout for search/lookup is 10 seconds, and the citizen
connect call allows 11 seconds for that result; announcements retain their
2-second timeout. Recursive caller or peer departure cancels owned work.
A failed branch cannot reinstall stale routes or erase successful branches.

## Routed delivery and private replies

A network request wraps the original signed, encrypted citizen/provider packet
with an ordered relay path and cursor. The wrapper also carries the publisher's
signed network announcement. The compact kind-5 body contains:

```text
version:u8, mode:u8, cursor:u8, path_count:u8,
packet_bytes:u32be, record_bytes:u16be,
path:32*path_count bytes, inner_packet, announcement_json
```

Version is 1. Mode 1 is a request; mode 2 is a reply. A path contains 2–9
distinct relay keys. The cursor identifies the receiving relay. Every receiver
checks its position and the preceding authenticated peer. Intermediate relays
require transit consent and an approved next partner. The cursor advances
without changing the inner packet.

Every request hop verifies the provider's signature, network permission, and
home binding. The destination also checks its current live publication:
revoking network sharing blocks new requests even if another relay retained
an older signed record or an earlier conversation.

Each hop records a temporary permission bound to the citizen, provider,
session identifier, and full route. Endpoint permissions are also bound to
the original local connection. Replies carry the reversed path and no
publication record; each hop must have the matching conversation permission.
A changed path, unrelated session, or replacement local connection cannot
reuse it. Ordinary citizens do not enter a remote provider directory.

Permissions expire after 180 seconds of inactivity. A direct partner's
disconnect removes affected routes and conversations. Imported announcements
expire with their signed lifetimes. More distant failures propagate through
background catalog withdrawals or cause in-flight calls to time out. There is no transparent
rerouting of an existing conversation.

Legacy direct forwarding still delivers only to local recipients with direct
sharing permission or an existing direct reply permission. It cannot be used
to bypass the network wrapper and transit rules.

## Privacy and operating limits

Relays cannot decrypt inner citizen/provider application payloads. They can
observe communicating identities and traffic patterns. Route metadata and
service announcements are visible to participating relays. Federation is not
an anonymity system. Private file providers receive encrypted objects;
ordinary compute providers retain their existing trust requirements.

Limits include 10,000 imported records, 10,000 direct return permissions,
10,000 network conversations, and 10,000 recently seen queries per relay.
Transport callbacks are separately capped at 32 globally and 4 per peer.
Federation controls are bounded to 256 KiB; directory pages reserve overhead
within that bound. Routed inner packets are limited to 8 MiB and publication
records to 8 KiB. Smaller operator frame caps apply to the complete outer
frame, including encryption and routing overhead.

These are resource bounds, not evidence of internet-scale performance.
There is no persistent offline queue or end-to-end delivery receipt.
Socket acceptance can precede a later timeout, including after a provider
has performed a mutation. ARC does not automatically retry mutations.

## Validation

The original direct-link integration remains in place. Network tests use
separate OS processes with independent keys, control files, and mailboxes.

A four-relay A–B–C–D chain tests discovery, installation, and an exact binary
private-file round trip across B and C. Local-only and direct-only providers
on D remain hidden from A. Restarting B without transit revokes onward access.
A cyclic topology exercises bounded discovery and deduplication.

Catalog integration tests exercise cached searches, empty cached results,
withdrawals, same-identity relay restart, and withdrawal convergence around a
cycle. Socket tests check background synchronization, local search replies,
bounded task scheduling, and rejection of late results after peer departure.
The lifecycle tests also verify that stopping a federation manager cancels an
outbound handshake and closes its socket without waiting for the peer timeout.

To diagnose a test that hangs, run it with the race detector and a timeout.
The stack dump names the goroutine that did not finish:

```bash
go test -race -timeout 60s -run Federation ./relay/...
```

Unit and socket tests cover signed scopes, route/home binding, malformed
paths, loop and budget limits, downstream partial replies, private return
permissions, local connection replacement, expiry, and peer departure.

```bash
go test ./relay/...
```
