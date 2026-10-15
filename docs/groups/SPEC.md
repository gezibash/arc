# Group messages

- Status: built
- Layers: core, sdk, runtime
- Owns: none
- Proof: go test -count=1 -run '^(TestAGroupMessageReachesEachMember|TestAnAcknowledgementMarksOnlyItsAuthor|TestAMessageThatDoesNotNameTheRecipientIsRefused|TestAGroupHasAtMostTenParticipants|TestAnOldOutboxMovesToTheNewKeys|TestAnOlderArcReadsAGroupMessageAsADirectMessage|TestGroupMessages|TestGroups|TestAPrivatePublishGoesToEachRecipient)$' ./core/mail/ ./sdk/arcprogram/ ./apps/dm/... ./runtime/iface/
- Unverified: No automated test runs the program of an older arc. `TestAnOlderArcReadsAGroupMessageAsADirectMessage` reads with a copy of the inbox rule of v0.31.0; one manual run used the program of v0.31.0 (section 8). No test sends a group message through a courier. No test runs a group of 10 participants on a relay.

A group message is one NIP-17 message to several citizens. Each member sees
the other members, and a reply goes to each of them.

## 1. Purpose

Before this spec, a private event went to one recipient. A list
(`cmd/arc/lists.go`) runs a command once for each member, so each member
gets a separate rumor with one `p` tag. No member sees who else got the
message, and a reply goes to the sender only. A list is a fan-out, not a
group.

This spec defines:

- the rumor of a group message, and the limit of its members,
- how the mail layer seals it and keeps its delivery state,
- what a receiver checks, and what it returns,
- the commands that send and read a group message.

The mail layer of [the delivery spec](../delivery/SPEC.md) carries each
copy. This spec adds no kind and no wrap.

This spec does not give a group a stable id. NIP-17 names a room by the set
of its members, and a new set is a new room. This spec does not define a
read receipt, and it does not change how a list sends.

## 2. Terms

- **Rumor**, **seal**, **gift wrap**: the layers of a private event, as
  [the delivery spec](../delivery/SPEC.md), section 3.4, defines them.
- **Message**: a rumor of kind 14, as NIP-17 defines it.
- **Group message**: a message with more than one `p` tag.
- **Direct message**: a message with one `p` tag.
- **Members**: the author of a message, then each public key that a `p` tag
  names, once each.
- **Participants**: the members of a group message, with its author.
- **Outbox entry**: the delivery state of one message to one recipient.

## 3. Rules

### 3.1 The rumor

- A group message MUST be one rumor of kind 14, with one `p` tag for each
  member other than the author, in the order that the author chose.
- A group message MUST NOT have more than 10 participants, with the author.
  NIP-17 advises another scheme past 10. In ARC each member costs two wraps
  (the courier form and the relay form) and one acknowledgement, and the
  outbox keeps at most 100 events for each recipient.
- A `p` tag MUST NOT name the author, and MUST NOT name a key twice. The
  sender refuses such a send before it seals anything.
- The rumor MAY hold a `subject` tag. ARC does not read it yet.

### 3.2 Sealing

- The sender MUST seal the one rumor to each member separately, and wrap
  each seal in the two forms, as for a direct message.
- The sender MUST NOT wrap a copy to itself. It opens its own seal, as
  [the delivery spec](../delivery/SPEC.md), section 4.2, says.

### 3.3 The outbox

- The outbox MUST keep one entry for each recipient of a message. The key
  of an entry is the rumor id, `:`, and the public key of the recipient,
  both in hex.
- The sender MUST commit the entries and the wraps of each member in one
  transaction, before it sends a wrap.
- An acknowledgement MUST mark the entry of its author only. A group
  message is delivered when each member acknowledged it.
- The first open of a mail database of an older arc MUST move each entry
  that the rumor id alone names to the new key, in the transaction that
  opens the database. It also names the recipient of each own wrap of
  those entries, so a sync still counts its attempts.

### 3.4 Reading

- A receiver MUST refuse a message whose `p` tags do not name its own key.
  It does not keep the seal, and it does not acknowledge the message.
- The inbox MUST return the members of each message.
- The inbox MUST leave out a stored message whose `p` tags do not name this
  citizen.

### 3.5 Commands

- `arc message send <key>,<key> [text]` sends one group message.
- The message line and the sent line of `arc message` have the field
  `members`, as [the client programs spec](../clients/SPEC.md) defines.
- A private publish of [the interface spec](../interface/SPEC.md) MAY name
  several recipients, with the same limit. The mail layer seals one rumor
  to each. The manifest gives the tags.
- `arc-dm` keys a conversation by the sorted set of its members. A reply in
  a group goes to each member.

## 4. Behavior

### 4.1 Sending

1. The author runs `arc message send <bob>,<carol> hello`.
2. arc checks the recipients: at least one, at most 9, no duplicate, not
   the author.
3. The mail layer makes one rumor of kind 14 with a `p` tag for bob and one
   for carol.
4. For each member, it seals the rumor, wraps the seal in the two forms,
   and saves the seal and the wraps in the store.
5. It commits the outbox entries `<rumor>:<bob>` and `<rumor>:<carol>`,
   and each wrap, in one transaction.
6. It sends the wraps of each member to the relays, and to the inbox relays
   of that member. A relay that fails does not fail the send.

### 4.2 Receiving

1. Bob syncs, and opens the wrap for bob.
2. Bob checks that a `p` tag names bob. It does, so bob keeps the seal and
   acknowledges the rumor to the author.
3. Bob's inbox shows the message from the author, with the members author,
   bob and carol.

### 4.3 Delivery state

1. The author syncs, and opens the acknowledgement of bob.
2. The mail layer finds the entry `<rumor>:<bob>`, and marks it delivered.
   The entry of carol stays pending.
3. `arc message outbox` shows `delivered to 1 of 2`.

### 4.4 Replying

1. Bob runs `arc-dm send <author>,<carol> yes`, or opens the group in
   `arc-dm` and replies.
2. arc sends a group message with a `p` tag for the author and one for
   carol. The members are the same set, so each member shows it in the
   same conversation.

## 5. Failures

| Cause | What happens | What the user sees |
| --- | --- | --- |
| More than 9 recipients | arc refuses the send. Nothing is sealed. | `a group message has at most 10 participants, with you; this one has 11`. |
| A recipient twice | arc refuses the send. | `<key> is a recipient twice`. |
| The author among the recipients | arc refuses the send. | `a message to yourself belongs in the journal`. |
| A recipient that is not a key | arc refuses the send. | `a recipient is 64 characters of hex, as arc whoami prints it; separate the recipients of a group with commas`. |
| A member does not acknowledge | Its entry stays pending, then expires after 7 days. The other entries are not changed. | `delivered to 1 of 2`, then `delivered to 1 of 2, 1 expired`. |
| A seal to this citizen that names it in no `p` tag | The receiver refuses it, keeps no seal, and sends no acknowledgement. | The sync report lists it as refused. |
| An older arc opens the outbox after the update | It lists each entry, but an acknowledgement or a reply does not find its entry. | The message stays `pending`, then `expired`. See section 7. |

## 6. Security

- Each member reads the keys of the other members. A sender that wants to
  hide the members uses a list, which sends a separate direct message to
  each.
- A member can seal a rumor that names other members to a citizen that the
  rumor does not name. Without the check of section 3.4, that citizen sees
  a conversation that it is not in. The receiver refuses it.
- A relay sees one wrap for each member, each to its own recipient key,
  with a one-time key. The wraps of one group message share no key and no
  tag. A relay that compares the sizes and times of the wraps can guess
  that they belong together.
- The author pays for each member: one seal, two wraps and one
  acknowledgement. The limit of 10 participants bounds this cost.
- An acknowledgement marks only the entry of its author. A member cannot
  mark the message delivered to another member.

## 7. Compatibility

- An older arc opens a group message as a direct message from the sender.
  Its inbox reads no `p` tag. A reply from it goes to the sender only.
  Nothing fails.
- An older sender sends only direct messages. A newer receiver shows each
  of them as a conversation of two.
- The outbox layout change is one way. The first open of a newer arc moves
  each entry. An older arc that opens the moved outbox still lists each
  entry, because it reads the values. It looks an entry up by the rumor id
  alone, so an acknowledgement or a reply that arrives does not find its
  entry, and the message stays pending. An entry that the older arc adds
  has the old key, and the next open of a newer arc moves it. The release
  notes say so, as a breaking change.
- The message line and the sent line get a new field, `members`. A client
  program that does not know it ignores it, as the client programs spec,
  section 3.5, requires.
- `arcprogram.Arc.Send` takes a list of recipients. An app that called it
  with one key passes a list of one.
- Kind 14 stays as it is. [The kind registry](../KINDS.md) needs no new
  row.

## 8. Proof

| Test | What it proves |
| --- | --- |
| `TestAGroupMessageReachesEachMember` | Three citizens on a relay. A group message reaches two members, each sees the three keys, and a reply from one member reaches the other two. |
| `TestAnAcknowledgementMarksOnlyItsAuthor` | After one member acknowledges, its entry is delivered and the entry of the other member is pending. |
| `TestAMessageThatDoesNotNameTheRecipientIsRefused` | A seal to carol that names bob only is refused, is not in the inbox, and gets no acknowledgement. |
| `TestAGroupHasAtMostTenParticipants` | A group of 10 sends. A group of 11, a key twice, the author and no recipient are refused, and leave no entry. |
| `TestAnOldOutboxMovesToTheNewKeys` | The mail database of a sender of v0.31.0, in `core/mail/testdata/outbox-v0.31.0`, opens with its two entries and their states, twice with no duplicate. Its pending message still goes out, and its acknowledgement marks it delivered. |
| `TestAnOlderArcReadsAGroupMessageAsADirectMessage` | The wrap for a member is a NIP-59 wrap of a kind 14 rumor from the sender. The inbox rule of v0.31.0, copied in the test, shows it as one message from the sender. |
| `TestGroupMessages` | `arc message send --json` with two keys: the queued line, the message lines and the sent lines name the three members, and the outbox has one line for each recipient. |
| `TestAPrivatePublishGoesToEachRecipient` | A private publish of a manifest with two recipients gives each the same rumor, with a `p` tag for each. |
| `TestGroups` | arc-dm on a relay: a group conversation apart from a direct one, `delivered to 1 of 2` in `arc message outbox` and in `arc-dm open`, and a reply that reaches each member. |

A manual run used the program of v0.31.0, built from its tag, as bob, with
a newer arc as the sender and as carol, on a relay on this machine. Bob's
`arc message inbox` showed the group message as a message from the sender,
and bob's reply reached the sender only.
