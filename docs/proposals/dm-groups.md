Status: proposed

# Group messages

No code implements this proposal. Each claim about ARC names a file and a
line, at commit `dba5a6a`. Each number says "measured" or "estimated".

## 0. Summary

- A group message is one NIP-17 rumor of kind 14 with a `p` tag for each
  member. The sender seals it to each member separately.
- The members of a conversation are the sender and the `p` tags. A change
  of members makes a new conversation, as NIP-17 says.
- The mail layer keeps one outbox entry for each member, so each member
  acknowledges the message separately.
- An older arc shows a group message as a direct message from its sender.
  A reply from it goes to the sender only.
- The first step changes `core/mail` and the delivery spec. `arc message`
  and `arc-dm` follow.

## 1. The problem

A private event goes to one recipient today:

- [The interface spec](../interface/SPEC.md), section 7.6, says so, and
  `runtime/iface/data.go:886` refuses a publish with more than one
  recipient.
- `core/mail/mail.go:162` `SendRumor` takes one recipient.
- `core/mail/mail.go:193` keys the outbox entry by the rumor id. One rumor
  to two recipients would keep one entry, and lose the delivery state of
  the other.
- `core/mail/mail.go:715` accepts an acknowledgement only from the one
  recipient of the entry.

A list sends to several citizens (`cmd/arc/lists.go`, proven in
`internal/proof/interface_test.go`). It runs the command once for each
member. Each member gets a separate rumor with one `p` tag. No member sees
who else got the message, and a reply goes to the sender only. A list is a
fan-out, not a group.

## 2. What NIP-17 defines

- A chat message is a rumor of kind 14. Its `p` tags name the receivers.
- The sender seals the rumor to each receiver, and wraps each seal
  separately (kind 13, then kind 1059).
- The set of the sender and the `p` tags names the room. A new `p` tag, or
  a removed one, makes a new room.
- A `subject` tag names the room. The newest `subject` wins.
- The sender also wraps a copy to itself. ARC does not: the sender opens
  its own seal ([the delivery spec](../delivery/SPEC.md), section 4.2).
- NIP-17 says that a chat with more than 10 participants should use
  another scheme, because each receiver costs one wrap.

## 3. Proposed rules

### 3.1 The rumor

- A group message MUST be one rumor of kind 14, with one `p` tag for each
  member other than the sender, in the order that the sender chose.
- A group has at most 10 participants, with the sender, as NIP-17
  advises. In ARC each member costs two wraps (courier and relay form) and
  one acknowledgement, and the outbox keeps at most 100 events for each
  recipient ([the delivery spec](../delivery/SPEC.md), section 4.2).
- The rumor MAY hold a `subject` tag.
- A direct message is the case with one `p` tag. Nothing changes for it.

### 3.2 Sealing and the outbox

- `core/mail` seals the one rumor to each member, as `prepare` does for one
  recipient today (`core/mail/mail.go:208`).
- The outbox key becomes the rumor id and the recipient. This changes the
  on-disk layout of the mail database. The first open MUST move each old
  entry to the new key in one transaction.
- An acknowledgement marks the entry of its author only. A group message is
  delivered when each member acknowledged it. The outbox shows the count,
  for example `delivered to 3 of 4`.

### 3.3 Reading

- The inbox returns the members of each message: the sender and the `p`
  tags.
- A receiver MUST check that its own key is in the `p` tags. It refuses a
  rumor that names it in no `p` tag.
- The message line of `arc message` gets a field `members`, a list of
  public keys. [The client programs spec](../clients/SPEC.md), section 3.5,
  allows a new field.

### 3.4 Commands

- `arc message send <key>,<key> <text>` sends one group message.
- `arc-dm` keys a conversation by the sorted set of its members. A reply in
  a group goes to each member.
- The interface spec, section 7.6, then allows a private publish to several
  recipients, with the same limit.

## 4. Compatibility

- An older arc opens a group message. Its inbox ignores the `p` tags, so it
  shows a direct message from the sender. A reply goes to the sender only.
  Nothing fails.
- An older sender sends only direct messages. A newer receiver shows each
  of them as a conversation of two.
- The outbox layout change is one way. An older arc cannot read the new
  outbox. The release notes MUST say so, as a breaking change.
- Kind 14 stays as it is. [The kind registry](../KINDS.md) needs no new
  row.

## 5. Open questions

- Should a member see who read the message? NIP-17 has no read receipt. The
  acknowledgement of ARC says only that the mail arrived.
- Should a group have a stable id, so that a change of members keeps the
  history in one view? NIP-17 says no: a new set is a new room.
- Should a list become a group by default? A list keeps working as a
  fan-out, and the members of a list do not see each other.

## 6. Proof that a build needs

- Three citizens on a relay: a group message reaches two members, and each
  sees all three keys.
- A reply from one member reaches the other two.
- The sender sees `delivered to 1 of 2` after one acknowledgement.
- An old outbox opens after the update, with each entry and its state.
- An older arc opens a group message as a direct message.
