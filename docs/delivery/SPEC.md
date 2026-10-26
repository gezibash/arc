# Delivery: ARC over Nostr events, on any transport

- Status: partial
- Layers: core, adapters, runtime
- Owns: core/keys, core/private, core/store, core/node, core/mail, core/kv, core/transport, core/compact, core/frame, core/relaylist, core/call
- Proof: go test -count=1 -run '^(TestDelivery|TestSeed|TestDevicePages|TestSelfDataBelongsToTheCitizen|TestSelfDataNeedsADeviceOnTheList|TestADeviceReadsSealedDataWithAToken|TestASendFollowsANewerRelayList|TestPendingMailGoesToARelayListThatAppearsLater|TestAWaitingAcknowledgementGoesToARelayListThatAppearsLater|TestASendWithNoAnswerUsesTheStoredList|TestAnnounceReplacesAnAnnouncementOfTheSameSecond)$' ./internal/proof/ ./core/store/ ./adapters/relay/sealed/ ./core/mail/ ./cmd/arc/
- Remaining: phases 5 and 6 of section 8.1
- Unverified: Phases 5 and 6 of section 8.1 have no proof. TURN of section 4.12 has a two-machine run behind NAT on the relay of Fly.io (docs/direct/SPEC.md section 8.2), and no automated test between two real NATs. Section 8.3 lists the deferred work. No test types the words or the passphrase of a seed on a real terminal.

Phases 1 to 4 are built, see section 8.1. Phases 5 and 6 are not built.
Phase 4 made this layer the only ARC stack in v0.11.0. The older stack used
its own protocol: Ed25519 keys, live sessions, and routed relays. Section 7.2
lists what replaced each part of it.

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

This document defines:

- The identity of a citizen.
- The unit, and how a private unit is wrapped.
- The transports, and the frame that carries an event on a small link.
- The delivery layer: store, router, outbox, sync, couriers, and mesh relay.
- The contract between delivery and the capability layer.

These are out of scope, and have their own documents:

- The capability interface and its primitives.
- The journal, which becomes the first consumer of this layer.

## 2. Terms

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
| seed | The 12 or 24 words of BIP-39 that derive a citizen key, as NIP-06 defines. The paper backup of the citizen key. |

## 3. Rules

### 3.1 Layers

```text
capability layer    manifests · install · providers · calls        ARC
delivery layer      store · router · outbox · sync · couriers      ARC (design from bitchat)
unit                event, gift wrap (NIP-01, NIP-44, NIP-59)      Nostr
transports          relay | file | Bluetooth LE | LoRa             Nostr for relays, ARC for the rest
```

A layer MUST use only the layer below it. The capability layer never selects
a transport. The delivery layer never reads a private payload.

### 3.2 Identity

- A citizen is one secp256k1 key pair. Signatures are BIP-340 Schnorr, as
  NIP-01 requires.
- The public key is the address of the citizen. It is 32 bytes, shown as 64
  lower-case hex characters.
- A petname follows from the public key, as it does today. The derivation
  runs over the new 32-byte key.
- A citizen uses one key on every transport. ARC does not keep a second key
  for the mesh.

A new citizen key comes from a seed:

- `arc keys gen` MUST make a seed of 24 words from 256 bits of entropy, as
  BIP-39 defines.
- The citizen key is the NIP-06 key of the seed: the BIP-32 key at the path
  `m/44'/1237'/0'/0/0` of the BIP-39 seed.
- The BIP-39 passphrase is empty by default. A citizen can add a passphrase.
  The same words with another passphrase give another key.
- A node MUST show the words once, when it makes them. It MUST NOT store the
  words, and MUST NOT write them to standard output, which scripts read.
- To derive a key from words, a node MUST accept 12 or 24 words from the
  English list of BIP-39, and MUST refuse a bad checksum. Case and the spaces
  between the words do not change the key.
- A node MUST NOT read the words from its command arguments, because the
  shell history and `ps` show arguments.
- The key file holds the derived secret key, as before. The key file does not
  record that a seed derived the key.
- No other key comes from the seed. A key from before the seed has no seed.
  Its paper backup is an `ncryptsec` of NIP-49.

### 3.3 Events

Every datum that ARC moves is an event, as NIP-01 defines.

A node MUST verify every event before it stores, forwards, or shows it:

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

### 3.4 Private events

A private event follows NIP-59:

1. The author writes a rumor. The rumor holds the real content, the real
   time, and the real author.
2. The author encrypts the rumor to the recipient with NIP-44, and puts it in
   a seal. The author signs the seal.
3. The author encrypts the seal to the recipient with NIP-44, and puts it in
   a gift wrap. A new one-time key signs the gift wrap.

The recipient MUST check that the `pubkey` of the seal equals the `pubkey` of
the rumor. If they differ, the recipient drops the event. Without this check,
any author can impersonate another, as NIP-17 states.

The kind of the gift wrap sets how long it lives:

| Kind | Use |
| --- | --- |
| 1059 | Store and forward. Nodes keep it until it expires. |
| 21059 | Live. Nodes never keep it. It is useful only while the recipient is present. |

Each gift wrap carries a NIP-40 `expiration` tag. An ARC relay refuses an
event whose expiration has passed. When the expiration of a stored event
passes, the relay stops serving the event and deletes it.

The author randomizes `created_at` on the seal and the gift wrap, up to two
days in the past, as NIP-17 describes. The real time stays inside the rumor.

One rumor can go to several recipients. The author seals and wraps it to
each recipient separately. A message of kind 14 names each recipient with a
`p` tag, and has at most 10 participants, with the author. The recipient of
a message MUST refuse it if no `p` tag names the recipient. [The group
messages spec](../groups/SPEC.md) defines the rules.

#### Replies

A reply is a message of kind 14 that names its parent with an `e` tag, as
NIP-17 defines it.

- A reply MUST hold exactly one tag `["e", "<parent id>"]`, with the rumor
  id of the parent in 64 lowercase hex digits. arc writes no relay hint and
  no marker.
- A sender MUST refuse a reply to a message that it does not hold, and a
  reply whose members are not the members of the parent.
- A reader takes the second element of the one `e` tag, and ignores the
  elements after it. A message with no `e` tag, more than one, or a value
  that is not 64 lowercase hex digits is a plain message. A reader MUST NOT
  refuse it.
- The tag is part of the rumor, so it is part of the rumor id and of what
  the seal encrypts. A relay or a courier cannot change it. The sender can
  name any id, so a reader shows the parent only from its own store
  ([the client programs spec](../clients/SPEC.md), section 3.10).
- The tag adds 73 bytes to the JSON of the rumor, inside the limit below.

`TestAReplyNamesItsParent`, `TestASendReplyChecksItsParent` and
`TestReplyToReadsOneETag` in `core/mail` prove these rules.

#### The size of mail

Mail is one gift wrap of kind 1059. The author MUST NOT split it into parts.
The limits apply to the rumor in each gift wrap of kind 1059: a message
(kind 14), a deferred call (3272), a reply (3273) and an acknowledgement
(3274). They are protocol limits, not relay settings.

- The content of the rumor MUST be at most 24,576 bytes (24 KiB).
- The JSON of the rumor, without a signature, MUST be at most 28,672 bytes
  (28 KiB). JSON escapes and tags make the JSON larger than the content, so
  this limit bounds the gift wrap.
- A sender MUST refuse a rumor above a limit before it seals the rumor.
  Larger content goes over a direct path, such as the transfer app.
- If the reply to a deferred call is above a limit, the provider MUST send
  an error reply in its place. It MUST NOT drop the reply.

These limits give the largest gift wrap. NIP-44 pads a plaintext to a
multiple of a chunk: 4,096 bytes from 16,385 to 32,768 bytes, and 8,192
bytes from 32,769 to 65,536 bytes. It adds 67 bytes: a version byte, a
32-byte nonce, a 2-byte length and a 32-byte MAC. Base64 gives 4 bytes for
each 3. A `created_at` and an `expiration` have 10 digits until the year
2286.

| Step | Bytes |
| --- | --- |
| Rumor JSON, padded | 28,672 |
| Seal content, base64 of 67 + 28,672 | 38,320 |
| Seal JSON, with 343 bytes of fields | 38,663 |
| Seal JSON, padded | 40,960 |
| Gift wrap content, base64 of 67 + 40,960 | 54,704 |
| Gift wrap JSON, relay form, with 445 bytes of fields | 55,149 |
| With room for one tag that admits the wrap: a stamp tag of 485 bytes, larger than a `nonce` tag of 38 | 55,634 |

The courier form is 32 bytes smaller. The content of the gift wrap stays
below 65,535 bytes. The Bolt event store holds at most that much content,
and so do common relays that cap an event at 64 KiB. The next padding step,
a rumor of 32,768 bytes, gives 65,628 bytes of content.

A relay MUST refuse a gift wrap of kind 1059 whose JSON holds more than
55,634 bytes. The room above 55,149 bytes holds the one tag that admits a
wrap from a stranger: a NIP-13 `nonce` tag, or a stamp
([the strangers spec](../strangers/SPEC.md), section 3.5). A relay setting can make this limit smaller, and never
larger. `core/private` holds these limits as `MaxMailContent`, `MaxRumor`
and `MaxWrap`.

An older ARC client takes a message of up to 32,768 bytes. Its store
cannot keep a gift wrap with more than 65,535 bytes of content, so it
fails to send a rumor of more than 28,672 bytes. Each gift wrap that it
sends fits the relay limit. It can send a message of 24,577 to about
28,400 bytes, which a newer client refuses to send. A recipient opens such
a message, because the limits bind senders and relays. A relay refuses a
larger gift wrap from other Nostr software.

These tests prove the limits: `TestTheLargestMailFitsTheWrapLimit` and
`TestMailRefusesOneByteMore` in `core/private`,
`TestADeferredReplyOverTheLimitArrivesAsAnError` in `core/mail`,
`TestTheRelayRefusesAWrapLargerThanMail` in `adapters/relay/limits`, and
`TestAFullMessageCrossesTheRelay` in `cmd/arc`.

#### The size of an event

One limit applies to each event that nodes keep: regular, replaceable and
addressable. It is a protocol limit, not a relay setting.

- The JSON of such an event, in the relay form, MUST be at most 65,536
  bytes (64 KiB). A gift wrap of kind 1059 has the smaller limit of mail.
- A relay MUST refuse a larger event. A relay setting can make this limit
  smaller, and never larger.
- A sender MUST refuse a larger event before it signs the event. Section
  4.7 gives the rule for an announcement.

An ephemeral event has no protocol limit, because no node keeps it. A
relay setting can cap it. A live reply travels in one gift wrap of kind
21059, and can be larger than 64 KiB.

There is no separate limit on content. The JSON of an event holds its
content, with escapes, and at least 300 bytes of other fields. An event of
65,536 bytes thus holds less than 65,535 bytes of content, which the Bolt
event store holds. Common relays also cap an event at 64 KiB.

The largest announcement of an app in this repository holds 10,083 bytes:
the journal app, with a manifest of 8,147 bytes. A draft of 32 KiB, with up
to 100 parts, holds 55,082 bytes. `core/store` holds this limit as
`MaxEvent`.

An older ARC relay has no limit by default, so it takes each event that a
newer client sends. An older ARC client does not check the size of an
announcement before it signs it. A newer relay refuses such an event above
65,536 bytes with an `invalid:` message, and the older client reports that
the event was not sent. Its own store cannot keep an event with more than
65,535 bytes of content, so only an event with escapes or large tags is
refused by a newer relay and kept by an older client. A reader of any
version reads each event that a relay serves.

These tests prove the limit: `TestTheRelayRefusesAnEventAboveTheLimit` in
`adapters/relay/limits`, `TestAnEphemeralEventHasOnlyTheFlag` in
`adapters/relay/limits`, and `TestAnAnnouncementAboveTheLimitIsRefused` in
`runtime/catalog`.

### 3.5 Routing a private event

A gift wrap names its recipient with one routing tag. It has one of two
forms:

- **Relay form.** The tag is `p` with the recipient's public key. Standard
  Nostr relays and clients understand this form. Direct messages use it, so
  that ARC direct messages reach any NIP-17 client.
- **Courier form.** The tag is `w`, with a route tag as its value. Couriers
  and mesh nodes use this form, because a route tag does not reveal the
  recipient's key.

The letter `w` is unused in every NIP and in the Nostr kind registry. The
letters `b`, `j`, `o` and `w` are unused in every NIP; the registry also uses
`v`. Relays index single-letter tags, so a recipient finds its events with a
filter on `#w`.

### 3.6 Route tags

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

### 3.7 Kind numbers

"272" spells ARC on a phone keypad. No NIP and no entry in the Nostr kind
registry uses these numbers:

| Kind | Class | Use |
| --- | --- | --- |
| 3272 | regular | a call request, inside a gift wrap |
| 3273 | regular | a call reply, inside a gift wrap |
| 3274 | regular | an acknowledgement, inside a gift wrap |
| 3275 | regular | one continuation part of sealed content longer than 32 KiB |
| 3276 | private rumor | core session frames, inside live wraps of kind 21059; see [sessions](../sessions/SPEC.md) |
| 4272 | regular | a journal entry, inside a NIP-37 draft; see [interface](../interface/SPEC.md), section 3.9.3 |
| 10272 | replaceable | reserved, and not used, see section 7.1 |
| 10273 | replaceable | the sealed storage list; see [storage](../storage/SPEC.md), section 3.8 |
| 10274 | replaceable | the sealed device list; see [devices](../devices/SPEC.md) |
| 10275 | replaceable | the sealed data roots; see [devices](../devices/SPEC.md), section 3.5 |
| 20274 | ephemeral | the token of a device; see section 4.13 and [devices](../devices/SPEC.md), section 3.6 |
| 30272 | addressable | a capability announcement |

Relays never see 3272, 3273 or 3274, because a gift wrap hides them. Sealed
data, such as a journal page, a KPI series or a file, is a NIP-37 draft of
kind 31234, with checkpoints of kind 1234. A part of kind 3275 carries
only content past the first 32 KiB of a draft. It stays on the device and on
storage citizens, and an ARC relay refuses it (section 4.11). ARC does not
make the private relay list of NIP-37, kind 10013. See
docs/interface/SPEC.md, section 3.9. ARC registers its kinds in the
registry. [The kind registry](../KINDS.md) lists every kind that ARC uses.

### 3.8 Transports

For implementation and review, follow the
[transport implementation contract](TRANSPORTS.md). It defines the current Go
interfaces, integration requirements, and evidence for a usable transport.

The Go contract in `core/transport/transport.go` moves signed Nostr events,
not radio frames:

| Method | Meaning |
| --- | --- |
| `Name()` | Names the transport for reports. |
| `Send(ctx, event)` | Gives one signed event to the transport. |
| `Fetch(ctx, filter)` | Returns a `Batch` of the events that match a filter. A batch counts unreadable items and can carry hop data. |

`transport.Live` adds `Watch(ctx, filter)`. It returns the matching stored and
new events while the context is active. The optional `Carrier` and
`Reconciler` interfaces add carried hop limits and event-set reconciliation.
The store verifies each received event, whatever its transport.

Bluetooth must implement these contracts. Compact encoding, frames, fragment
sizes and radio reachability belong to the adapter. They are not methods of
the shared interface. The table below describes transport design, not fields
of the Go contract. The normal command and provider paths must accept the
shared interfaces before Bluetooth is usable there. Direct delivery and its
proof on real devices come before mesh forwarding.

| Transport | Live | Frame size | Directed | Notes |
| --- | --- | --- | --- | --- |
| relay | yes, while connected | the relay's limit | through tags | NIP-01 over WebSocket. |
| file | no | none | no | A directory: a USB stick, a shared folder, a disk. |
| Bluetooth LE | yes | about 469 bytes per fragment | yes | Mesh. Linux first. Not built. |
| LoRa | yes, slowly | 233 bytes per packet | yes | Mesh. Later. Not built. |

**Relay.** A node connects to relays over WebSocket, as NIP-01 defines. It
authenticates with NIP-42 where a relay asks. It finds where to send with
NIP-65 relay lists, see section 4.10, and sends a direct message to the
NIP-17 relay list of its recipient. It syncs with NIP-77 where a relay
supports it.

A session keeps one WebSocket connection to each of its relays. Each call
of the session uses this connection: sends, queries, NIP-77 sessions,
watches and live calls. The session fetches the NIP-11 document of a relay
once. When the session ends, it closes the connection. If a connection was
idle for 10 seconds, the next call first asks the relay for one answer. If
no answer comes within 5 seconds, the call opens a new connection. A call
to an indexer, or to the relay of another citizen, opens a connection of
its own.

A network can lose the packets of one address family, often IPv6. If the
TCP connect, the TLS handshake or the WebSocket handshake to a relay does
not end within 4 seconds, the node tries once more over the other address
family. For the life of the process, the node remembers each relay host
that IPv4 reached, and tries IPv4 first for it. For any other host, the
node races both families, IPv6 first, and starts IPv4 after 300 ms.
`TestACallMovesToIPv4WhenIPv6Stalls` and
`TestSyncUsesOneConnectionPerRelay` prove this.

One relay that never answers MUST NOT stall a command when another relay
answers (#439). A pull asks each relay at once. After the first answer, it
waits at most 500 ms for the others, and then stops them. When a relay of
the session did not connect, each send, query, NIP-77 session or live call
to it in the next 30 seconds fails at once with the same error, in this
process and in each other process of the same home: the home keeps the
time in `relays-down.json`, by the wall clock. Only the relay's own timeout
or a dial that failed marks it down, never a refusal or the deadline of the
caller. A relay that took the connection, and then gave no EOSE, OK or
CLOSED within its own timeout, is down in the same way, unless it answers a
probe at once, the event of no ID, within 5 seconds: a relay that ignores
one kind is up, and only that call fails. A first attempt that raced both
address families and reached neither within 4 seconds ends the connect: the
host is behind a black hole. `arc sync` first asks each relay that probe,
all at once, so that relays that give no answer cost one timeout in all. It
names each relay that gave no answer in one line at its end, with its
reason: a refused connection, a host that does not exist, or no answer. A
file that is missing, does not parse or cannot be written marks nothing,
and two processes that write it at once leave a whole file. A watch
still tries to connect, so a provider watches again as soon as its relay
is back. A relay that gave no answer in time counts as unreached: a command
that works with what the machine holds says so in one line (#396). An older arc asks the relays one at a time,
and waits for each one up to its timeout. `TestASilentRelayStallsAPullOnlyForTheGrace`,
`TestARelayThatDidNotConnectFailsAtOnce`, `TestARefusalIsNotDown`, `TestARelayThatConnectsAndNeverAnswersIsDown`, `TestARelayThatIgnoresOneKindIsUp`, `TestABlackHoleHostIsTriedOnce`, `TestSyncWaitsForAMuteRelayOnce`, `TestPendingMessagesWaitForAMuteRelayOnce`, `TestABrokenFileOfDownRelaysIsNoRecord`, `TestDownRelaysOfManyProcessesKeepAWholeFile` and `TestOfflineJournalIsQuiet`
prove this.

A citizen publishes two relay lists to its relays: NIP-65, kind 10002, and
the NIP-17 list, kind 10050. Each list names every relay of the citizen. A
citizen does not publish the private NIP-37 list, kind 10013, because no
relay holds its drafts. `arc relay add`, `arc relay rm`, and
`arc serve` publish them. `core/relaylist` holds the NIP-65 list.

A citizen can also name indexer relays with `arc relay add <url> --index`.
An indexer holds only relay lists. The citizen publishes its NIP-65 and NIP-17
lists there. A one-time key answers the NIP-42 challenge of an indexer, so the
indexer does not learn who looks up a list. `arc` names no indexer by default.

**The lookup of a NIP-17 list.** Before each send to a recipient, the
citizen MUST ask its relays and its indexers for the NIP-17 list of the
recipient, for at most 5 seconds, and MUST use the newest list. The list in
the store can be old: a recipient who moves to other relays publishes a new
list there. The mail goes to the relays of the sender and to the relays of
that list.

- If no relay or indexer answers in time, the citizen uses the list in its
  store, and tells the user:
  `no relay or indexer answered the lookup of the relay list of <name>: the mail goes to the relays of the list in your store, which can be old`.
- If no list is found, the mail waits on the relays of the sender only, and
  the citizen tells the user:
  `no relay list for <name> on your relays or indexers: the mail waits on your relays only; if <name> reads mail on other relays, add one of them with arc relay add --index <url>, and arc sync sends it there`.
  Two citizens with no relay and no indexer in common cannot find the list
  of the other.

**Waiting mail.** After the sync of its relays, `arc sync` and each tick of
`arc serve` look up the list of each recipient of a message or a request
that is not delivered and not expired, and of each acknowledgement or reply
that is not expired, and send its wraps to each relay of that list that
does not have them yet. Each wrap goes to a relay once. So an
acknowledgement that waited on the relays of its sender reaches the
sender of the message once the list is found, and the outbox shows the
message delivered. `arc sync` prints
`mail: sent <n> waiting messages to the relays that their recipients read`,
with `message` and `its` for one.

An older arc looks in its store first, and asks the relays only when the
store has no list. So it keeps sending to the old relays of a recipient who
moved, and it never sends waiting mail to a list that it learns later. Its
mail reaches a newer recipient only on the relays of the old list. Reason:
Cato's test of v0.36.1 (2026-10-19). A citizen on relay.republic.sh only and
a citizen on aurelia.republic.sh only exchanged no mail after they swapped
relays. `TestASendFollowsANewerRelayList`,
`TestPendingMailGoesToARelayListThatAppearsLater`,
`TestAWaitingAcknowledgementGoesToARelayListThatAppearsLater` and
`TestASendWithNoAnswerUsesTheStoredList` in `core/mail` prove this. Zim
accepted the lookup on each send and the sending of waiting mail on
2026-10-19.

**File.** A node writes events to a directory as JSON lines, one event on
each line. Another node reads the directory, imports each event that it does
not hold, and writes each event that the directory lacks. A person carries
the directory between the two nodes.

**Bluetooth LE.** Not built as an event transport. Every node takes both
roles at once: central and peripheral. BlueZ supports both roles at once on
Linux, if the controller supports them. The Linux radio probe calls BlueZ
through `godbus/dbus` directly, because its server needs the BlueZ write
context of each device. The probe keeps advertising active while it scans and
tests an outbound connection. [The Linux probe guide](BLUETOOTH-LINUX.md)
describes the test profile. Hardware validation is pending.
`muka/go-bluetooth` is archived, so ARC does not use it. On macOS, the first
implementation is central only, so a node reaches one hop. The CoreBluetooth
bindings under the Go library aim to cover all of CoreBluetooth, which
includes the peripheral manager. A spike after phase 5 decides whether a
macOS node can become a full mesh node.

**LoRa.** Not built. A node reaches LoRa through Reticulum. The node connects
to a local Reticulum instance over TCP, and that instance drives the radio.
Reticulum moves a payload larger than one radio packet itself, so the node
sends each compact event whole. ARC uses Reticulum, not Meshtastic:

- Reticulum is a network stack. It runs over LoRa, serial links, packet
  radio, TCP, UDP and I2P, and it moves payloads larger than one packet.
- Meshtastic is firmware for chat radios. Its packets hold 233 bytes, and it
  runs its own flood. ARC would be a guest on it.

Three Go implementations of Reticulum exist. Only one drives a LoRa radio, and
one person maintains it. ARC therefore connects to a local Reticulum instance
over TCP, and does not embed a Go port. The Python instance works today. A Go
port can replace it later without a change in ARC.

### 3.9 The compact form

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

- Version 1 uses shortest-form unsigned base-128 varints and UTF-8 byte
  lengths.
- Timestamps MUST fit a nonnegative signed 64-bit integer, and kinds MUST fit
  an unsigned 16-bit integer.
- An encoded event is limited to 1 MiB.
- Decoders MUST reject trailing bytes and malformed fields.
- Decoding reconstructs the ID; the store still verifies the signature before
  accepting the event.

`core/compact` implements the compact form. See the
[Bluetooth implementation plan](../proposals/bluetooth.md) for implementation
status and the sequence of small PRs.

### 3.10 The frame

On a mesh transport, a frame carries one event, or one fragment of an event:

```text
frame type       1 byte: event, fragment, sync, announce, handshake, session
hop limit        1 byte
fragment header  8-byte fragment ID, uint16 index, uint16 total (big-endian)
body             the compact event, a fragment of it, or a sync message
```

The hop limit lives in the frame, not in the event. A mesh node changes it
without changing the signed event.

A receiver joins fragments by fragment ID. It keeps at most 128 incomplete
events, and drops an incomplete event after 30 seconds. It refuses an event
larger than 1 MiB.

`core/frame` implements event (type 1) and fragment (type 2) frames.

- Types 3–6 are reserved for sync, announce, handshake and session,
  respectively; this implementation rejects them until their protocols are
  implemented.
- There is no frame version byte: a future link handshake must negotiate the
  framing profile before exchanging frames.
- A transport supplies exactly one complete frame per decode; the body
  occupies all remaining bytes.
- An event frame has a 2-byte header. A fragment frame has a 14-byte header;
  indices start at zero and totals range from 2 to 4096. Bodies must be
  nonempty.
- The fragment ID is the first 8 bytes of SHA-256 of the complete compact
  event, including its signature. Reassembly checks that ID before compact
  decoding. This short digest detects inconsistent assembly; it is not
  authentication. Every completed event still needs store verification.
- Frame headers, including hop limits, are not signed by the event.

The splitter takes the usable application frame size, including these
headers, from the link adapter. This is not the raw BLE ATT MTU. Events that
fit use one event frame. Others use fragment frames; a size requiring more
than 4096 pieces is refused. Different fragment sizes for the same event
should not be mixed in one assembly; a link must finish, expire or reset that
transfer before reframing.

The assembler accepts pieces out of order and ignores identical duplicates.
Conflicting totals or different bodies for one index discard that assembly.
The completed hop limit is the minimum seen, including duplicate pieces; zero
allows local receipt but grants no forwarding permission. Forwarding is future
work. Completing an event removes its assembly, so replay suppression remains
with the store and future mesh layer.

The 30-second deadline starts at the first piece and is never extended.

- `Add` expires stale assemblies; the adapter must also call `Expire` on a
  timer while idle.
- Use a monotonic clock, and serialize calls to an assembler.
- Buffered bodies across all incomplete events are capped at 8 MiB; count and
  byte capacity failures reject the incoming piece without evicting unrelated
  events.
- The 4096 piece limit also bounds per-assembly bookkeeping. Exceeding the
  1 MiB limit for one event discards that assembly.
- The limits are local receiver policy.

### 3.11 The store

Each node keeps a store. The store is the source of truth for the node.
Transports write into it. The capability layer reads from it.

A node keeps these events:

| Class | Examples | Bound |
| --- | --- | --- |
| own | events that this citizen signed | none |
| addressed | private events for this citizen | none |
| carried | private events that this node carries as a courier | 40 events, see section 4.4 |
| public | capability announcements, public posts | a quota that the citizen sets |

The store MUST refuse an event that fails verification. It keeps one copy of
each event, by ID. It removes an event when its `expiration` tag passes.

Self data is a draft, a checkpoint, a part, or a deletion whose `k` tag names
one of their kinds (section 4.11). The owner of self data is the citizen
that its `p` tag names, or else its author. The owner of each other event is
its author.

- The store MUST keep one version of a replaceable or addressable event for
  each owner, kind and `d` tag. Thus two devices of one citizen write one
  page, not two.
- The store MUST apply a NIP-09 deletion to the events of its owner.
- If a key other than the owner signed self data, the store MUST ask the
  device list of the owner. The store is given the list; core reads no
  file. It MUST take the event only if the key is on the list. For a
  revoked key, it MUST take the event only if the event came from a storage
  citizen, its `created_at` is earlier than the revocation, and its
  generation is earlier than the generation of the revocation. If the
  owner has no device list at all, it takes the event. A store that is
  given no device list takes self data only from its owner.
- A refusal of a key that is not on the list says
  `the signer is not on the device list of the citizen`. The key can join
  the list later, so a sync checks such a record again.

## 4. Behavior

### 4.1 The router

The router takes an event from the capability layer and gets it to its
recipient. For each event:

1. If a live transport reaches the recipient now, send the event over it.
2. Send the event to each relay in the recipient's relay list.
3. If no transport delivers now, put the event in the outbox.
4. If the event is private and can take the courier form, deposit it with
   couriers, see section 4.4.

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

### 4.2 The outbox

The outbox keeps each undelivered event until the recipient acknowledges it.

- It keeps one entry for each recipient of an event. The key of an entry is
  the rumor id, `:`, and the public key of the recipient. An event to
  several recipients has one entry for each.
- It keeps at most 100 events for each recipient.
- A message lives for 7 days, set in the `expiration` tag of its wraps. After
  that, the outbox shows it as expired. The failure never stays silent.
- A send gives the wraps to the relays at once, and counts the attempt.
  Each sync gives the wraps to the transport again, and counts the attempt.
  A relay gets each wrap once. On a live transport, the outbox tries at most
  8 times.
- The outbox holds wraps and the sender's own seal, which are ciphertext. The
  sender reads its own message by opening its seal, because a NIP-44
  conversation key is the same from both ends.

An acknowledgement is a private event whose rumor has kind 3274. The rumor
names the delivered rumor with an `e` tag. When the sender receives the
acknowledgement, the outbox marks the entry of the author of the
acknowledgement delivered. The entries of other recipients of the same
rumor do not change. A node does not acknowledge an acknowledgement. A node
does not acknowledge mail that it holds from a stranger
([the strangers spec](../strangers/SPEC.md), section 3.4), until the
citizen accepts the stranger.

Queued calls keep a durable `pending`, `processing`, or `completed` state:

- Before saving a received request to the event store, the receiver commits
  its pending state and encrypted seal to the mail journal. It removes that
  extra seal after the event-store save succeeds. A restart can finish either
  write without another delivery from the sender.
- A failed journal write must not leave a receipt that looks like an
  uncertain historical execution.
- The receiver reads and opens a pending request before it atomically claims
  execution. A storage failure, signer refusal, or cancellation before that
  claim leaves the request pending. Concurrent receivers must recheck the
  state when they claim it.
- A request whose execution started without a recorded result remains
  uncertain; it must not execute again automatically.

### 4.3 Sync

When two nodes meet, they reconcile their stores for one filter at a time.
Sync uses the Negentropy protocol that NIP-77 wraps.

- Over a relay, the node uses NIP-77 where the relay supports it. Otherwise
  it sends a NIP-01 `REQ` with `since`. Sync compares sets with Negentropy
  when a relay lists NIP-77 in its information document, and fetches every
  event otherwise.
- Over a mesh link, the nodes send the same Negentropy messages as binary, in
  frames of type `sync`. Not built.
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

A node that serves does not sync a live relay on a timer. It keeps a watch
on the relay instead:

- The watch has two filters: wraps that name the key of the node in a `p`
  tag, and wraps with a route tag of the node. The relay sends the stored
  wraps first, and then each new wrap at once.
- When a watch begins, the node gives the relay each wrap of its outbox that
  the relay does not hold yet, and answers each pending call.
- The route tags change at midnight UTC. Then the node ends the watch, and
  starts a new watch with the tags of the new day.
- If the relay ends the watch, the node watches again after 3 seconds.
- On each tick, the node gives a watched relay only the wraps that the relay
  does not hold yet. If the relay holds each wrap, the node sends nothing.
- A directory cannot send a new event, so the node syncs each directory on
  each tick. A relay that has no watch also gets a sync on each tick.

### 4.4 Couriers

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

**On a mesh link.** Not built. Two nodes on a mesh link know each other, so a
copy budget holds:

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
  relays and files. A capability announcement is public, so it moves by sync,
  not by couriers.
- On a mesh link, a courier that meets the recipient delivers the event and
  removes it. On a directory, a courier cannot know who reads it, so the event
  stays until it expires.
- A courier that meets a relay can post the courier form there. The recipient
  finds it by route tag.

The courier cannot read what it carries. It knows only the route tag.

### 4.5 Mesh relay

Not built. On a mesh transport, a node relays events to nodes that it does
not reach directly. It uses a controlled flood:

- An event starts with a hop limit of 7.
- Each relay lowers the hop limit by 1. If the node has 6 or more neighbours,
  it lowers the hop limit to at most 5.
- A node waits a random 10 to 220 ms before it relays. If a copy arrives from
  another node first, it cancels its relay.
- A node never sends a frame back on the link that it came from.
- A node remembers the IDs of the last 1,000 events for 5 minutes, and relays
  each one once.

### 4.6 Live links

Not built as an integrated transport. The experimental
[direct adapter](BLUETOOTH-DIRECT.md) has in-memory event and call proofs,
but no normal CLI or secure-radio proof. Two nodes with a live mesh link open a Noise session with the XX
pattern, with Curve25519, ChaCha20-Poly1305, and SHA-256. The session
authenticates both nodes and gives forward secrecy on that link.

A live call over the mesh travels inside the session. Store-and-forward
events do not need the session, because their content is already sealed.

A live call over a relay has no forward secrecy. NIP-44 does not give it.

### 4.7 Announcement

A provider announces a capability with an addressable event of kind 30272:

- The `d` tag is the capability ID.
- The content is the manifest.
- The provider signs the event. The signature is the authorship of the
  manifest.

Announcements are public. Nodes keep them and sync them. A new version of the
manifest replaces the old one, because the event is addressable.

Two announcements of one second tie, and a store keeps the one with the lower
ID. Before `arc announce` signs, it therefore reads the announcements of that
capability that the store and the relays hold. It MUST sign the new one at
least one second after the newest of them. An older client signs at the
current second, so its new version can lose to an announcement of the same
second. A reader needs no change.
`TestAnnounceReplacesAnAnnouncementOfTheSameSecond` in `cmd/arc` proves
this.

An announcement is one event, so its JSON holds at most 65,536 bytes, see
section 3.4. If the announcement of a manifest is larger, the provider MUST
refuse to sign it. The error gives the size of the event and the size of
the manifest. The author must make the manifest smaller. No other path
carries a manifest.

### 4.8 Discovery

`arc discover` reads announcements from the local store and from relays.
Search terms match the `t` tags of an announcement, and full text where a
relay supports NIP-50.

### 4.9 Install and trust

`arc install <provider>` reads the announcement, verifies it, and asks the
citizen to trust its author once. The installed commands come from the
manifest, as today.

### 4.10 Calls

A call is a private event. The request is a rumor of kind 3272. The reply is
a rumor of kind 3273. The reply names the request rumor with an `e` tag.

The manifest declares the class of each command:

| Class | Gift wrap | Behaviour |
| --- | --- | --- |
| store and forward | 1059 | The router delivers it on any transport, however late. The reply returns the same way. |
| live | 21059 | The router needs a live path now. If none exists, the call fails at once. |

A live call subscribes, waits until the relay has taken the subscription, and
only then sends, all on one connection, because a relay never stores the
ephemeral reply.

Receipt and processing are separate:

- A deferred request remains pending when no provider is attached, including
  across restarts.
- One citizen can serve several capabilities, each with its own provider,
  and each provider gets all the requests to the citizen. A provider does
  not answer a request or a session for a capability that it does not
  serve. A live request gets no reply from it, and a deferred request
  remains pending for the provider of its capability. A request for a
  capability that no provider of the citizen serves gets no reply.
- The mail journal records pending, processing and completed requests, and
  commits the encrypted reply's forwarding state before transmission.
- A restarted or interrupted execution with no recorded reply returns
  `outcome_unknown` once its execution window has elapsed; it is never
  automatically executed again. The same conservative rule applies to
  historical receipts without a processing journal. This does not promise
  exactly-once effects in an external service.

Outgoing calls commit their outbox and forwarding state before any send.
Live replay protection remembers request IDs for the live window. Each request
rumor carries a `nonce` tag with 8 random bytes as hex. Thus two equal
requests in one second have two IDs, and the provider answers both.

A call has no acknowledgement. The reply clears the caller's outbox. The
provider sends its reply again on each sync until the reply expires, so a
caller whose reply was lost still gets it.

A provider MUST refuse a live request whose rumor is more than 5 minutes old.
This window applies to live calls only. A store-and-forward call has no
window, because it can travel for days.

A provider publishes its relay list with NIP-65:

- A caller sends a live request to its own relays, then to each read relay of
  the provider that it does not use.
- Failover is allowed only when the transport proves that the request was not
  submitted. After an ambiguous send or a lost reply, the caller reports an
  unknown outcome and does not repeat the operation on another relay.
- The caller also looks for the provider's announcement on those relays
  before the call. The caller finds the provider's list in its store, or on
  its own relays.
- A store-and-forward request goes to the caller's relays and to the
  provider's NIP-17 relay list, as mail does.

### 4.11 Data that a citizen keeps for itself

Some data belongs to one citizen only, such as a private journal. A device
of the citizen seals it with a data root, signs it with its device key, and
keeps it in the local store. A `p` tag names the citizen. It syncs it between
its own nodes through its storage citizens, or a directory that it carries. It
does not send it to a relay. The data has the shape of a NIP-37 draft, and
`core/draft` makes it. See docs/interface/SPEC.md, section 3.9.4, and
docs/storage/SPEC.md. Data of the format before the data roots is sealed to
the citizen key, and the citizen key signs it.

An ARC relay refuses new drafts, checkpoints, parts and private relay lists,
kinds 31234, 1234, 3275 and 10013. The one exception is the draft with the d
tag `arc-keyed-root`. The relay still serves the old events of these kinds
to their author, until the author deletes them or the operator removes
them. The NIP-11 document of the relay has the tag `arc-drafts-retired`. See
docs/storage/SPEC.md, sections 4.2.1 and 7.

An older client sends drafts to its relays. An older relay takes a draft
only from its author, because the draft carries the NIP-70 tag. The relay
transport answers the NIP-42 challenge of the relay with the citizen's key.

The store applies a NIP-09 deletion of an owner to the events of that
owner, and refuses them after that, so a stick or a relay cannot bring a
deleted event back (section 3.11). The one exception is a deletion with the tag
`["arc", "moved"]`. A device sends it to an ARC relay for the drafts that a
storage citizen holds. The store refuses it, and the relay applies it and
then serves it to nobody. A deletion whose `k` tag names a sealed kind is
sealed: the relay serves it only to its author.

### 4.12 Relay paths for direct connections

Two apps can open a direct connection for bulk bytes, as the transfer app
does. ARC carries only the offer and the answer. The relay helps the two
machines find a path, and carries the bytes only as the last resort.

**STUN.** `arc relay serve` answers STUN binding requests (RFC 8489) on UDP
port 3478 of its host. A machine asks it for its public address.

**TURN.** If the operator sets `--turn-ports`, the relay also serves TURN
(RFC 8656) on the same UDP port. These rules apply:

- The relay MUST give an allocation only to a citizen with a valid
  credential.
- A credential has the form of the TURN REST API. The username is
  `<expiry>:<key>`, where `<expiry>` is a Unix time and `<key>` is the
  public key of the citizen as hex. The password is the base64 HMAC-SHA1 of
  the username, with a secret that only the relay knows. A credential lives
  one hour.
- The relay gives a credential only in reply to `GET /turn` on its HTTP
  listener, with a NIP-98 `Authorization` header: `Nostr ` and a base64
  event of kind 27235. The relay MUST refuse the request if the signature is
  not valid, if the `created_at` of the event is more than 60 seconds from
  its clock, if the `method` tag is not `GET`, or if the host and path of
  the `u` tag are not those of the request. The relay does not compare the
  scheme, because a proxy can end TLS.
- The reply is JSON: `urls`, `username`, `password`, `ttl` in seconds, and
  `quota_left` in bytes.
- The relay MUST count the bytes of each allocation, in both directions,
  against the key in its username. Each key has a quota for each day, which
  the operator sets with `--turn-quota`. When a key used its quota, the
  relay MUST refuse a new allocation for it, and stop relaying for the
  allocations that it has.
- A relay sets its relay ports with `--turn-ports`, and the public address
  of those ports with `--turn-ip`.
- A citizen whose network lets out no UDP, such as a cloud sandbox that
  allows only TCP port 443, can reach TURN over TCP. With `--turn-tcp
  <address>` the relay also serves TURN on that TCP address, such as
  `127.0.0.1:3479` behind a TLS proxy on port 443. The relay ports stay UDP.
  `--turn-url <url>`, which can repeat, adds an address to each credential
  after the UDP one, such as `turns:relay.example:443?transport=tcp`.
- The relay MUST relay a packet from one of its allocations to another of
  its allocations. If the relay ports listen on a private address behind
  the public address, as on Fly.io, the network can drop a packet from the
  host to its own public address. The relay then sends the packet to the
  private address, and shows the public address as the source to the other
  allocation. Only the relay ports get this change.

`arc relay turn [url]` signs the NIP-98 request with the key of the citizen
and prints the reply. With no URL, it asks the relays in their order, and
takes the first credential that names TURN over TCP or TLS, else the first
that any relay gives. An app gets a credential with this command, because
an app cannot read the key.

An app MUST use TURN only after its direct attempts fail. A failure of the
direct attempts to the same citizen in the last 6 hours counts, as in the
sync of self data (docs/storage/SPEC.md, section 4.6). An app that then
goes to TURN first MUST run the direct attempts if TURN gives no path. It
MUST tell the user that the bytes go through the relay. Before it starts an attempt
through the relay, it compares `quota_left` with the bytes that remain, and
refuses with an error if the quota is too small.

### 4.13 Authentication of a device

A device signs through a remote signer (docs/devices/SPEC.md). A relay
serves sealed data, such as the storage list, only to its author. Before
the token, the device answered the NIP-42 challenge with the citizen key,
so each read of sealed data needed the signer. A token of kind 20274
(docs/devices/SPEC.md, section 3.6) lets the device key read the sealed
data of the citizen with no signature of the citizen key.

Rules for an ARC relay:

- The relay MUST list the tag `arc-delegation` in its NIP-11 document.
- The relay MUST take a token only on a connection that authenticated with
  NIP-42. It MUST check the signature, that the `p` tag names a key that
  authenticated on the connection, the time, and T. It MUST refuse a token
  that fails a check, with a reason that starts with `restricted:`, or with
  `auth-required:` when no key authenticated.
- After it takes a token, the relay MUST serve the sealed data of the
  citizen who signed it on that connection, until T. The token MUST NOT
  give any other right: the NIP-70 check still needs the key of the author.
- The relay MUST NOT keep a token, and MUST NOT send it to any
  subscription. khatru therefore answers a token that it took with
  `mute: no one was listening for this`.
- The relay MUST forget the tokens of a connection when it closes.

Rules for the relay transport of a device:

- If the relay lists `arc-delegation`, and the device holds a token that
  lasts one more hour, the device MUST answer the challenge with its device
  key, and then send its token on the same connection. It MUST NOT send the
  token to a relay without the tag.
- Else, the citizen key answers the challenge, through the signer, as
  before.
- A protected event of the citizen (NIP-70) needs a connection on which the
  citizen key authenticated. If the relay refuses such an event on a
  connection of the device key, the transport sends it again on a new
  connection on which the citizen key answers the challenge.

The token rides as an event after the AUTH event, not as a tag of it.
khatru gives no hook that reads the AUTH event, and its NIP-70 check reads
only the last key that authenticated.

## 5. Failures

| Failure | What happens |
| --- | --- |
| An event fails verification | The node drops the event, and does not forward it. The store refuses it. |
| The seal and the rumor name different authors | The recipient drops the event. |
| No path delivers a live call now | The call fails at once, and the router tells the caller. |
| No path exists for the size of an event | The router tells the caller which path it needs. |
| A message is not acknowledged in 7 days | The outbox shows it as expired. The failure never stays silent. |
| An execution started, and no reply was recorded | The request returns `outcome_unknown` once its execution window has elapsed. It never executes again automatically. |
| A send is ambiguous, or a reply is lost | The caller reports an unknown outcome, and does not repeat the operation on another relay. |
| A stored seal cannot be read | Reading recorded mail returns the storage, signer or cancellation failure. An unreadable stored seal must not appear as an empty mailbox or a successful partial history. |
| A live request is more than 5 minutes old | The provider refuses it. |
| A request to `GET /turn` has a bad, old or foreign NIP-98 event | The relay replies 401 with the reason, and gives no credential. |
| A relay serves no TURN | `GET /turn` gets 404. `arc relay turn` says that the relay serves no TURN. The app fails with "no direct path, and no path through the relay". |
| The TURN quota of a citizen is too small for a file | The app refuses before the attempt, and names the quota that is left and the bytes that the file needs. |
| A citizen uses its TURN quota during a transfer | The relay stops relaying. The connection ends, and the app reports how many bytes the part file holds. |
| The words of a seed have a bad checksum, a word that is not in the list, or a count other than 12 or 24 | `arc keys recover` refuses the words, says why, and keeps nothing. |
| A seed passphrase is wrong or missing | The words give another key. Nothing can detect this: the node shows a name and a public key that the citizen does not know. |

## 6. Security

### 6.1 What each party sees

| Party | Sees | Does not see |
| --- | --- | --- |
| relay | the one-time key, the routing tag, the size, the arrival time | the author, the content, the real time |
| mesh neighbour | the frame type, the size, the timing, the Bluetooth address | the author and content of a private event |
| courier | the route tag, the size | the recipient, unless it already knows their key; the author; the content |
| recipient | everything | — |
| provider | the caller and the call, because it is the recipient | — |

A public event, such as a capability announcement, is readable by everyone.

### 6.2 Abuse limits

- Copy budgets are capped, so one event cannot amplify itself through
  couriers.
- Couriers limit deposits for each node, and keep room for contacts. The
  contacts of a node are the contacts of
  [the strangers spec](../strangers/SPEC.md), section 3.1.
- A recipient's device holds mail from a stranger without 16 bits of
  NIP-13 work apart from the inbox, and does not acknowledge it. A sender
  makes that work for a recipient that never wrote to it. See
  [the strangers spec](../strangers/SPEC.md).
- The hop limit caps the reach of a flood.
- Each store has a quota for public events.
- A one-time key signs each gift wrap, so a relay cannot limit abuse by
  author. An ARC relay therefore asks for NIP-42 authentication, or for
  NIP-13 proof of work, before it accepts a gift wrap.

- A relay refuses a gift wrap of kind 1059 above the size of mail, and
  each other event that it keeps above 65,536 bytes, see section 3.4. These limits are
  always on.

- A TURN relay gives allocations only to citizens that prove their key
  with NIP-98, and counts their bytes against a quota for each day, see
  section 4.12. The relay pays for the bandwidth of each relayed transfer.

`arc relay serve` applies the other limits when its flags turn them on, see
`arc relay serve --help`. A relay can also lower the size limit of one event,
and cap the rate of events from one IP address.

The relay transport answers the NIP-42 challenge of a relay and sends the
gift wrap again. On the relays of the citizen, the citizen's key answers,
or the device key with a token (section 4.13). On
the inbox relay of a recipient, a one-time key answers. Thus the relay of the
recipient does not learn who wrote to them.

### 6.3 Security properties

| Property | Holds | Why |
| --- | --- | --- |
| Authorship | yes | Every event is signed. The seal of a private event is signed by its author. |
| Integrity | yes | The ID covers every field, and the signature covers the ID. |
| Confidentiality of content | yes | NIP-44 inside a seal inside a gift wrap. |
| Recipient hidden from relays | no, in relay form | The `p` tag names the recipient. |
| Recipient hidden from couriers | partly | See section 3.6. |
| Forward secrecy | live mesh links only | Noise gives it. NIP-44 does not. |
| Replay | harmless | A node keeps one copy of each event, by ID. A provider answers each request once. |
| Key loss | fatal without a backup | The seed derives a key that came from a seed again. A key from before the seed comes back only from its `ncryptsec`. Nothing else recovers a lost key. |

## 7. Compatibility

### 7.1 Ed25519 keys

ARC does not move an Ed25519 key to a secp256k1 key. A citizen of the older
stack makes a new key, installs its capabilities again, and trusts their
signers again. One operator runs all the Ed25519 citizens, so a migration
record costs more than it saves. Kind 10272 stays reserved for that record,
and is not used.

### 7.2 What replaced the older stack

The CHANGELOG entries of 0.10.0 and 0.11.0 record the switchover: `arcn`
became `arc`, the older program was `arc-legacy` for one release, and 0.11.0
removed the older stack and moved the home of `arc` to `~/.config/arc`.

| Before v0.11.0 | From v0.11.0 |
| --- | --- |
| `identity` (Ed25519) | secp256k1 keys from `fiatjaf.com/nostr` |
| `sealedbox`, `session`, `packet`, `frame`, `internal/wire` | NIP-44 and NIP-59 |
| `announce` | capability announcement events |
| `relay` with routes and federation | relays built on khatru, and the NIP-65 outbox model |
| `client` | the node: store, router, and transports |
| `direct` | left out of the first version |
| `citizen` provider runtime | moved to `adapters/provider/host`, which `arc serve` uses: a provider still runs as a process over standard input and output |
| `capability`, `toolbox`, installed commands | kept; the manifest travels in an announcement |
| `cmd/dm-provider` | NIP-17 direct messages; no provider needed |
| `cmd/journal-provider` | data that the citizen keeps for itself; no provider needed |
| `cmd/agora-provider` | public events on a relay; no provider needed |
| `cmd/files-provider` | the files manifest of docs/interface/SPEC.md; no provider needed |
| `apps/exec`, `apps/sqlite`, `apps/releases` | kept, answering calls |
| `wake` | kept; the hook runs before a call on the node |
| `control` | removed; the node store answers who this citizen is |
| `cmd/arc-relay` | `arc relay serve`, a khatru relay |
| `cmd/arc` and `cmd/arcn` | one program, `arc`, built from the former `cmd/arcn` |

Commands of the older `arc` that the switchover removed:

| Command | Why |
| --- | --- |
| `tool pin`, `unpin` | An announcement keeps only its newest version. Consent stops a version that asks for more, see docs/interface/SPEC.md. |
| `trust list`, `allow`, `deny` | An install trusts the author of the capability. |
| `cache` | It needs an output filter that manifests of interface version 1 do not have. |
| `publish` | The node store replaces the control plane. |

This spec states no plan for its own removal.

### 7.3 Keys from before the seed

The seed changes no wire format and no key file. An older client reads the
key file of a key that a seed derived, because the file holds the secret key
as before. A key from before the seed stays valid. ARC does not move it to a
seed.

### 7.4 Tokens of a device

A token changes nothing for an older relay or an older client. An older
relay has no tag `arc-delegation`, so a device never sends it a token, and
answers its challenge with the citizen key. An older client never sends a
token, and a new relay serves it as before. An older relay that gets a
token anyway broadcasts it to the subscriptions of kind 20274. It shows
that the device key belongs to the citizen, and gives nobody a right
without the device key.

### 7.5 Self data of device keys

The owner rule of section 3.11 changes the store. An older store names self
data by its author. It therefore refuses nothing that a device key signed
for the citizen, but keeps it as data of the device key, and its reader does
not open it. An older device that gets such a record from a storage citizen
refuses it, because the record does not name the citizen as its author. It
reads the record after its update. [The storage spec](../storage/SPEC.md),
section 7, says how the newer device fetches it again.

### 7.6 The outbox of group messages

The key of an outbox entry was the rumor id alone. It is now the rumor id
and the recipient (section 4.2). The first open of the mail database moves
each older entry, and names the recipient of each own wrap of that entry,
in the transaction that opens the database.

- An older arc that opens the moved outbox lists each entry. It looks an
  entry up by the rumor id, so an acknowledgement or a reply that arrives
  does not find its entry. The message stays pending.
- An older arc opens a message with several `p` tags as a message from its
  author, and its reply goes to the author only.
- `TestAnOldOutboxMovesToTheNewKeys` opens the mail database of a sender
  of v0.31.0. [The group messages spec](../groups/SPEC.md), section 7,
  has the details.

### 7.7 Replies

The `e` tag of a reply is new in kind 14 (section 3.4). Kind 14 stays as
it is, and the kind registry needs no new row.

- An older arc reads the author, the `p` tags, the time and the content of
  a message, and no `e` tag. It shows a reply as a plain message. Nothing
  fails. `TestAnOlderArcReadsAReplyAsAMessage` in `core/mail` reads a reply
  with the inbox rule of an older arc.
- Other NIP-17 clients read the same tag. No test or run uses one.

## 8. Proof

### 8.1 Phases

Each phase ends with its proof. A phase that does not pass its proof does not
merge. `mise run delivery` runs the proofs of phases 1 to 3.

| Phase | Scope | Proof | State |
| --- | --- | --- | --- |
| 1 | Identity, events, the store, the router, and the relay and file transports | Two machines sync a journal through a relay, then through a USB stick. A changed event is refused. | Built |
| 2 | The outbox, acknowledgements, route tags, couriers, and sync | A message reaches an offline recipient through a third machine that carries a USB stick. | Built |
| 3 | The capability layer: announcements, discovery, install, and both classes of call; direct messages on NIP-17 | A live call to `exec` succeeds over a relay, and the round-trip time is recorded. A store-and-forward call crosses the courier path. An ARC direct message opens in a NIP-17 client. | Built |
| 4 | The switchover: `arc` runs on this layer only, and the older stack is removed | No package imports `relay`, `session`, `packet`, `frame`, `direct`, `sealedbox`, `client` or `identity`. `arc update apply` of v0.10.0 installs a later signed build. | Built |
| 5 | Bluetooth LE on Linux, the compact form, fragments, the mesh relay, and Noise links | Three Linux nodes in a line pass a message from one end to the other. The two end nodes are out of each other's reach. | Not built. The experimental direct BLE adapter uses `core/compact` and `core/frame`; normal CLI integration, hardware proof and mesh are pending. |
| 6 | LoRa through a local Reticulum instance | A message crosses two LoRa nodes with no internet. | Not built |

Phases 1 and 2 are built: the packages under `core/` and `adapters/`, and
the command `arc`. Phase 2 added `core/private` for gift wraps and route
tags, and `core/mail` for the outbox, acknowledgements and couriers.
`core/draft` seals data to its author as NIP-37 drafts, and
`adapters/relay/sealed` makes a relay serve those drafts only to their
author, after NIP-42 authentication.
The capability interface uses both. Phase 3 added `runtime/catalog` for
announcements, discovery and installs, and `core/call` for both classes of
call. `arc call` calls a capability with a raw body.

The journal of phase 1 is now the journal manifest of the capability
interface. The capability interface, docs/interface/SPEC.md, declares the
commands of a capability. The journal adds one thing that phase 3 names: a
page travels as parts of at most 32 KiB, so it fits the event limit of common
relays, and `arc journal tail` streams text as it is appended.

The seed of section 3.2 has these tests:
`go test -run 'Seed' ./core/keys/` derives the two test vectors of NIP-06,
copied from the NIP text, and refuses a bad checksum, an unknown word and 18
words. `go test -run '^TestSeed$' ./internal/proof/` runs the real `arc`
program: `keys gen` shows 24 words on standard error and stores none,
`keys recover` on another home makes the same key, a seed passphrase gives
another key, and a bad checksum and words as arguments are refused.

The relay paths of section 4.12 have these tests:
`go test -run '^(TestCredential|TestQuota)$' ./adapters/relay/turn/` checks
the NIP-98 refusals and the byte quota on a real TURN allocation.
`go test -run '^TestRelayToRelay$' ./adapters/relay/turn/` sends packets
between two allocations of a relay whose public address does not reach the
host.
`go test -run '^TestAnAllocationOverTCP$' ./adapters/relay/turn/` gets an
allocation over TCP, relays bytes on its UDP relay port against the quota,
and checks the extra URL of the credential.
`go test -run '^(TestTURNFromTheFirstRelayThatServesIt|TestTURNPrefersARelayThatTakesTCP)$' ./cmd/arc/`
checks which relay gives the credential.
`go test -run '^TestRelayPath$' ./sdk/direct/` puts two ends behind
symmetric NATs on an emulated network: the direct attempt finds no path, and
the attempt through TURN sends the file.

The token of section 4.13 has these tests:
`go test -run '^TestADeviceReadsSealedDataWithAToken$' ./adapters/relay/sealed/`
reads a draft of the citizen with a device key and a token, while the
citizen signer fails each signature. The relay refuses a token for another
key, a token that has ended, and the data of a citizen who did not sign the
token, and keeps and broadcasts no token. `go test -run '^TestDevicePages$'
./internal/proof/` runs the real `arc` program: with the bunker stopped, the
phone lists its storage list and syncs in less than 5 seconds, and a desk
on a relay of v0.26.0 waits for the signer.

### 8.2 Measurements

On a local relay, a live call to `exec` takes about 10 ms for the round trip.

### 8.3 Deferred work

| Item | Why it waits | When to decide |
| --- | --- | --- |
| The TLS direct carrier as a live transport | Relays can carry live calls: about 10 ms for a round trip on a local relay. | After a round trip is measured through a public relay. |
| A full macOS mesh node | It needs a peripheral backend in Go. | After phase 5, see section 3.8. |
| A bridge to bitchat direct messages | bitchat's Nostr envelopes are not NIP-17. Only the citizen's own node can translate them, because translation needs the private key. | When ARC direct messages must reach bitchat users. |
