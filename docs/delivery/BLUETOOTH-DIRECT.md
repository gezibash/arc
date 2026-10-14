# Direct Bluetooth event adapter

Status: **Integrated, verification pending.** Stage 5 connects the secure
adapter to normal ARC commands on Linux. Automated tests use a simulated radio;
no two-device test has yet proved this profile over BlueZ. See the
[Linux test guide](BLUETOOTH-CLI.md) and [implementation plan](../proposals/bluetooth.md).

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
- `Watch` takes up to 16 independent subscriptions per link. It returns after the peer
  has accepted the subscription and queued the initial permitted events. It
  then receives matching events sent through this link. Writes to the injected
  store through unrelated adapters do not generate notifications here.
- `Exchange` installs a watch before sending a live request. A failure after
  submission begins is uncertain; it never causes automatic replay or failover.
  It releases its subscription when the exchange finishes; later calls reuse the link.
- One outbound request runs at a time. Each has a 30-second cap, subject to an
  earlier caller deadline. Cancellation after submission closes the link.
- There are 66 outbound message slots, 65 reply slots and 64 event slots per subscription.
  A slow consumer or excess queued work closes the connection explicitly.
- An event's JSON is limited to core's 64 KiB limit. An encoded wire message is
  at most 256 KiB. Record buffers are at most 4,098 bytes. Each GATT direction
  has at most 256 queued packets of at most 512 bytes.

A canceled established watch closes only its own channel. Connection loss closes
all watches; core ends the affected sessions. Canceling a pending wire request
still closes the connection because its outcome may be uncertain. `Direct.Err`
exposes the terminal cause. `Fetch` does not claim reconciliation.

`Listen` accepts successive connections from the selected citizen. It starts
advertising when the first local watch starts, after `arc serve` has saved its
announcement. Disconnect ends the old watches. Normal provider/mail loops open
new watches on a new connection; they do not resume or replay live calls.
No peer within 30 seconds starts a new bounded admission attempt. A central that
drops during setup has sent no ARC data, so the listener waits for its next
connection. Configuration and identity errors are reported; restart after
correcting them.

Each side listens for the BlueZ disconnect signal, so a listener is ready for
the next connection before the central can dial again. A one-second poll stays
as a fallback. Within the 30-second setup limit, a central tries a new
connection after a radio failure. It never retries a wrong identity: when the
responder closes the link after the initiator's proof, the initiator reports an
identity error.

Mesh, a forwarding relay, self-data/bulk transfers, macOS and automatic fallback
are not supported. Existing core mail can retain and acknowledge direct mail;
Stage 7's broader durable-delivery work remains separate.

## Experimental wire profile

The service UUID is `8f71c110-5b9a-4cf0-a8d1-3e290da1b301`. Its write/indicate
characteristic is `8f71c111-5b9a-4cf0-a8d1-3e290da1b301`. The central enables
indications, then writes acknowledged GATT requests. The peripheral sends its
packets as indications, one at a time: it waits for the central's confirmation,
which BlueZ reports through the characteristic's `Confirm` method, before the
next. Neither side polls. Packet loss or duplication ends the encrypted link; it
does not resume an uncertain stream.

A packet is at most the negotiated ATT MTU minus 3 bytes, between 20 and 512
bytes. The central reads the MTU from the characteristic's `MTU` property; the
peripheral reads it from the `mtu` option of each write. Both need BlueZ 5.62
or later; an unknown MTU keeps 20-byte packets. Each side chooses the size it
sends, and a receiver accepts any packet up to 512 bytes.

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
`sub`, `filter`, `frames`, and `error` fields. `frames` contains base64 byte arrays from
`core/frame` with a 1,024-byte frame limit and zero forwarding hops.

Operations are `send`/`ack`, `fetch`/`result`/`end`, and `watch`/`event`/`end`.
`watch` and its snapshot `event` messages carry a positive `sub` ID, distinct
from the request ID. `unwatch` removes that subscription. IDs are never reused
within a connection. Late snapshot packets for canceled subscriptions are discarded. Each request has a
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
30-second setup timeout. BlueZ does not name the device that enables
indications, so another connected central can receive the same indications.
It sees only Noise ciphertext, as a radio observer does. This profile needs
security review before production use.

The profile string stays `v1` while Bluetooth is unreleased. A wire change
before the first release updates both endpoints together; a change after it
needs a negotiated profile, see the reserved handshake frame in the delivery spec.

## Evidence and the remaining acceptance gate

`go test -race ./adapters/transport/ble` exercises two adapter endpoints through
an in-memory ordered packet link. Tests cover signed fragmented events reaching
the core store, refusal of invalid signatures, remote fetch, private delivery,
watch readiness and cancellation, ephemeral delivery, wrong identities,
ciphertext corruption, and a request/reply through the real `core/call` server.
The provider program is replaced at core's existing provider boundary in that test.

`TestBluetoothCLI` builds the normal CLI with a test-only Go overlay replacing
OS radio I/O by ordered Unix-socket packets. Two separate homes discover and
install an echo service, make repeated calls on successive connections, deliver
private mail and sync. A local HTTP trap detects unintended relay access. The
production binary has no socket option or test environment-variable hook.

These tests do not exercise BlueZ or a radio. There are no new real-device results. The earlier iPhone/Pi echo test does not validate this secure profile.
Before completing Stage 5, normal ARC commands must run on two devices with
relay fallback disabled. Record device/OS/BlueZ/build versions, signed/private
event delivery, live calls, disconnect/retry, cancellation and restart. Neither
a probe nor an in-memory test substitutes for that evidence.
