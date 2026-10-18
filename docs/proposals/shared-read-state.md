Status: proposed

# One read state: arc keeps what a citizen read

No code implements this proposal. Each claim about ARC names a file and a
line at commit `02a731b`. It answers the first dogfood note of Otto: two
clients on one device disagree on what is read.

## 0. Bad news first

- The read state is local only. It stays in one arc home, and does not
  sync between the devices of a citizen: a message read on the phone is
  still unread on the Mac. This proposal does not change that.
- Listing is not reading. An agent that reads its mail with
  `arc message inbox --json` must still call `arc message read`, or the
  messages stay unread for arc-dm too. There is no `--mark-read`: the call
  that read is the one that marks.
- arc-dm then needs an arc with `arc message read`. With an older arc,
  arc-dm stops and names the version that it needs (section 5). It keeps
  no second read state as a fallback.
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
- `arc message read` marks messages read;
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
  inbox, and cannot be marked. `arc contacts add <stranger>` moves it to
  the inbox with the same id (`docs/strangers/SPEC.md:592`); from then on
  it can be marked, and it is unread until a client marks it.
- A mark of a message that is read already changes nothing: the first
  time stays.
- arc MUST NOT mark a message read on its own. `arc message inbox`,
  `arc message watch` and `arc sync` read the mail and mark nothing.

### 3.2 The commands

- `arc message read <id>...` marks messages read. Each id is 64 lowercase
  hex digits. With `-`, arc reads the ids from standard input, one on
  each line, so a client can mark a whole conversation.
- If an id is not a message of the inbox, or is not 64 lowercase hex
  digits, arc MUST refuse the whole call, and mark nothing. The error
  names each bad id of the call, not only the first:
  `2 ids are not messages of the inbox: <id>, <id>`.
- `--json` prints one line for each id: `{"id":…,"read_at":…}`.
- There is no `arc message unread` until a client needs one.
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
- `arcprogram.Arc.Read(ctx, ids)` runs `arc message read -`, with the
  ids on standard input.

### 3.5 arc-dm

- `arc-dm ls` and `open` take the read state from `read_at`.
- Only `open` and `read` mark, through `Arc.Read`. `arc-dm watch` marks
  nothing, as `arc message watch` marks nothing.
- `open` marks only the ids that it printed. A message that arrives
  between the listing and the mark is not among them, and stays unread.
- `read <peer>` marks the messages of the conversation that its own
  listing holds, by the same rule.
- At its first run with an arc that has `arc message read`, arc-dm moves
  its old `read.json` into arc, under the lock that it takes on that file
  now (`apps/dm/cmd/arc-dm/conversations.go:411`), so two runs of arc-dm
  do not both move it:
  1. If `read.json.moved` exists, arc-dm stops, and says so: it does not
     overwrite a file that an earlier move left.
  2. It marks each id of `read.json` that is in the inbox, in one call.
     An id that is not in the inbox, such as the id of a message that
     expired from a held list, is left out, and arc-dm says how many.
  3. It renames `read.json` to `read.json.moved`.
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
| Ids that are not in the inbox, or not 64 hex digits | arc refuses the call, and marks nothing. | `2 ids are not messages of the inbox: <id>, <id>`, with each bad id |
| arc-dm with an arc older than this proposal | arc-dm stops. | `arc-dm needs arc v0.35.0 or newer, which has arc message read; this arc has none` (the release that ships this sets the version) |
| Two clients mark at once | Each mark is a transaction; the first time stays. | Nothing. |
| An old `read.json` that does not parse | arc-dm stops, and leaves the file. | The path and the parse error. |
| `read.json.moved` exists already | arc-dm stops, and moves nothing. | `<dir>/read.json.moved exists: an earlier move left it; remove it or read.json` |
| Two runs of arc-dm move at once | The second waits for the lock, then finds no `read.json`. | Nothing. |

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
| `TestReadRefusesAnIdNotInTheInbox` | An unknown id, a sent message and a bad id are refused in one call; the error names each of them, and no id of the call is marked. |
| `TestHeldMailIsMarkableOnceAccepted` | A held message of a stranger is refused by `arc message read`. After `arc contacts add <stranger>` moves it to the inbox, with the same id, the same call marks it. |
| `TestTheFirstReadTimeStays` | A second mark keeps the first time. |
| `TestListingMarksNothing` | `arc message inbox`, `arc message watch`, `arc sync` and `arc-dm watch` leave each message unread. |
| `TestOpenMarksOnlyWhatItPrinted` | A message that arrives after `arc-dm open` listed the conversation, and before it marks, stays unread. The test puts the message in the store between the listing and the mark, through an arc wrapper that delivers it when arc-dm runs `arc message read`. |
| `TestArcDmMovesItsOldReadState` | A `read.json` of an older arc-dm moves into arc once, and the file is renamed. An id not in the inbox is left out and counted. |
| `TestTheMoveKeepsAnEarlierMove` | With `read.json.moved` there, arc-dm stops and changes neither file. |
| `TestTwoRunsMoveOnce` | Two runs of arc-dm at once: one moves, the other finds no `read.json`. |
| `TestTheVersionErrorNamesTheVersion` | With an arc that has no `arc message read`, arc-dm names the version that it needs. |
| `TestReadMarksAtOnce` (`core/mail`) | Two marks at once keep one time. |

## 9. Decisions of the review

Otto reviewed this proposal on 2026-10-18:

1. No `--mark-read` on `arc message inbox --json`: the call that read is
   the one that marks.
2. No sync between devices yet: the read state is local only (section 0).
3. No `arc message unread` until a client asks for it.
4. `arc-dm open` marks only the ids that it printed, and `arc-dm watch`
   marks nothing (section 3.5).
5. The refusal of a call names each bad id (section 3.2).
6. The move of `read.json` keeps an earlier move, and runs under the lock
   of arc-dm (section 3.5).
7. The error for an older arc names the version that arc-dm needs.
