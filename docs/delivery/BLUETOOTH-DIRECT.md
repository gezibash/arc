# Direct Bluetooth event adapter

Stage 4 is an experimental adapter component. Normal ARC commands do not select
it yet. Stage 5 must supply configuration, lifecycle and the two-device CLI proof
before Bluetooth is described as usable. See the
[implementation plan](../proposals/bluetooth.md).

## What this adds

`adapters/transport/ble.OpenDirect` opens one Linux BlueZ connection. It checks
the remote ARC identity and returns a `transport.Transport`, `transport.Live`
and `call.Exchanger` implementation. It uses the existing compact and frame
packages, core signature verification, and an injected core event store.
It does not implement another application session protocol.

A caller supplies a `DirectConfig`:

- `Signer`: the existing ARC signer, including context-aware signing.
- `Peer`: the exact expected ARC public key, obtained through a trusted channel.
- `Store`: the local core store, owned by the caller.
- `Share`: an explicit policy for events the peer may fetch or watch. It has no
  default. Do not share self data or private records merely because they exist
  in the local store. Configure this policy before opening the connection.
- `Adapter`: the BlueZ adapter, with `hci0` as the default.
- `Address`: the remote Bluetooth address for the central. Leave it empty on
  the peripheral, which advertises and waits for the first connection.

A Bluetooth address selects a radio endpoint. Only the ARC identity proof
establishes trust. Both endpoints must name each other's ARC public key. The
caller context owns the link lifetime. Close the adapter before closing its store.
Linux needs BlueZ, a powered BLE adapter, and D-Bus permission to register a
GATT service or connect to a device. Other operating systems return an error.
The existing `arc-ble-probe` remains unchanged and uses a different service.

## Supported operations and limits

- `Send` transfers one signed event. Stored events pass through `Store.Save`.
  Ephemeral events pass core verification and require a matching local live
  subscription; they are never persisted. An acknowledgement means acceptance
  by this endpoint, not successful processing of a capability or mail receipt.
- `Fetch` queries the remote store. It checks at most 65 matching candidates and
  refuses a query with more than 64. Narrow the filter when this occurs. The
  share policy is applied before the caller's result limit. No silent partial
  batch is returned. The receiving core still owns sync and mail behavior.
- `Watch` takes one subscription per link lifetime. It returns after the peer
  has accepted the subscription and queued the initial permitted events. It
  then receives matching events sent through this link. Writes to the injected
  store through unrelated adapters do not generate notifications here.
- `Exchange` installs a watch before sending a live request. A failure after
  submission begins is uncertain; it never causes automatic replay or failover.
  A new exchange needs a new link after the one lifetime watch has been used.
- One outbound request runs at a time. Each has a 30-second cap, subject to an
  earlier caller deadline. Cancellation after submission closes the link.
- There are 66 outbound message slots, 65 reply slots and 64 watched-event slots.
  A slow consumer or excess queued work closes the connection explicitly.
- An event's JSON is limited to core's 64 KiB limit. An encoded wire message is
  at most 256 KiB. Record buffers are at most 4,098 bytes. Each GATT direction
  has at most 256 queued packets of 20 bytes.

These limits are for a first direct adapter, not a general mesh or a multiplexed
provider connection. Stage 5 must address multiple subscriptions required by
normal mail/provider composition, repeated calls, and explicit error reporting
when a watch closes. `Direct.Err` exposes the terminal cause. `Fetch` does not
claim reconciliation. Durable retry, forwarding and macOS support are absent.

## Experimental wire profile

The service UUID is `8f71c110-5b9a-4cf0-a8d1-3e290da1b301`. Its read/write
characteristic is `8f71c111-5b9a-4cf0-a8d1-3e290da1b301`. The central writes
acknowledged GATT requests and polls reads for queued peripheral packets. An
empty read means no packet is available. Reads consume a packet. Packet loss
or duplication ends the encrypted link; it does not resume an uncertain stream.
This conservative profile uses 20-byte packets and needs no larger MTU.

Each record starts with a two-byte big-endian body length and ends at a GATT
packet boundary. Its body is at most 4,096 bytes. The first three records carry
Noise XX messages with Curve25519, ChaCha20-Poly1305 and SHA-256. The prologue is
`arc-ble-direct-v1`. Each connection generates fresh Noise static keys; those
keys are not themselves ARC identities.

After Noise completes, each endpoint signs a connection-local kind-1 event with
its existing ARC signer. The event has no tags. Its content is exactly:

```text
arc-ble-direct-v1:<hex Noise channel binding>:<initiator or responder>:<recipient ARC public key hex>
```

The initiator sends its proof first. The responder verifies the expected author,
signature, content, role and transcript binding before returning its own proof.
The initiator then performs the same checks. Proof events are encrypted inside
the Noise connection and are never published, saved, or dispatched as messages.
No new Nostr event kind is introduced. The transcript prevents proof replay
across connections, and the recipient and role prevent reflection.

After the handshake, records are Noise ciphertexts. A plaintext message begins
with its four-byte big-endian JSON length. The plaintext stream is split across
records of at most 4,080 plaintext bytes. The JSON has `op`, optional `id`,
`filter`, `frames`, and `error` fields. `frames` contains base64 byte arrays from
`core/frame` with a 1,024-byte frame limit and zero forwarding hops.

Operations are `send`/`ack`, `fetch`/`result`/`end`, and `watch`/`event`/`end`.
`unwatch` clears the remote subscription; the public API instead closes the link
on watch cancellation to prevent reuse of stale events. Each request has a
positive connection-local ID. Fragments are assembled within one bounded
message; incomplete messages are discarded with the connection. Partial records
and messages have a 30-second budget once their first data arrives. Idle watches
can wait until canceled. Malformed records, identity mismatches and authentication
failures close the link. There is no downgrade to the echo profile or relay.

The implementation uses [flynn/noise](https://github.com/flynn/noise) v1.1.0 and
its [channel binding](https://github.com/flynn/noise/blob/v1.1.0/state.go).
Private ARC content continues to use core's existing encryption. Noise adds
forward secrecy for this direct link; it does not change application session
semantics or promise forward secrecy across future intermediate nodes.

An observer can see Bluetooth addresses, connection timing and packet sizes.
An authenticated peer sees events explicitly sent or permitted by the share
policy. An untrusted radio peer can occupy the single admission slot until the
30-second setup timeout. This profile needs security review before production use.

## Evidence and the remaining acceptance gate

`go test -race ./adapters/transport/ble` exercises two adapter endpoints through
an in-memory ordered packet link. Tests cover signed fragmented events reaching
the core store, refusal of invalid signatures, remote fetch, private delivery,
watch readiness and cancellation, ephemeral delivery, wrong identities,
ciphertext corruption, and a request/reply through the real `core/call` server.
The provider program is replaced at core's existing provider boundary in that test.

These tests do not exercise BlueZ or a radio. Stage 4 has no new real-device
results. The earlier iPhone/Pi echo test does not validate this secure profile.
Before completing Stage 5, normal ARC commands must run on two devices with
relay fallback disabled. Record device/OS/BlueZ/build versions, signed/private
event delivery, live calls, disconnect/retry, cancellation and restart. Neither
a probe nor an in-memory test substitutes for that evidence.
