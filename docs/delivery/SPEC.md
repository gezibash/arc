# Delivery: ARC over Nostr events, on any transport

Status: phases 1 to 3 are built, see section 15. The rest is proposed. The older ARC
code uses its own protocol: Ed25519 keys, live sessions, and routed relays.
Section 14 lists what changes.

## 1. Purpose

ARC must survive the worst case. The internet can fail. A relay can go away.
Two citizens can have no live path between them for days. Data must still
move, and a citizen must still trust what arrives.

Three ideas make this possible:

1. **A unit that verifies itself.** Every datum is a signed Nostr event. A
   citizen checks the signature, so it does not matter who carried the event
   or how late it arrived.
2. **Store and forward.** A node keeps what it cannot deliver now. Other
   nodes carry sealed events for citizens that they cannot identify.
3. **Many transports.** A router sends each event over every transport that
   can carry it: relays, files, Bluetooth, and radio.

The ARC capability layer sits on top: manifests, installed commands, and
providers.

The delivery design follows bitchat, whose iOS source is in the public
domain. ARC ports the design to Go. It does not share bitchat's wire format.

## 2. Scope

This document defines:

- The identity of a citizen.
- The unit, and how a private unit is wrapped.
- The transports, and the frame that carries an event on a small link.
- The delivery layer: store, router, outbox, sync, couriers, and mesh relay.
- The contract between delivery and the capability layer.

These are out of scope, and have their own documents:

- The capability interface and its primitives.
- The journal, which becomes the first consumer of this layer.

## 3. Terms

| Term | Meaning |
| --- | --- |
| citizen | One identity. A person, an agent, or a provider. |
| node | The `arc` program of one citizen on one machine. |
| event | A signed Nostr event, as NIP-01 defines it. |
| rumor | An unsigned event, as NIP-59 defines it. |
| seal | A kind 13 event that holds one encrypted rumor. The author signs it. |
| gift wrap | A kind 1059 or 21059 event that holds one encrypted seal. A one-time key signs it. |
| private event | A gift wrap, together with the seal and the rumor inside it. |
| store | The events that one node keeps. |
| transport | A way to move events between two nodes. |
| live transport | A transport that can deliver an event now, while both nodes are present. |
| courier | A node that carries a private event for another citizen. |
| route tag | A short tag that names the recipient of a private event for one day. |
| provider | A citizen that answers calls to a capability. |

## 4. Layers

```text
capability layer    manifests · install · providers · calls        ARC
delivery layer      store · router · outbox · sync · couriers      ARC (design from bitchat)
unit                event, gift wrap (NIP-01, NIP-44, NIP-59)      Nostr
transports          relay | file | Bluetooth LE | LoRa             Nostr for relays, ARC for the rest
```

A layer uses only the layer below it. The capability layer never selects a
transport. The delivery layer never reads a private payload.

## 5. Identity

- A citizen is one secp256k1 key pair. Signatures are BIP-340 Schnorr, as
  NIP-01 requires.
- The public key is the address of the citizen. It is 32 bytes, shown as 64
  lower-case hex characters.
- A petname follows from the public key, as it does today. The derivation
  runs over the new 32-byte key.
- A citizen uses one key on every transport. ARC does not keep a second key
  for the mesh.

### 5.1 Migration from an Ed25519 key

A citizen with an ARC Ed25519 key moves to a new secp256k1 key once. The new
key publishes a migration record: an event of kind 10272, which is
replaceable.

```json
{
  "kind": 10272,
  "pubkey": "<new secp256k1 public key>",
  "content": "",
  "tags": [
    ["ed25519", "<old Ed25519 public key, 64 hex>"],
    ["proof", "<Ed25519 signature, 128 hex>"]
  ]
}
```

The Ed25519 signature covers these bytes:

```text
"ARC-MIGRATE-V1" || 0x00 || new public key (32 bytes) || old public key (32 bytes)
```

- The new key signs the event, so the record proves that the citizen holds the
  new key.
- The old key signs the proof, so the record proves that the old key agreed.
- A node that trusted the old key trusts the new key the same way: its
  petname, its signer trust, and its installed capabilities.
- If two records name different new keys for one old key, the node trusts
  neither. It reports both to the citizen, because this means that someone
  else holds the old key.

## 6. The unit

### 6.1 Events

Every datum that ARC moves is an event, as NIP-01 defines it.

A node verifies every event before it stores, forwards, or shows it:

1. Compute the event ID from the NIP-01 serialization.
2. Check that the ID matches the `id` field.
3. Check the Schnorr signature over the ID with the `pubkey` field.

If a check fails, the node drops the event, and does not forward it.

An event has no freshness window. It stays valid until its `expiration` tag
passes, as NIP-40 defines. An event without that tag stays valid.

The kind of an event sets how nodes keep it, as NIP-01 defines:

| Kind range | Class | Kept by nodes |
| --- | --- | --- |
| 1000–9999, and most below 1000 | regular | yes |
| 10000–19999 | replaceable | the newest for each author and kind |
| 20000–29999 | ephemeral | never |
| 30000–39999 | addressable | the newest for each author, kind, and `d` tag |

### 6.2 Private events

A private event follows NIP-59:

1. The author writes a rumor. The rumor holds the real content, the real
   time, and the real author.
2. The author encrypts the rumor to the recipient with NIP-44, and puts it in
   a seal. The author signs the seal.
3. The author encrypts the seal to the recipient with NIP-44, and puts it in
   a gift wrap. A new one-time key signs the gift wrap.

The recipient must check that the `pubkey` of the seal equals the `pubkey`
of the rumor. If they differ, the recipient drops the event. Without this
check, any author can impersonate another, as NIP-17 states.

The kind of the gift wrap sets how long it lives:

| Kind | Use |
| --- | --- |
| 1059 | Store and forward. Nodes keep it until it expires. |
| 21059 | Live. Nodes never keep it. It is useful only while the recipient is present. |

The author randomizes `created_at` on the seal and the gift wrap, up to two
days in the past, as NIP-17 describes. The real time stays inside the rumor.

### 6.3 Routing a private event

A gift wrap names its recipient with one routing tag. It has one of two
forms:

- **Relay form.** The tag is `p` with the recipient's public key. Standard
  Nostr relays and clients understand this form. Direct messages use it, so
  that ARC direct messages reach any NIP-17 client.
- **Courier form.** The tag is `w`, with a route tag as its value. Couriers
  and mesh nodes use this form, because a route tag does not reveal the
  recipient's key.

The letter `w` is unused in every NIP and in the Nostr kind registry. Relays
index single-letter tags, so a recipient finds its events with a filter on
`#w`.

### 6.4 Route tags

```text
route tag = first 16 bytes of HMAC-SHA256(key = recipient public key,
                                          message = "arc-route-v1" || 0x00 || UTC date as YYYY-MM-DD)
```

The node writes the route tag as 32 lower-case hex characters.

- A recipient checks the tag of each day that a wrap can still be alive on:
  the last 7 days, today, and tomorrow. Mail on a USB stick can take days, and
  tomorrow covers a clock that runs ahead.
- Tags for one recipient on two days do not match. A courier therefore cannot
  link two envelopes to one recipient across days.
- Any node that already knows the recipient's public key can compute the tag.
  A route tag hides the recipient from a courier that does not know them. It
  does not hide the recipient from an observer who looks for that one key.

## 7. Transports

### 7.1 The interface

Every transport gives the router the same information:

| Property | Meaning |
| --- | --- |
| live | The transport can deliver to a present node now. |
| frame size | The largest frame that one send can carry. |
| directed | The transport can send to one named node, not only to all nodes in reach. |
| cost | The price of one byte: battery, air time, or money. |

A transport does three things: it sends a frame, it receives frames, and it
reports which nodes it can reach now.

### 7.2 The four transports

| Transport | Live | Frame size | Directed | Notes |
| --- | --- | --- | --- | --- |
| relay | yes, while connected | the relay's limit | through tags | NIP-01 over WebSocket. |
| file | no | none | no | A directory: a USB stick, a shared folder, a disk. |
| Bluetooth LE | yes | about 469 bytes per fragment | yes | Mesh. Linux first. |
| LoRa | yes, slowly | 233 bytes per packet | yes | Mesh. Later. |

**Relay.** A node connects to relays over WebSocket, as NIP-01 defines. It
authenticates with NIP-42 where a relay asks. It finds where to send with
NIP-65 relay lists. It syncs with NIP-77 where a relay supports it.

**File.** A node writes events to a directory as JSON lines, one event on
each line. Another node reads the directory, imports each event that it does
not hold, and writes each event that the directory lacks. A person carries
the directory between the two nodes.

**Bluetooth LE.** Every node takes both roles at once: central and
peripheral. BlueZ supports both roles at once on Linux. ARC builds on the
Linux backend of `tinygo.org/x/bluetooth`, which reaches BlueZ over D-Bus, and
extends it to hold a GATT server and a GATT client together. ARC offers the
change upstream. On macOS, a node is a central only, so it reaches one hop.
See section 16.

**LoRa.** A node reaches LoRa through Reticulum. The node connects to a local
Reticulum instance over TCP, and that instance drives the radio. Reticulum
moves a payload larger than one radio packet itself, so the node sends each
compact event whole. See section 16.

### 7.3 The compact form

A small link cannot carry an event as JSON. A short event is 300 to 600 bytes
of JSON, and a LoRa packet holds 233 bytes. On Bluetooth LE and LoRa, a node
sends each event in a compact binary form:

```text
version       1 byte, value 1
pubkey        32 bytes
sig           64 bytes
created_at    unsigned varint
kind          unsigned varint
tags          varint count, then for each tag: varint count, then for each value: varint length, UTF-8 bytes
content       varint length, UTF-8 bytes
```

The compact form leaves out `id`. The receiver computes the ID from the
NIP-01 serialization of the other fields, then checks the signature. The form
therefore loses nothing that the signature covers.

### 7.4 The frame

On a mesh transport, a frame carries one event, or one fragment of an event:

```text
frame type       1 byte: event, fragment, sync, announce, handshake, session
hop limit        1 byte
fragment header  8-byte fragment ID, index, total, when the type is fragment
body             the compact event, a fragment of it, or a sync message
```

The hop limit lives in the frame, not in the event. A mesh node changes it
without changing the signed event.

A receiver joins fragments by fragment ID. It keeps at most 128 incomplete
events, and drops an incomplete event after 30 seconds. It refuses an event
larger than 1 MiB.

## 8. The node and its store

Each node keeps a store. The store is the source of truth for the node.
Transports write into it. The capability layer reads from it.

A node keeps these events:

| Class | Examples | Bound |
| --- | --- | --- |
| own | events that this citizen signed | none |
| addressed | private events for this citizen | none |
| carried | private events that this node carries as a courier | 40 events, see 10.4 |
| public | capability announcements, public posts | a quota that the citizen sets |

The store refuses an event that fails verification. It keeps one copy of each
event, by ID. It removes an event when its `expiration` tag passes.

## 9. What each party sees

| Party | Sees | Does not see |
| --- | --- | --- |
| relay | the one-time key, the routing tag, the size, the arrival time | the author, the content, the real time |
| mesh neighbour | the frame type, the size, the timing, the Bluetooth address | the author and content of a private event |
| courier | the route tag, the size | the recipient, unless it already knows their key; the author; the content |
| recipient | everything | — |
| provider | the caller and the call, because it is the recipient | — |

A public event, such as a capability announcement, is readable by everyone.

## 10. Delivery

### 10.1 The router

The router takes an event from the capability layer and gets it to its
recipient. For each event:

1. If a live transport reaches the recipient now, send the event over it.
2. Send the event to each relay in the recipient's relay list.
3. If no transport delivers now, put the event in the outbox.
4. If the event is private and can take the courier form, deposit it with
   couriers, see 10.4.

The router sends one event over more than one path. The receiver keeps one
copy, by ID. Redundancy therefore costs bytes, not correctness.

The router respects cost. If a transport costs more than the event is worth,
the router skips it. A citizen sets this policy per transport.

A live call, a gift wrap of kind 21059, takes only step 1 and step 2. If
neither delivers now, the call fails, and the router tells the caller.

The size of an event limits its paths:

| Size | Paths |
| --- | --- |
| up to 64 KiB | every path, couriers included |
| up to 1 MiB | relays, files, and sync between mesh neighbours; no couriers |
| over 1 MiB | relays and files only |

If an event cannot take any path that exists now, the router tells the caller
which path it needs.

### 10.2 The outbox

The outbox keeps each undelivered event until the recipient acknowledges it.

- It keeps at most 100 events for each recipient.
- A message lives for 7 days, set in the `expiration` tag of its wraps. After
  that, the outbox shows it as expired. The failure never stays silent.
- Each sync gives the wraps to the transport again, and counts the attempt.
  A relay gets each wrap once. On a live transport, the outbox tries at most
  8 times.
- The outbox holds wraps and the sender's own seal, which are ciphertext. The
  sender reads its own message by opening its seal, because a NIP-44
  conversation key is the same from both ends.

An acknowledgement is a private event whose rumor has kind 3274. The rumor names
the delivered rumor with an `e` tag. When the sender receives the
acknowledgement, the outbox removes the event. A node does not acknowledge an
acknowledgement.

### 10.3 Sync

When two nodes meet, they reconcile their stores for one filter at a time.
Sync uses the Negentropy protocol that NIP-77 wraps.

- Over a relay, the node uses NIP-77 where the relay supports it. Otherwise
  it sends a NIP-01 `REQ` with `since`.
- Over a mesh link, the nodes send the same Negentropy messages as binary, in
  frames of type `sync`.
- Over a file transport, the node reads the event IDs in the directory, and
  copies what each side lacks.

The khatru relay framework serves NIP-77, and the Go client library supports
it. Both now live in `fiatjaf.com/nostr`. The strfry relay supports it too.
ARC uses that one implementation on every transport.

A node syncs these filters, in this order:

1. Private events for its own route tags of yesterday, today, and tomorrow.
2. Private events that it carries, for the route tags that the other node
   owns.
3. Public events that it chose to keep.

### 10.4 Couriers

When no transport can deliver a private event now, other nodes carry its
courier form. How a courier bounds the spread depends on the transport.

**On a directory.** Anyone who reads a directory gets a copy, so a copy
budget cannot hold there. A hop limit bounds the spread instead:

- The sender writes the event with a hop limit of 3, in a file beside it.
- A node carries the event only when the limit it reads is above 0. It keeps
  the event with the limit one lower, and writes that lower limit.
- A node reads at most 3, whatever the file says, so a forged limit cannot
  spread an event further.
- A node that holds an event with a limit of 0 still writes it. The recipient
  takes it; no other courier does.

A directory does not say who wrote an event to it, so a courier cannot count
deposits for each node there. It keeps at most 40 carried events, and drops
the oldest past that.

**On a mesh link.** Two nodes on a mesh link know each other, so a copy
budget holds:

- The router deposits each event with at most 3 couriers.
- Each event carries a copy budget. The budget starts at 4, and is never more
  than 8.
- When one courier meets another, it gives half of its remaining budget to
  the other. Mail therefore spreads through a moving group, and the cap stops
  one event from flooding the network.
- A courier accepts at most 5 events from each contact, and at most 2 from
  any other verified node. It keeps at most 40 carried events, and at most 20
  of those from nodes that are not contacts.

**On both:**

- A courier accepts an event of at most 64 KiB. A larger event moves only over
  relays and files.
- On a mesh link, a courier that meets the recipient delivers the event and
  removes it. On a directory, a courier cannot know who reads it, so the event
  stays until it expires.
- A courier that meets a relay can post the courier form there. The recipient
  finds it by route tag.

The courier cannot read what it carries. It knows only the route tag.

### 10.5 Mesh relay

On a mesh transport, a node relays events to nodes that it does not reach
directly. It uses a controlled flood:

- An event starts with a hop limit of 7.
- Each relay lowers the hop limit by 1. If the node has 6 or more neighbours,
  it lowers the hop limit to at most 5.
- A node waits a random 10 to 220 ms before it relays. If a copy arrives from
  another node first, it cancels its relay.
- A node never sends a frame back on the link that it came from.
- A node remembers the IDs of the last 1,000 events for 5 minutes, and relays
  each one once.

### 10.6 Live links

Two nodes with a live mesh link open a Noise session with the XX pattern,
with Curve25519, ChaCha20-Poly1305, and SHA-256. The session authenticates
both nodes and gives forward secrecy on that link.

A live call over the mesh travels inside the session. Store-and-forward
events do not need the session, because their content is already sealed.

A live call over a relay has no forward secrecy. NIP-44 does not give it.

## 11. The capability layer on top

### 11.1 Announcement

A provider announces a capability with an addressable event of kind 30272:

- The `d` tag is the capability ID.
- The content is the manifest.
- The provider signs the event. The signature is the authorship of the
  manifest.

Announcements are public. Nodes keep them and sync them. A new version of the
manifest replaces the old one, because the event is addressable.

### 11.2 Discovery

`arc discover` reads announcements from the local store and from relays.
Search terms match the `t` tags of an announcement, and full text where a
relay supports NIP-50.

### 11.3 Install and trust

`arc install <provider>` reads the announcement, verifies it, and asks the
citizen to trust its author once. The installed commands come from the
manifest, as today.

### 11.4 Calls

A call is a private event. The request is a rumor of kind 3272. The reply is
a rumor of kind 3273. The reply names the request rumor with an `e` tag.

The manifest declares the class of each command:

| Class | Gift wrap | Behaviour |
| --- | --- | --- |
| store and forward | 1059 | The router delivers it on any transport, however late. The reply returns the same way. |
| live | 21059 | The router needs a live path now. If none exists, the call fails at once. |

A provider keeps the IDs of the requests that it answered, and answers each
request once.

A call has no acknowledgement. The reply clears the caller's outbox. The
provider sends its reply again on each sync until the reply expires, so a
caller whose reply was lost still gets it.

A provider refuses a live request whose rumor is more than 5 minutes old.
This window applies to live calls only. A store-and-forward call has no
window, because it can travel for days.

A provider publishes its relay list with NIP-65. A caller sends each request
to the provider's read relays.

### 11.5 Data that a citizen keeps for itself

Some data belongs to one citizen only, such as a private journal. The citizen
seals it to its own key, and syncs it between its own nodes and its chosen
relays. No provider takes part. The data is a NIP-37 draft, and
`delivery/draft` makes it. See docs/interface/SPEC.md, section 7.2.

A relay takes a draft only from its author, because the draft carries the
NIP-70 tag. The relay transport answers the NIP-42 challenge of the relay with
the citizen's key. The store applies a NIP-09 deletion of an author to the
events of that author, and refuses them after that, so a stick or a relay
cannot bring a deleted event back.

## 12. Abuse limits

- Copy budgets are capped, so one event cannot amplify itself through
  couriers.
- Couriers limit deposits for each node, and keep room for contacts.
- The hop limit caps the reach of a flood.
- Each store has a quota for public events.
- A one-time key signs each gift wrap, so a relay cannot limit abuse by
  author. An ARC relay therefore asks for NIP-42 authentication, or for
  NIP-13 proof of work, before it accepts a gift wrap.

## 13. Security properties

| Property | Holds | Why |
| --- | --- | --- |
| Authorship | yes | Every event is signed. The seal of a private event is signed by its author. |
| Integrity | yes | The ID covers every field, and the signature covers the ID. |
| Confidentiality of content | yes | NIP-44 inside a seal inside a gift wrap. |
| Recipient hidden from relays | no, in relay form | The `p` tag names the recipient. |
| Recipient hidden from couriers | partly | See 6.4. |
| Forward secrecy | live mesh links only | Noise gives it. NIP-44 does not. |
| Replay | harmless | A node keeps one copy of each event, by ID. A provider answers each request once. |
| Key loss | fatal | Nothing recovers a lost key. |

## 14. What changes in ARC

| Today | After |
| --- | --- |
| `identity` (Ed25519) | secp256k1 keys from `fiatjaf.com/nostr` |
| `sealedbox`, `session`, `packet`, `frame`, `internal/wire` | NIP-44 and NIP-59 |
| `announce` | capability announcement events |
| `relay` with routes and federation | relays built on khatru, and the NIP-65 outbox model |
| `client` | the node: store, router, and transports |
| `direct` | left out of the first version, see section 16 |
| `citizen` provider runtime | moved to `provider/host`, which both stacks use: a provider still runs as a process over standard input and output |
| `capability`, `toolbox`, installed commands | kept; the manifest travels in an announcement |
| `cmd/dm-provider` | NIP-17 direct messages; no provider needed |
| `cmd/journal-provider` | data that the citizen keeps for itself; no provider needed |
| `cmd/agora-provider` | public events on a relay; no provider needed |
| `cmd/exec-provider`, `cmd/sqlite-provider`, `cmd/releases-provider` | kept, answering calls |

## 15. Phases

Each phase ends with its proof. A phase that does not pass its proof does not
merge.

Phases 1 and 2 are built: the packages under `delivery/` and the command
`arcn`. `mise run delivery` runs both proofs. The journal of phase 1 is now the
journal manifest of the capability interface.
Phase 2 adds `delivery/private` for gift wraps and route tags, and
`delivery/mail` for the outbox, acknowledgements and couriers. Sync compares
sets with Negentropy when a relay lists NIP-77 in its information document,
and fetches every event otherwise.

Phase 3 adds `delivery/catalog` for announcements, discovery and installs,
and `delivery/call` for both classes of call. A live call subscribes, waits
until the relay has taken the subscription, and only then sends, all on one
connection, because a relay never stores the ephemeral reply. On a local relay,
a live call to `exec` takes about 10 ms for the round trip. `arcn call` calls
a capability with a raw body. The capability interface, docs/interface/SPEC.md,
now declares the commands of a capability. The journal adds one thing that
the phase names: a page travels as parts of at most 32 KiB, so it fits the
event limit of common relays, and `arcn journal tail` streams text as it is
appended.

| Phase | Scope | Proof |
| --- | --- | --- |
| 1 | Identity, events, the store, the router, and the relay and file transports | Two machines sync a journal through a relay, then through a USB stick. A changed event is refused. |
| 2 | The outbox, acknowledgements, route tags, couriers, and sync | A message reaches an offline recipient through a third machine that carries a USB stick. |
| 3 | The capability layer: announcements, discovery, install, and both classes of call; direct messages on NIP-17 | A live call to `exec` succeeds over a relay, and the round-trip time is recorded. A store-and-forward call crosses the courier path. An ARC direct message opens in a NIP-17 client. |
| 4 | Bluetooth LE on Linux, the compact form, fragments, the mesh relay, and Noise links | Three Linux nodes in a line pass a message from one end to the other. The two end nodes are out of each other's reach. |
| 5 | LoRa through a local Reticulum instance | A message crosses two LoRa nodes with no internet. |

## 16. Decisions

### 16.1 Kind numbers

"272" spells ARC on a phone keypad. No NIP and no entry in the Nostr kind
registry uses these numbers:

| Kind | Class | Use |
| --- | --- | --- |
| 3272 | regular | a call request, inside a gift wrap |
| 3273 | regular | a call reply, inside a gift wrap |
| 3274 | regular | an acknowledgement, inside a gift wrap |
| 3275 | regular | one continuation part of sealed content longer than 32 KiB |
| 10272 | replaceable | a migration record, see 5.1 |
| 30272 | addressable | a capability announcement |

Relays never see 3272, 3273 or 3274, because a gift wrap hides them. Sealed
data, such as a journal page, a KPI series or a file, is a NIP-37 draft of
kind 31234, with checkpoints of kind 1234 and a relay list of kind 10013. A
part of kind 3275 carries only content past the first 32 KiB of a draft. See
docs/interface/SPEC.md, section 7.2. ARC registers its six kinds in the
registry.

### 16.2 The route tag

The route tag uses the letter `w`. The letters `b`, `j`, `o` and `w` are unused
in every NIP. The registry also uses `v`.

### 16.3 Bluetooth from Go

`muka/go-bluetooth` is archived, so ARC does not use it. ARC extends the Linux
backend of `tinygo.org/x/bluetooth`, which is maintained and reaches BlueZ over
D-Bus. If the change does not fit that library, ARC calls BlueZ through
`godbus/dbus` directly.

### 16.4 macOS

A macOS node is a Bluetooth central only in the first version. The
CoreBluetooth bindings under the Go library aim to cover all of CoreBluetooth,
which includes the peripheral manager. A spike after phase 4 decides whether a
macOS node can become a full mesh node.

### 16.5 LoRa

ARC uses Reticulum, not Meshtastic:

- Reticulum is a network stack. It runs over LoRa, serial links, packet radio,
  TCP, UDP and I2P, and it moves payloads larger than one packet.
- Meshtastic is firmware for chat radios. Its packets hold 233 bytes, and it
  runs its own flood. ARC would be a guest on it.

Three Go implementations of Reticulum exist. Only one drives a LoRa radio, and
one person maintains it. ARC therefore connects to a local Reticulum instance
over TCP, and does not embed a Go port. The Python instance works today. A Go
port can replace it later without a change in ARC.

### 16.6 Sync

ARC uses Negentropy from `fiatjaf.com/nostr`, as section 10.3 states. A relay
without NIP-77 gets a `REQ` with `since`.

### 16.7 Courier size

Couriers keep the 64 KiB cap. Section 10.1 lists the paths for larger events.
A capability announcement is public, so it moves by sync, not by couriers.

## 17. Deferred work

| Item | Why it waits | When to decide |
| --- | --- | --- |
| The TLS direct carrier as a live transport | Relays can carry live calls: about 10 ms for a round trip on a local relay. | After a round trip is measured through a public relay. |
| A full macOS mesh node | It needs a peripheral backend in Go. | After phase 4, see 16.4. |
| A bridge to bitchat direct messages | bitchat's Nostr envelopes are not NIP-17. Only the citizen's own node can translate them, because translation needs the private key. | When ARC direct messages must reach bitchat users. |
