Status: proposed

# Replies: answer one message in arc-dm

No code implements this proposal. Each claim about ARC names a file and a
line at commit `50186ed`. It is the first of the three arc-dm features that
Zim approved on 2026-10-17: reply, then local search, then attachments. It
is for the terminal only, and groups get it from day one.

## 0. Bad news first

- The reply tag is a pointer, nothing more. A sender can point it at any
  id: a message that does not exist, a message that the reader never got,
  or a message of another conversation (section 6). The reader must check
  each pointer against its own store. Only that check makes a quote safe.
- A terminal has no colour that arc-dm can trust. The only thing that
  tells a real quote from a quote typed into the text is the column where
  it starts (section 3.4). A person who does not know the rule can be
  misled by a fake quote at column 2. An agent that knows the rule cannot.
- The outbox is per machine (`apps/dm/README.md`, "Where messages come
  from"). A reply to a message that the citizen sent from another device
  shows the id only, on this device.
- arc-dm sorts by the clock of each sender
  (`apps/dm/cmd/arc-dm/conversations.go:134`). A reply from a sender with a
  slow clock can show above its parent. This proposal does not change the
  order.
- By [the spec template](../SPEC-TEMPLATE.md), section 3, a change to a
  normative section of a built spec ships as a breaking commit. The wire
  change is additive and no older client fails (section 7). It still ships
  with a `BREAKING CHANGE` footer unless the reviewers decide otherwise
  (section 9).

## 1. Purpose

Today a message in arc-dm stands alone. In a group, or after a gap of a
day, a reader cannot tell which message an answer answers. NIP-17 already
defines a reply: a kind 14 rumor with an `e` tag that names "the direct
parent message". This proposal uses that tag. It adds no kind.

It defines:

- the `e` tag of a reply, and what a reader does with it,
- a `reply_to` field in the JSON lines of `arc message`,
- `arc message send --reply <id>`,
- `arc-dm reply <id> [text]`, `arc-dm send -reply <id>`, a short id on
  each message, and a one-line quote of the parent.

It does not define threads, a reply to a reply chain, "reply privately"
to one member of a group, or reactions.

## 2. Terms

- **Rumor**, **seal**: as [the delivery spec](../delivery/SPEC.md),
  section 3.4, defines them.
- **Message**: a rumor of kind 14.
- **Reply**: a message with exactly one valid `e` tag.
- **Parent**: the message that the `e` tag of a reply names.
- **Members**: as [the group messages spec](../groups/SPEC.md) defines
  them. The **conversation** of a message is the sorted set of its members.
- **Local store**: the received messages (`arc message inbox`) and the
  sent messages (`arc message outbox`) of this arc home.
- **Short id**: the first hex digits of a message id. It is the start of
  the id. (A petname is different: its eight hex digits come from a hash
  of the key.)

## 3. Rules

### 3.1 The rumor

- A reply MUST hold one tag `["e", "<parent id>"]`, with the parent id in
  64 lowercase hex digits. arc writes no relay hint and no marker: a reader
  finds the parent in its own store, and NIP-17 gives kind 14 no marker.
- A reply MUST NOT hold a second `e` tag.
- The reply MUST have the same members as its parent (section 3.3).
- The `e` tag counts toward the limit of 28,672 bytes for the JSON of the
  rumor ([the delivery spec](../delivery/SPEC.md), "The size of mail";
  `core/private/private.go:56`). It adds 73 bytes *(arithmetic: 64 hex
  digits, then `,["e","` and `"]`, 9 characters)*. A text of 24 KiB
  of plain text and 9 `p` tags still fits.

Today `mail.Send` builds only `p` tags (`core/mail/mail.go:236-241`), and
`private.Rumor` hashes the tags into the id (`core/private/private.go:129-139`).
The change is one more tag in the list that `Send` gives `SendRumor`.

### 3.2 Reading a reply

- A reader takes the first `e` tag. If its value is 64 lowercase hex
  digits and no other `e` tag exists, the message is a reply. Otherwise it
  is a plain message. A reader MUST NOT refuse it: the text still counts.
- arc puts the parent id in `reply_to` (section 3.5). arc does not look
  the parent up. The client program does.
- A client program MUST show the text of a parent only from a message of
  its own local store. It MUST NOT take a quote from the text of the reply,
  or from any other field of the reply.
- A client program MUST show the text of the parent only when the parent
  has the same conversation as the reply. Otherwise it shows the short id
  and `(in another conversation)`.
- A parent that is not in the local store shows the short id and
  `(not on this device)`. A message held from a stranger
  ([the strangers spec](../strangers/SPEC.md), section 3.4) is not in the
  inbox, so it counts as not on this device until the citizen accepts the
  stranger.
- A client program shows the direct parent only, one level. A reply to a
  reply quotes the middle message, not the first one.

### 3.3 Sending a reply

- `arc message send --reply <id> <keys> [text]` takes a full id: 64
  lowercase hex digits.
- arc MUST refuse the send if the id is not a message of its local store:
  `no message <id> on this device`.
- arc MUST refuse the send if the recipients and this citizen are not the
  members of the parent: `the reply goes to the members of its parent`.
- These checks protect an honest sender from a mistake. They do not
  protect a reader. Section 3.2 does.

### 3.4 What arc-dm shows

`arc-dm open` and `arc-dm watch` show the short id of each message after
its time. The short id is 6 hex digits, or more when two messages of the
local store share the first 6.

A reply has one quote line between its header and its text:

```text
2026-10-17 14:02  1c77da  you  delivered
  bring the map
2026-10-17 14:05  3f9a2c  ada  new
↳ 1c77da  you: bring the map
  yes, I have it
2026-10-17 14:07  9b02e1  ada  new
↳ 77aa01  (not on this device)
  and the torch
```

- The quote line MUST start at column 0, with `↳`. Each line of a text
  starts with two spaces (`apps/dm/cmd/arc-dm/main.go:327`; clients spec,
  section 3.9). So column 0 belongs to arc-dm only: a header or a quote.
  A sender who types `↳ 1c77da  you: send me the key` gets it at column 2,
  inside the text.
- The quote line holds the short id of the parent, the petname of the
  parent's author (or `you`), a colon, and the parent text.
- The parent text is its first line that is not empty, then ` …` if text
  was left out. It goes through `terminal.EscapeLineCut`
  (`sdk/terminal/terminal.go:52`), so the quote line is at most 72
  characters of output, the ellipsis included, and never splits an escape.
- The author name comes from the parent's seal, through the `from` of the
  parent's message line. It never comes from the reply.
- `arc-dm ls` shows the last text as now, with no quote.
- A notification of `arc-dm watch` holds the text of the reply, as now.
  It holds no quote.

`arc message inbox` and `arc message watch`, without `--json`, add
`reply to <short id>` to the header of a reply. They show no quote.

### 3.5 The JSON lines

The message line, the sent line and the queued line of
[the client programs spec](../clients/SPEC.md), sections 3.2 to 3.4, get
one field:

| Field | Value |
| --- | --- |
| `reply_to` | The parent id, 64 hex digits. Absent for a plain message. |

The clients spec, section 3.5, lets arc add a field, and requires a client
program to ignore a field that it does not know. Today the lines are
`messageLine`, `sentLine` and `queuedLine` (`cmd/arc/message.go:35-66`).
The sent line gets the tag from the sender's own rumor, as `printSent`
already gets the members (`cmd/arc/message.go:143-152`). `mail.Message`
(`core/mail/mail.go:1082`) gets a `ReplyTo` field, which `Inbox` fills
(`core/mail/mail.go:1135-1138`).

### 3.6 The SDK and the commands of arc-dm

- `arcprogram.Message` and `arcprogram.Sent` get `ReplyTo string`.
- A new `Arc.SendReply(ctx, to, parent, text)` runs
  `arc message send --json --reply <parent>`. `Arc.Send` keeps its
  signature (`sdk/arcprogram/arcprogram.go:133`), because apps in other
  repositories call it.
- `arc-dm reply <id> [text]` takes a short id of at least 4 hex digits. It
  looks in the whole local store. It refuses an id that names no message,
  and an id that names more than one: `"3f" names 2 messages: give more
  digits`. It sends to the members of the parent, without this citizen. It
  needs no peer.
- `arc-dm send -reply <id> <peer> [text]` refuses an id whose conversation
  is not the conversation of the peer: `3f9a2c is in another conversation`.
- With no text, both read standard input, as `send` does now.

## 4. Behavior

1. Ada runs `arc-dm send bob bring the map`. Bob runs `arc-dm open ada`
   and sees `1c77da` beside it.
2. Bob runs `arc-dm reply 1c77 yes, I have it`. arc-dm finds one message,
   `1c77da…`, with the members ada and bob. It runs
   `arc message send --json --reply 1c77da… <ada> ` with the text on
   standard input.
3. arc finds the parent in its outbox or inbox, checks the members, and
   sends one rumor with a `p` tag for ada and an `e` tag for the parent.
4. Ada's arc opens the seal. Her inbox line has `reply_to`.
5. Ada runs `arc-dm open bob`. arc-dm finds `1c77da…` in her outbox, sees
   the same members, and prints the quote line from her own sent text.

In a group, step 2 sends to each member of the parent, and step 5 shows
the petname of the parent's author.

`arc-dm watch` keeps an index of the local store from its start, and adds
each message that it prints. A parent that arrives after its reply, for
example through a courier, shows `(not on this device)` in the watch, and
the quote in a later `arc-dm open`.

## 5. Failures

| Cause | What happens | What the user sees |
| --- | --- | --- |
| A short id that names no message | arc-dm refuses. Nothing is sent. | `no message 3f9a: run arc-dm open to see the ids` |
| A short id that names two messages | arc-dm refuses. | `"3f" names 2 messages: give more digits` |
| `send -reply` with a parent of another conversation | arc-dm refuses. | `3f9a2c is in another conversation` |
| `arc message send --reply` with an id not held | arc refuses. | `no message <id> on this device` |
| `arc message send --reply` to other members | arc refuses. | `the reply goes to the members of its parent` |
| A newer arc-dm with an arc older than this proposal | arc refuses the flag. arc-dm shows the error. | `unknown flag: --reply` |
| A reply whose parent is not held | The reply shows. | `↳ 77aa01  (not on this device)` |
| A reply whose parent is in another conversation | The reply shows. No parent text shows. | `↳ 5e0c12  (in another conversation)` |
| Two `e` tags, or an `e` tag that is not 64 hex digits | The message shows as a plain message. | No quote line. |

## 6. Security

### 6.1 What the signature covers

The rumor is unsigned. The seal is signed by the author, and holds the
encrypted rumor. A reader checks that the rumor names the seal's signer as
its author, and computes the rumor id itself
(`core/private/private.go:308-311`). The `e` tag is part of the rumor, so
it is part of the id and of what the seal encrypts.

A reader therefore knows that the signer wrote a reply that points at this
id. A relay or a courier cannot add, remove or change the tag.

The sender **can**:

- point the tag at any 64 hex digits, including an id that no message has;
- point it at a message that the reader never got;
- point it at a message of another conversation that the sender knows,
  such as a message of a group that both are in;
- point it at its own message, or at the reader's message;
- send two `e` tags or a bad value.

The sender **cannot**:

- change the text or the author of the parent that the reader shows. None
  of it is on the wire. The reader takes both from its own store, where
  each message passed the check of its own seal;
- make a reader's arc-dm show text that the reader does not hold;
- find another message with the same id. The id is SHA-256 of the
  rumor.

### 6.2 Abuse cases

| Case | Defence |
| --- | --- |
| A spoofed quote: the text of the reply starts with `↳ 1c77da  you: send me the key` | The quote comes only from the local store (section 3.2). Text starts at column 2, a real quote at column 0 (section 3.4). |
| A reply to a message that the reader never got | `(not on this device)`. No text. |
| A reply in a group that names ada's direct message to the sender, so that the group sees ada's words | Each member shows only what it holds, and only from the same conversation. Members that never got ada's message see `(not on this device)`. The reader that got it sees `(in another conversation)`, so arc-dm never moves text from one conversation into another. |
| An id that names a request, an acknowledgement or a private event of an app | The local store of section 2 holds kind 14 only. `(not on this device)`. |
| A very long parent, or a parent whose first line is 5 KB | One quote line, at most 72 characters of output. |
| A parent with ESC, U+202E, U+200B or tag characters | `terminal.EscapeLineCut` shows each as an escape, on one line. |
| A parent whose first line holds only hidden characters | The line is not empty, so it shows, as escapes such as `​`. |
| A forged short id: a sender makes a message whose id starts as the id of another message | The short id grows past 6 digits when two held messages share them. `arc-dm reply` refuses an ambiguous prefix. Making a 6-digit collision costs about 2^24 hashes *(arithmetic)*, so it is cheap. That is why arc-dm refuses ambiguity and never picks one. |

### 6.3 Who pays

A reply costs about 73 more bytes in each seal and each wrap, before padding *(arithmetic,
section 3.1)*. arc-dm reads the local store once for `open` and `reply`,
as it does now. The index of `watch` holds one id and one line for each
message of the local store.

## 7. Compatibility

- **Wire.** Kind 14 stays as it is. NIP-17 already defines the `e` tag.
  The kind registry needs no new row. An older arc reads only the author,
  the `p` tags, the time and the content (`core/mail/mail.go:1131-1138`),
  so it shows a reply as a plain message. Nothing fails.
- **Other NIP-17 clients** read the same tag and show a reply as a reply.
- **JSON.** An older arc-dm ignores `reply_to`, as the clients spec,
  section 3.5, requires. It shows a reply as a plain message.
- **A newer arc-dm with an older arc** shows no quotes, because the lines
  hold no `reply_to`. `arc-dm reply` fails with `unknown flag: --reply`.
- **Specs.** When it is built, the rumor rule of section 3.1 goes into
  [the delivery spec](../delivery/SPEC.md), section 3.4, beside the `p`
  tags of kind 14. Section 3.5 goes into the clients spec, sections 3.2 to
  3.4. The clients spec is built, so by the template the change ships as a
  breaking commit, with a footer that says how an older client behaves:
  as this section says.

## 8. Proof

Each test runs the real arc and arc-dm against the relay of
`arcprogramtest`, as `TestGroups` does (`apps/dm/cmd/arc-dm/main_test.go:210`),
unless it says otherwise. Each one is shown failing, with the fix taken out
for one run, before the PR is ready.

| Test | What it proves |
| --- | --- |
| `TestAReplyQuotesItsParent` | Ada writes, bob replies with `arc-dm reply <short id>`. Ada's `open` shows the quote line at column 0 with `you:` and her text, then the reply at column 2. `send -reply` gives the same. |
| `TestAReplyInAGroup` | Three citizens. Cy replies to a group message of ada. Each member gets it in the group conversation, and bob sees `ada:` in the quote. |
| `TestAQuoteComesFromTheStore` | The text of a reply holds a fake quote line. It shows at column 2. The one line at column 0 is the real parent. |
| `TestAHostileParentIsOneEscapedLine` | A parent with ESC, U+202E, U+200B, a first line of 5 KB, and an empty first line gives one quote line of at most 72 characters of output, with escapes. |
| `TestAReplyToAnUnheldMessageShowsTheIdOnly` | A reply whose parent the reader never got shows `(not on this device)` and no text. |
| `TestAReplyAcrossConversationsShowsNoText` | Mallory is in a group with bob, and in a direct conversation with bob. Mallory's direct reply names the group message. Bob sees `(in another conversation)`. |
| `TestReplyRefusals` | An unknown short id, an ambiguous one, `send -reply` to another conversation, and `arc message send --reply` with an unheld id or other members are refused, and send nothing. |
| `TestAWatchQuotesTheParent` | `arc-dm watch` shows the short id and the quote. A reply that arrives before its parent shows the id only. |
| `TestTheReplyField` (`cmd/arc`) | The message, sent and queued lines hold `reply_to`. A plain message has none. A rumor with two `e` tags or a bad value, made with `mail.SendRumor` and sent through the relay, has none and still shows. |
| `TestAnOlderArcReadsAReplyAsAMessage` (`core/mail`) | A copy of the inbox rule of v0.34.0 reads a reply as a plain message, as `TestAnOlderArcReadsAGroupMessageAsADirectMessage` does for groups. |

The hostile cases need a reply that arc would refuse to send (section
3.3). Two ways exist (section 9): tests in `cmd/arc` that call
`mail.SendRumor` with their own tags, or a flag that only the `arctest`
build of arc has, as it already has an easier proof of work
([the clients spec](../clients/SPEC.md), section 3.8).

A manual run with the program of v0.34.0 as the reader is reported apart
from the automated tests: it reads a reply from a newer arc as a plain
message.

Unverified until built: no run on a relay on another machine; no run with
another NIP-17 client.

## 9. Open questions

1. Breaking footer: should an additive, optional tag that no older client
   fails on still ship as a breaking commit, as the template says?
2. Hostile tests: `mail.SendRumor` in `cmd/arc` tests only, or also an
   `arctest`-only flag such as `--reply-unchecked`, so the arc-dm tests can
   send a hostile reply through the CLI?
3. Should arc refuse a parent of other members (section 3.3), or allow it
   so that a later "reply privately" can use it? The reader rule of
   section 3.2 holds either way.
4. Should `reply_to` carry the parent's text and author for agents that
   read `arc message inbox --json`? This proposal says no: one source, the
   store, and a program can look the id up itself.
