# Bluetooth implementation plan

ARC will let nearby devices exchange events without Wi-Fi or internet. Two
reachable devices can communicate directly. A nearby ARC node can also carry
messages between devices that cannot reach each other directly.

This plan follows the current Go delivery design in [SPEC.md](SPEC.md). The
shared transport interface already exists, so extracting another transport
interface is not the first step.

## Current status: PRs 1 and 2

`delivery/compact` implements the compact event format from section 7.3 of the
specification. Think of it as packing the same signed message into a smaller
envelope. It preserves the public key, signature, timestamp, kind, tags and
content. The receiver rebuilds the event ID from those fields.

`delivery/frame` now wraps those compact bytes for a small link. It splits
large events into numbered pieces and rebuilds them when they arrive, including
out-of-order arrivals. It bounds unfinished work, expires missing pieces after
30 seconds, and passes completed bytes back to the compact decoder. See
[section 7.4](SPEC.md#74-the-frame) for the exact byte format and limits.

Run `mise exec -- go test ./delivery/compact ./delivery/frame -v` to check both
layers without Bluetooth hardware. Tests cover frame sizes, reordered and
duplicate pieces, conflicting data, expired assemblies, memory limits and
signed-event round trips. A fuzz check is available with
`mise exec -- go test ./delivery/frame -fuzz=FuzzDecode -fuzztime=30s`.

Neither package turns on Bluetooth, finds devices, connects to a relay or
sends messages. Existing transports behave as before. Bluetooth is not yet a
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
| 4 | Secure direct Bluetooth sessions | Two nearby nodes authenticate a session and exchange events using the planned Noise handshake. |
| 5 | Mesh forwarding and a room relay | A third node forwards messages between two nodes that cannot reach each other, with hop limits and duplicate suppression. |
| 6 | ARC node and discovery integration | Bluetooth becomes an ARC transport; received events enter the verified store and capability announcements can travel over it. |
| 7 | Capability requests and durable delivery | A client calls a nearby provider over Bluetooth; live calls fail clearly when unreachable, while eligible durable messages use the outbox and acknowledgements. |
| 8 | macOS connection support | A Mac connects as a BLE central to supported Linux nodes; full Mac peripheral support needs a separate feasibility check. |
| 9 | Configuration, packaging and hardware guide | Users can enable Bluetooth, understand permissions and diagnose connection failures. |
| 10 | Automatic direct-to-approved-relay fallback | An optional Bluetooth routing policy tries the recipient directly, then an approved nearby forwarding node when the direct path fails. |

PR 10 is explicitly separate from basic forwarding in PR 5. It must define how
a relay is approved, how failure is detected and how retries preserve event
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
