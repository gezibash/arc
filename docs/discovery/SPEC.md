# Discovery through relays

## Scope

A configured ARC relay provides identity lookup and capability search.
Citizens and providers announce themselves over the same relay connection
that carries ARC packets. A new client needs the relay address and its public
key; it does not need another citizen's local control files or a separate
directory address.

The same control connection also supports a relay-owned `status` query; see
[status](../status/SPEC.md). This query stays on the selected relay and does not
search or forward through federation.

Discovery includes participants on the **same relay** and opted-in publishers
on explicitly configured federation peers, including further relays reached
through permitted transit. See
[relay federation](../federation/SPEC.md) for operator setup and sharing
permissions. Partners synchronize signed service catalogs in the background.
Searches read the connected relay's local catalog once a partner has synchronized.
These records are temporary presence, not permanent identity registration or
proof that a provider has spare capacity. No relay promises a complete global
catalog.

## User flow

Use a build containing this protocol on both the relay and clients. From this
checkout, `mise run arc --` runs the current source CLI. Keep a persistent
relay identity as described in [deployment](../DEPLOY.md).

In each provider/client shell, set the same chosen relay address and its
public key. Replace the placeholders with the operator's actual values:

```bash
export ARC_RELAY='relay.example.com:7331'
export ARC_RELAY_PUBKEY='<relay-public-key-hex>'
```

With a provider identity active, run:

```bash
mise run arc -- serve go/cmd/files-provider
```

The running provider announces its signed identity and file-storage summary.
With a different citizen identity active on another computer:

```bash
mise run arc -- discover files
mise run arc -- info <provider-public-key> primary
mise run arc -- install <provider-public-key> primary
mise run arc -- files put ./report.pdf
mise run arc -- files get <file-id> --output ./restored.pdf
```

The existing install trust decision still applies. The citizen verifies the
signed capability package before installing the commands. File encryption
still happens locally; see [private files](../files/SPEC.md).

`discover` accepts `--limit` (up to fifty providers per page) and `--after`
using the returned cursor. An empty search lists providers offering services;
ordinary citizen announcements with no capabilities are excluded. Search
results and cursors are scoped to the connected relay's current view. Warm
searches, including empty results and subsequent pages, do not fan out to peers.
A cold catalog falls back to a bounded live search. Exact full-key lookups use
fresh cached entries when available; misses and name/prefix lookups retain live
lookup. Known incomplete views are marked partial. A cache can still lag a
distant provider change; partial=false is not a network-wide census.

Search replies include `cached: true` when served locally from the synchronized
catalog and `cached: false` for live federation results. `Arc.Net.discover_via_relay`
exposes this as `cached?`. Older local-only replies may omit the field.

The relay can run on the same computer for local use. Until the separate
default-delivery migration, commands without relay configuration retain the
older local mode. Once these commands choose a relay, discovery and their
peer calls do not fall back to local identity records or file mailboxes.

The `publish` and `resolve` control-plane commands still operate on the local
control store. Network presence is announced automatically by connected
agents (`serve`, `listen`, discovery, and tool calls); there is no need to copy
control-store files between machines.

## Transport and bootstrap

Directory requests use a distinct `ARC_DIRECTORY_V1` prefix inside the
existing length-framed relay connection. A client must complete the existing
identity handshake before a relay handles directory commands. This avoids
needing to resolve a directory service before identity lookup can work.

The requests are:

- `announce`: publish the authenticated identity's signed record.
- `resolve`: look up a name or public-key prefix among live records.
- `search`: find capability summaries with bounded pagination.

Correlated replies have bounded size and a timeout. A relay that does not
support directory requests produces an explicit discovery failure. Clients
must not interpret that failure as permission to use local state.

Directory records and search terms are **public metadata** carried in relay
control frames. They are not encrypted application messages. Normal ARC
request/reply packets retain end-to-end encryption. Private file names and
contents never belong in an announcement or directory query.

## Signed announcement

`Arc.Data.RelayAnnouncement` owns the interoperable record format. The default
local announcement is version 1; direct federation uses signed version 2,
and explicit network sharing uses signed version 3. These extensions are
described in the federation spec:

| Field | Meaning |
| --- | --- |
| `version` | Record format, currently `1` |
| `public_key` | Citizen's lowercase Ed25519 public-key hex |
| `capabilities` | Bounded public summaries, possibly empty |
| `issued_at` | Unix time in seconds |
| `expires_at` | Expiration time, at most 180 seconds after issue |
| `signature` | Lowercase hex signature by the citizen |

The signature covers `arc-relay-announcement-v1\n` followed by compact JSON
with recursively sorted object keys, excluding the signature field. Public
names are derived from the identity key, never supplied as separate aliases.
The signed record binds all capability fields. The record carries no X25519
key. A receiver computes the X25519 key of a citizen from its Ed25519 public
key.

Records are limited to 8 KiB and eight capability summaries. Receivers reject
bad signatures, unsupported fields, expired records, and issue times more
than thirty seconds in the future. Agents refresh announcements every minute.
Both the relay and the querying client verify records.

The relay accepts announcements only for the authenticated connection's
identity. Connection replacement, disconnection, and expiration invalidate
offers. Directory restart clears the directory; it is not a durable registry.
Disconnected clients must reconnect before announcing again.

## Trust and delivery boundaries

An announcement proves who published an offer; it does not prove capacity,
service quality, or the completeness of a relay's search results. Relay
operators can omit offers. Provider detail still uses the existing signed
capability package and local signer-trust checks.

No new decryption authority is granted to a relay. The X25519 key of a citizen
follows from its Ed25519 public key, so a directory cannot substitute a
different key for a known citizen. This does not make arbitrary search results
trusted providers.

Relay-backed agents send requests, replies, and events through their selected
transport, even if the recipient happens to run in the same process. Incoming
relay packets are handed to the local recipient after arrival. Loss of the
transport produces an error rather than a file-mailbox delivery attempt.
Successful socket submission is not a delivery receipt; offline queuing and
durable delivery remain separate work. Forwarding through approved additional
relay hops is implemented by federation when operators enable transit and
publishers choose network sharing.

A request timeout can occur after a provider has already performed an action.
Do not automatically retry arbitrary operations on that basis. Private file
objects support resubmitting the exact same envelope, but rerunning a file
upload creates a fresh encrypted object. After an uncertain upload, inspect
the provider's file list before uploading again.

## Validation

The test coverage includes signed records and real relay control frames.
The CLI integration launches provider and citizen in separate operating-system
processes with distinct local control stores and mailboxes. It exercises
discovery, signed installation, and private file upload/download through a
shared relay, and checks that neither child uses a local file mailbox.

The same-process regression checks relay forwarding during capability detail
requests, then disconnects the transport and verifies that resident peers and
local control entries cannot satisfy the request.

```bash
mix test apps/arc_cli/test/arc/cli_relay_discovery_test.exs apps/arc_cli/test/arc/cli_relay_mode_test.exs
```
