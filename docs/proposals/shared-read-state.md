Status: proposed

# One read state: arc keeps what a citizen read

No code implements this proposal. Each claim about ARC names a file and a
line at commit `02a731b`. It answers the first dogfood note of Otto: two
clients on one device disagree on what is read.

## 0. Bad news first

- The read state stays on one arc home, as today's arc-dm state stays on
  one device. It does not sync between the devices of a citizen. A
  message read on the phone is still unread on the Mac.
- Listing is not reading. An agent that reads its mail with
  `arc message inbox --json` must still call `arc message read`, or the
  messages stay unread for arc-dm too (section 9, question 1).
- arc-dm then needs an arc with `arc message read`. With an older arc,
  arc-dm fails with `unknown command "read"`, and says to update arc. It
  keeps no second read state as a fallback.
- The message line of the clients spec gets a field, `read_at`. The
  clients spec is built, so the change ships with a `BREAKING CHANGE`
  footer, as Zim decided for replies.

## 1. Purpose

Today arc keeps no read state. `arc message watch` "never marks a message
read" (`cmd/arc/message.go:261`), and the message line has no read field
(`cmd/arc/message.go:36`). arc-dm keeps its own set of read ids in
`read.json` in its state directory
(`apps/dm/cmd/arc-dm/conversations.go:364-386`), and marks them in
`open` and `read` (`apps/dm/cmd/arc-dm/main.go:352`, `:505`).

So a second client cannot see what arc-dm marked, and arc-dm cannot see
what a second client read. On 2026-10-18 arc-dm showed 66 unread messages
from Otto, while an agent had read each of them with
`arc message inbox --json`.

This proposal moves the read state into arc, so each client of one arc
home sees the same state:

- arc keeps the read time of each received message;
- `arc message read` marks messages read, and `arc message unread`
  unmarks them;
- the message line says when a message was read;
- arc-dm reads and writes that state, and keeps none of its own;
- arc-dm moves its old `read.json` into arc once.

It does not add read receipts. [The read-receipts
proposal](read-receipts.md) already plans `arc message read` as the one
place where a reader marks a message read; this proposal builds that
command first, with no receipt.

## 2. Terms

- **Read**: a client of this arc home marked the message read. It does
  not prove that a person or an agent looked at it.
- **Read state**: the read time of each received message of one arc home.
- **Client**: arc-dm, an agent, or any program that runs `arc message`.

## 3. Rules

### 3.1 The state

- arc MUST keep the read state in the mail database of the arc home, in a
  bucket `read`: the rumor id, and the UTC time at which a client marked
  it read. A mark and its check are one transaction.
- Only a message of the inbox can be read: a received message of kind 14
  that names this citizen in a `p` tag. Held mail of a stranger
  ([the strangers spec](../strangers/SPEC.md), section 3.4) is not in the
  inbox, and cannot be marked.
- A mark of a message that is read already changes nothing: the first
  time stays.
- arc MUST NOT mark a message read on its own. `arc message inbox`,
  `arc message watch` and `arc sync` read the mail and mark nothing.

### 3.2 The commands

- `arc message read <id>...` marks messages read. Each id is 64 lowercase
  hex digits. With `-`, arc reads the ids from standard input, one on
  each line, so a client can mark a whole conversation.
- If an id is not a message of the inbox, arc MUST refuse the whole call,
  and mark nothing: `no message <id> in the inbox`.
- `arc message unread <id>...` removes the mark, with the same rules.
- `--json` prints one line for each id: `{"id":…,"read_at":…}`.
- `arc message inbox --unread` lists only the messages that are not read.

### 3.3 The message line

The message line of [the client programs spec](../clients/SPEC.md),
section 3.2, gets one field:

| Field | Value |
| --- | --- |
| `read_at` | The time at which a client of this arc home marked the message read, RFC 3339, UTC. Absent while it is unread. |

`arc message inbox --json`, `arc message watch --json` and
`arc message held --json` print it. A held message never has it.

### 3.4 The SDK

- `arcprogram.Message` gets `ReadAt time.Time`, zero while unread.
- `arcprogram.Arc.Read(ctx, ids)` and `Arc.Unread(ctx, ids)` run
  `arc message read -` and `arc message unread -`, with the ids on
  standard input.

### 3.5 arc-dm

- `arc-dm ls`, `open` and `watch` take the read state from `read_at`.
  `open` and `read` mark through `Arc.Read`.
- At its first run with an arc that has `arc message read`, arc-dm finds
  its old `read.json`, marks each id in it that is in the inbox, and
  renames the file to `read.json.moved`. An id that is not in the inbox
  is left out.
- The flag `-state` and `DM_STATE` then name only where the old file is.

## 4. Behavior

1. An agent runs `arc message inbox --json --unread`, and gets 66 lines.
2. It answers them, and runs `arc message read -` with the 66 ids.
3. Zim runs `arc-dm ls`: the conversation with Otto shows no unread.
4. Zim runs `arc-dm open otto`: arc-dm shows the messages, and marks the
   ones that were still unread, through `arc message read -`.

## 5. Failures

| Cause | What happens | What the user sees |
| --- | --- | --- |
| An id that is not in the inbox | arc refuses the call, and marks nothing. | `no message <id> in the inbox` |
| An id that is not 64 hex digits | arc refuses the call. | `a message id is 64 lowercase hex digits` |
| arc-dm with an arc older than this proposal | arc-dm stops. | `arc message read: unknown command; update arc` |
| Two clients mark at once | Each mark is a transaction; the first time stays. | Nothing. |
| An old `read.json` that does not parse | arc-dm stops, and leaves the file. | The path and the parse error. |

## 6. Security

- The read state stays in the arc home, with the mail. It goes to no
  relay, no storage citizen and no other device.
- A client that can run arc with the home can mark and unmark, as it can
  now read and send. The read state gives it no new right.
- A sender learns nothing: no event leaves the machine. Receipts are the
  other proposal, and stay off by default.
- Cost: one key of 64 bytes and one time for each read message.

## 7. Compatibility

- An older arc prints no `read_at`. A client program that does not know
  the field ignores it, as the clients spec, section 3.5, requires.
- An older arc-dm keeps its own `read.json` and does not see marks of
  other clients, as today.
- A newer arc-dm needs a newer arc (section 0).
- When it is built, section 3.1 goes into [the delivery
  spec](../delivery/SPEC.md), beside the outbox, and sections 3.2 to 3.4
  into the clients spec. The commit carries `!` and a `BREAKING CHANGE`
  footer that says how an older client behaves.

## 8. Proof

Each test runs the real arc and arc-dm against the relay of
`arcprogramtest`, unless it says otherwise.

| Test | What it proves |
| --- | --- |
| `TestAReadMarkIsSharedByEachClient` | An agent marks with `arc message read`; `arc-dm ls` shows no unread. `arc-dm open` marks; `arc message inbox --json` shows `read_at`, and `--unread` leaves the message out. |
| `TestReadRefusesAnIdNotInTheInbox` | An unknown id, a sent message, a held message and a bad id are refused, and no id of the call is marked. |
| `TestTheFirstReadTimeStays` | A second mark keeps the first time; `unread` removes it. |
| `TestListingMarksNothing` | `inbox`, `watch` and `sync` leave each message unread. |
| `TestArcDmMovesItsOldReadState` | A `read.json` of an older arc-dm moves into arc once, and the file is renamed. |
| `TestReadMarksAtOnce` (`core/mail`) | Two marks at once keep one time. |

## 9. Open questions

1. Should `arc message inbox --json` take a flag, such as `--mark-read`,
   that marks each line that it prints, so an agent needs one call? This
   proposal says no for now: a listing is not reading, and the call that
   marks should be the one that read.
2. Should the read state sync between the devices of a citizen, as self
   data through the storage citizens? It would need a merge rule for two
   devices. This proposal leaves it for later.
3. Is `arc message unread` worth having now? arc-dm has no command that
   needs it.
