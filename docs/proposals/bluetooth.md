# Bluetooth implementation plan

Status: proposed

ARC will let nearby devices exchange events without Wi-Fi or internet. Two
reachable devices can communicate directly. A nearby ARC node can also carry
messages between devices that cannot reach each other directly.

This plan follows the current Go delivery design in [the delivery spec](../delivery/SPEC.md). The
shared transport interface already exists, so extracting another transport
interface is not the first step.

## Current status: PRs 1–3 (radio hardware validation pending)

`core/compact` implements the compact event format from section 7.3 of the
specification. Think of it as packing the same signed message into a smaller
envelope. It preserves the public key, signature, timestamp, kind, tags and
content. The receiver rebuilds the event ID from those fields.

`core/frame` now wraps those compact bytes for a small link. It splits
large events into numbered pieces and rebuilds them when they arrive, including
out-of-order arrivals. It bounds unfinished work, expires missing pieces after
30 seconds, and passes completed bytes back to the compact decoder. See
[section 3.10](../delivery/SPEC.md#310-the-frame) for the exact byte format and limits.

Run `mise exec -- go test ./core/compact ./core/frame -v` to check both
layers without Bluetooth hardware. Tests cover frame sizes, reordered and
duplicate pieces, conflicting data, expired assemblies, memory limits and
signed-event round trips. A fuzz check is available with
`mise exec -- go test ./core/frame -fuzz=FuzzDecode -fuzztime=30s`.

`adapters/transport/ble` and the separate `arc-ble-probe` command now provide
an experimental Linux BlueZ discovery and byte-round-trip test. See the
[Linux hardware guide](BLUETOOTH-LINUX.md). Automated checks and cross-builds
are not a substitute for complete hardware validation. A user-reported iPhone
to Raspberry Pi echo test passed; the remaining radio checks are listed in the guide. ARC messages and
capability calls are not connected to Bluetooth yet.

The compact and frame packages do not turn on Bluetooth, find devices, connect to a relay or
send messages. Existing transports behave as before. Bluetooth is not yet a
usable ARC transport.

The encoder accepts an event whose ID matches its fields. The decoder checks
message structure and reconstructs its ID. Neither operation authenticates
the sender: received events must still pass through the store's signature
verification before use. Compact encoding is not encryption.

The format uses shortest-form unsigned base-128 integers (Go's `Uvarint`),
UTF-8 text and byte lengths. One encoded event may be at most 1 MiB. The decoder
rejects unknown versions, truncated messages, invalid text, overflowing or
nonminimal integers, out-of-range timestamps and kinds, and trailing bytes.
It preserves tag order, repeated tags, empty tags and empty strings.

Tests cover a fixed wire example, signed-event round trips, store rejection of
a corrupted signature, malformed input, size limits and fuzzed input.

## Small, reviewable PRs

Each row is a separate proposed PR. Later rows remain planned; their exact
scope can be refined as hardware testing reveals constraints.

| PR | Change | What we can demonstrate afterward |
| --- | --- | --- |
| 1 | Compact event encoding and decoding | A signed event becomes smaller and is reconstructed without changing its identity or signature. |
| 2 | Mesh frames and message fragments | Large messages split into small pieces and reassemble with size, count and timeout limits. |
| 3 | Linux Bluetooth discovery and connections | Two Linux devices find the ARC service and exchange test bytes over BLE. |
| 4 | Secure direct Bluetooth event transport | Implement the existing `transport.Transport` contract, plus `transport.Live` for subscriptions, using compact events, fragments and the planned secure session. Received events enter ARC's verified store. |
| 5 | Normal ARC commands, providers and direct-delivery proof | Enable Bluetooth through normal startup/configuration; remove relay-only assumptions in shared paths; prove private messages and a live capability request/reply between two real devices with relay fallback disabled. |
| 6 | Mesh forwarding and a room relay | After the direct-delivery acceptance gate passes, a third node forwards messages with hop limits and duplicate suppression. |
| 7 | Durable Bluetooth delivery | Extend eligible durable messages through the existing outbox and acknowledgements; verify retries preserve event identity and do not repeat capability operations. |
| 8 | macOS connection support | A Mac connects as a BLE central to supported Linux nodes; full Mac peripheral support needs a separate feasibility check. |
| 9 | Packaging and expanded hardware guide | Broaden installation, permissions and troubleshooting guidance beyond the setup required for PR 5. |
| 10 | Automatic direct-to-approved-relay fallback | An optional Bluetooth routing policy tries the recipient directly, then an approved nearby forwarding node when the direct path fails. |

This order supersedes the earlier plan that placed mesh before normal ARC
integration. PRs 4 and 5 are small parts of one next milestone: usable direct
ARC communication. A secure byte session alone does not complete that milestone.
Specification section 7.1 now describes the actual Go event contract; frame
sizes and radio discovery remain adapter details rather than a second public
transport interface. The radio probe stays a diagnostic.

### Direct-delivery acceptance gate (before PR 6)

- Reuse ARC's event verification, private-content encryption, mail and call
  behavior. Connect compact encoding and fragment reassembly to those paths;
  define usable payload sizes, bounded buffering and fragment expiry scheduling.
- Implement `Name`, `Send` and `Fetch` with documented remote query behavior,
  cancellation and errors. Implement `Watch` for live subscriptions. The probe's
  internal `radio` interface remains only a diagnostic testing boundary.
- Integrate normal CLI startup and provider paths. In particular, `watchAll`
  currently asserts `relay.Relay`; audit related watches and calls and use the
  appropriate shared contracts without changing relay-specific discovery rules.
- Test two real devices using normal ARC commands, with relay fallback disabled
  and no internet delivery path. Record hardware, OS, BlueZ versions, build
  revision, commands and observed results. A phone echo app cannot prove this gate.
- Show signed-event delivery and rejection of invalid signatures, private content
  readable only by the intended recipient, and disconnect/reconnect and retry
  results that preserve message identity without duplicate processing.
- Demonstrate a live capability request/reply through the normal provider path,
  including a clear unreachable-peer failure. Use an offline-capable provider.
  Any deferral of live calls must be explicit; it cannot count as a passed test.

Record evidence in the Linux hardware guide and the relevant PR. Automated
checks complement these hardware results; neither a probe echo nor a three-node
mesh demonstration substitutes for this direct ARC workflow.

PR 10 remains separate from basic forwarding in PR 6. It must define how a
relay is approved, how failure is detected and how retries preserve event
identity. Retrying delivery must not recreate or repeat a capability operation.
The policy must fit the existing router, which supports sending over multiple
paths; it must not silently replace that behavior or enable internet fallback.

## Why operating-system support is needed

ARC cannot operate the Bluetooth radio by encoding messages alone. The
operating system controls scanning, advertising, connections and permissions.
On Linux this is normally BlueZ. On Apple platforms it is CoreBluetooth. ARC
needs an adapter that talks to those system facilities.

A BLE **service** is a named group of Bluetooth data endpoints, identified by
a UUID. It lets another device recognize an ARC connection and find where to
write or receive bytes. It is different from an ARC **capability**, such as a
provider offering a tool. Capability requests travel inside ARC events over
the Bluetooth connection.

A BLE **central** scans and initiates connections. A **peripheral** advertises
its service and accepts connections. The Linux implementation needs both
roles for mesh participation. The first macOS stage is central-only, following
the delivery specification.

## What a room relay means

Here, a room relay is an ARC node that forwards mesh messages. It is not
necessarily a Nostr WebSocket relay, and Bluetooth does not automatically
make an existing WebSocket relay accessible. A relay machine must run the
Bluetooth adapter and forwarding logic to participate.

Direct communication needs no relay. Forwarding extends reach only when each
individual Bluetooth link can connect. Consuming a capability also requires
its provider to be reachable and the capability itself to work offline; a
Bluetooth link cannot make an internet-dependent service work without internet.

The mesh acceptance test uses three Linux devices: A reaches B, B reaches C,
and A cannot reach C directly. A signed event must reach C through B, retain
its identity, and be processed only once. Hardware tests, disconnection tests
and permissions checks belong to the relevant later PRs.
