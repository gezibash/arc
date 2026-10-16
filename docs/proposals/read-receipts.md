Status: proposed

# Read receipts: tell a sender that a message was read

No code implements this proposal. Each claim about ARC names a file or a
spec section at commit `4d530e5`. Each number has a tag:

- *measured*: a run on a real machine. The source is given.
- *arithmetic*: a calculation from other numbers. The inputs are given.
- *estimated*: a judgement. No run supports it.

## 0. Bad news first

- A receipt says that a device of the recipient marked a message read. It
  cannot prove that a person, or an agent, looked at it. A client can mark
  a message read without showing it, or show it and never send a receipt.
- Receipts are mutual only by agreement. A client that sends no receipts
  can still show the receipts that it gets. arc keeps the rule of section
  3.1, but a changed client can break it, and nobody can detect that.
- A receipt tells the sender when the reader was active, to the minute
  (section 3.3). That is the cost that a citizen accepts when it turns
  receipts on.
- The read state of `arc-dm` stays on one device (`apps/dm/README.md`).
  A receipt that one device sends does not mark the message read on the
  other devices of the citizen. This proposal does not sync read state.
- An arc older than this proposal acknowledges a receipt as if it were a
  message (section 7). That costs one extra wrap, and nothing else.

## 1. Purpose

The outbox shows `delivered` when the recipient's arc stored the message.
arc acknowledges a message as soon as it stores it in a sync, not when
someone opens it (`core/mail/mail.go:678-682`, kind 3274,
[the delivery spec](../delivery/SPEC.md), section 4.2). A sender that sees
`delivered to 1 of 2` cannot tell a member that did not sync from a member
that synced and did not read. Otto, an agent, read `delivered` as `read`
on 2026-10-16, and was misled.

This proposal defines an optional read receipt:

- a setting that a citizen turns on, off by default,
- the event that a reader sends when it marks messages read,
- what the sender learns, and what it does not,
- how `arc message outbox` and `arc-dm` show it, for example
  `delivered 2/2 · read 1/2` in a group.

It does not define a typing signal, a "seen" state for a live call, read
state that syncs between the devices of one citizen, or a receipt for
mail of a capability.

Prior art: Signal makes receipts optional and mutual: both sides must turn
them on to see them
([Signal support](https://support.signal.org/hc/en-us/articles/360007059812-Read-Receipts),
checked 2026-10-16). Matrix added a private receipt that clears the
reader's own unread count and goes to nobody else
([MSC2285](https://github.com/matrix-org/matrix-spec-proposals/blob/main/proposals/2285-hidden-read-receipts.md)).
ARC already has the private kind: the read state of `arc-dm` never leaves
the device.

## 2. Terms

| Term | Meaning |
| --- | --- |
| delivered | The recipient's arc stored the message and sent an acknowledgement (kind 3274). |
| read | A device of the recipient marked the message read, and sent a receipt. |
| receipt | A rumor of kind 3277, inside a gift wrap, that names messages that its author read. |
| receipts setting | The choice of a citizen to send and to show receipts: `on` or `off`. |

## 3. Rules

### 3.1 The setting

- The setting is `off` unless the citizen turns it on:
  `arc message receipts on`, and `arc message receipts off`.
- The setting is self data of the citizen. It syncs through the storage
  citizens of the citizen, and never goes to a relay, as the contact list
  does ([the strangers spec](../strangers/SPEC.md), section 3.1). So each
  device of the citizen follows it.
- While the setting is `off`, arc MUST NOT send a receipt, and
  `arc message outbox` MUST NOT show a read state. arc still keeps the
  receipts that arrive, so a citizen that turns the setting on sees the
  receipts of the past. A receipt that arrives says nothing to the reader.

### 3.2 When a reader sends a receipt

- A client marks messages read with `arc message read <id>...`, or
  `arcprogram.Arc.Read`. `arc-dm open` and `arc-dm read` call it for each
  message that they mark read. arc does not mark a message read on its own:
  `arc message inbox` and `arc message watch` send no receipt.
- arc sends a receipt for a message only when all of these hold:
  1. The setting of the reader is `on`.
  2. The message is of kind 14, and is in the inbox. Held mail is not in
     the inbox ([the strangers spec](../strangers/SPEC.md), section 3.4),
     so it never gets a receipt.
  3. The author of the message is a contact of the reader. A stranger
     whose work let a message in learns nothing more.
  4. arc sent no receipt for that message before.
- arc MUST send one receipt for each author, which names each message of
  that author that the call marked read: at most 100 `e` tags in one
  receipt (estimated), and more receipts for more messages.

### 3.3 The receipt

- A receipt is a rumor of kind 3277, with empty content, one `e` tag for
  each message that it names, and a `p` tag that names the author of the
  messages. Its `created_at` is the time at which arc made it, rounded down
  to the minute.
- arc seals and wraps it as it does an acknowledgement: relay form and
  courier form, kind 1059, an expiry of 7 days (`core/private/private.go`).
- In a group, a receipt goes to the author of the message only. The other
  members learn nothing from it.
- arc MUST NOT acknowledge a receipt, and MUST NOT put it in the outbox. A
  receipt is sent once, at best effort, like an acknowledgement.

### 3.4 When a sender gets a receipt

- arc MUST check that the author of the receipt is a recipient of each
  message that it names: the outbox has the entry `<rumor id>:<author>`
  ([the delivery spec](../delivery/SPEC.md), section 4.2). It ignores
  each `e` tag that names another message.
- For each entry that passes, arc keeps the time of the receipt as the
  read time of the entry. A later receipt for the same entry changes
  nothing.
- A receipt marks the entry delivered too, if no acknowledgement came. A
  device that read the message stored it.

### 3.5 What arc and arc-dm show

- The text of `arc message outbox`, while the setting is `on`:

  | Message | Shows |
  | --- | --- |
  | direct, not acknowledged | `pending` |
  | direct, acknowledged | `delivered` |
  | direct, with a receipt | `read` |
  | group | `delivered 2/2 · read 1/2`: the members that acknowledged, then the members that sent a receipt, each of all the recipients |

  While the setting is `off`, the group form is `delivered 2/2`, and a
  direct message shows `delivered` at most.
- The sent line of `arc message outbox --json`
  ([the clients spec](../clients/SPEC.md)) gains the field `read_at`: the
  time of the receipt of that recipient, or absent. It is absent while the
  setting is `off`. The field `state` keeps its values: `pending`,
  `delivered`, `expired`.
- `arc-dm open` shows the same text after each message of the citizen.
  `arc-dm ls` shows no read state.

## 4. Behavior

1. Zim turns receipts on. Otto turns receipts on.
2. Otto sends a group message to Zim and Alfred. Alfred has receipts off.
3. Both devices sync and acknowledge. Otto's outbox shows
   `delivered 2/2 · read 0/2`.
4. Zim runs `arc-dm open otto,alfred`. arc-dm calls `arc message read` for
   the message. Zim's arc sends one receipt to Otto, at 18:42.
5. Otto's arc gets it in a sync, checks that Zim is a recipient, and keeps
   18:42 as Zim's read time. Otto's outbox shows `delivered 2/2 · read 1/2`.
6. Alfred opens the group. His setting is off, so his arc sends no
   receipt. Otto never sees Alfred read it.

## 5. Failures

| Failure | What happens |
| --- | --- |
| The reader has receipts off | No receipt. The sender sees `delivered` at most. |
| The sender has receipts off | Receipts arrive and are kept, and are not shown. |
| The receipt names a message that the author did not get | arc ignores that `e` tag. |
| The receipt is lost, or expires before the sender syncs | The sender sees `delivered`, as if the reader had receipts off. arc does not send the receipt again. |
| The reader reads on two devices | The first device to mark the message read sends the receipt. The second sends none for it if it knows of the first; if not, the sender gets two receipts and keeps the first time. |
| A stranger's message got in with work | No receipt, until the reader adds the stranger as a contact and marks the message read again. |
| A sender on an arc older than this proposal | Its arc acknowledges the receipt as a message, and shows nothing (section 7). |

## 6. Security

### 6.1 What each party learns

| Party | Learns | Does not learn |
| --- | --- | --- |
| The sender | For each message and member with receipts on: that a device of the member marked it read, and the minute. | Which device, how long the member looked, whether a person or an agent read it, what the member read in other conversations. |
| Another member of a group | Nothing from a receipt. | Who read the message. |
| The relay | One more wrap to the sender, of about the size of an acknowledgement. | The kind, the messages, or that it is a receipt. |
| A stranger | Nothing: receipts go only to contacts. | Whether its message was read. |

### 6.2 Timing

The relay already sees that a wrap goes to the sender when the reader
syncs, because the reader acknowledges. A receipt adds a second wrap when
the reader opens the conversation. A relay that compares the two times
learns when the reader looked at mail from that sender. The minute in the
rumor tells the sender the same. A citizen that does not want to share
this keeps receipts off, which is the default.

### 6.3 Who pays

- A receipt costs the reader one wrap for each author in each `read` call,
  and the relay keeps it for at most 7 days: about 1.5 KiB (estimated, the
  size of an acknowledgement wrap).
- The sender pays nothing more: it would sync anyway.

## 7. Compatibility

- Kind 3277 is new. It needs a row in [the kind registry](../KINDS.md):
  regular, inside a gift wrap, because it is mail that waits for the
  sender's sync.
- An arc older than this proposal treats an unknown private kind as mail
  of a capability: it stores it and acknowledges it
  (`core/mail/mail.go:678-682`). So an old sender's arc sends one
  acknowledgement back for each receipt. The reader's arc finds no outbox
  entry for it, and ignores it. The old sender sees `delivered`, as today.
- `state` in the JSON lines keeps its values, so a client program that
  reads them keeps working. A client that ignores `read_at` shows no read
  state.
- Removal: turn the setting off. Stored read times stay in the outbox until
  their entries expire.

## 8. Proof

Planned tests, each through the real relay and the normal CLI:

- With both settings `on`, `arc-dm open` on the reader makes the sender's
  outbox show `read`, and `read_at` in the JSON line.
- In a group of three where one member has receipts off, the sender sees
  `delivered 2/2 · read 1/2`, and the other members see no read state.
- With the reader's setting `off`, no wrap of kind 3277 leaves the reader.
- With the sender's setting `off`, the outbox shows no read state, and
  turning it on shows the receipt that arrived before.
- A receipt that names a message whose recipient is not its author changes
  nothing.
- A message from a stranger gets no receipt; after `arc contacts add` and a
  second `arc-dm read`, it gets one.
- `created_at` of a receipt is a whole minute.
- A receipt gets no acknowledgement from a new arc.

## 9. Open questions

- Is a minute the right grain for `created_at`, or should it be an hour?
- Should a citizen be able to turn receipts off for one conversation?
- Should an agent that reads with `arc message inbox --json` send receipts?
  This proposal says no: only an explicit `arc message read` does.
