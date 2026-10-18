Status: proposed

# One read state: arc keeps what a citizen read

No code implements this proposal. Each claim about ARC names a file and a
line at commit `02a731b`. It answers the first dogfood note of Otto: two
clients of one arc home disagree on what is read.

## 0. Bad news first

- The read state is local only. It stays in one arc home, and does not
  sync between the devices of a citizen: a message read on the phone is
  still unread on the Mac. This proposal does not change that.
- Listing is not reading. An agent that reads its mail with
  `arc message inbox --json` must still call `arc message read`, or the
  messages stay unread for arc-dm too. There is no `--mark-read`: the call
  that read is the one that marks.
- `arc-dm open` marks only when it writes to a terminal. A program that
  runs `arc-dm open` through a pipe marks nothing, and must call
  `arc-dm read` or `arc message read`.
- arc-dm then needs an arc with `arc message read`. With an older arc,
  arc-dm stops and names the version that it needs (section 5). It keeps
  no second read state as a fallback.
- The state directory of arc-dm must be local and used by one user. Its
  lock is an flock beside the file, which does not hold over NFS, and an
  older arc-dm writes the file with no lock.
- The message line of the clients spec gets a field, `read_at`. The
  clients spec is built, so the change ships with a `BREAKING CHANGE`
  footer, as Zim decided for replies.

## 1. Purpose

Today arc keeps no read state. `arc message watch` "never marks a message
read" (`cmd/arc/message.go:261`), and the message line has no read field
(`cmd/arc/message.go:36`). arc-dm keeps its own set of read ids in
`read.json` in its state directory
(`apps/dm/cmd/arc-dm/conversations.go:364-386`), and marks them in
`open` and `read` (`apps/dm/cmd/arc-dm/main.go:352`, `:505`). One
`read.json` serves each arc home that uses the state directory
(`conversations.go:357-359`).

So a second client cannot see what arc-dm marked, and arc-dm cannot see
what a second client read. On 2026-10-18 arc-dm showed 66 unread messages
from Otto, while an agent had read each of them with
`arc message inbox --json`. The agent and arc-dm used the same arc and
the same arc home: the default, `~/.config/arc`, the citizen
`tidal-picard-c614cd5b`.

This proposal moves the read state into arc, so each client of one arc
home sees the same state:

- arc keeps the read time of each received message;
- `arc message read` marks messages read;
- the message line says when a message was read;
- arc-dm reads and writes that state, and keeps none of its own;
- each arc home imports the old `read.json` of arc-dm once.

It does not add read receipts. [The read-receipts
proposal](read-receipts.md) already plans `arc message read` as the one
place where a reader marks a message read; this proposal builds that
command first, with no receipt.

## 2. Terms

- **Read**: a client of this arc home marked the message read. It does
  not prove that a person or an agent looked at it.
- **Read state**: the read time of each received message of one arc home.
- **Received index**: the ids of the received messages of one arc home,
  which arc keeps beside the read state.
- **Client**: arc-dm, an agent, or any program that runs `arc message`.

## 3. Rules

### 3.1 The state

- arc MUST keep the read state in the mail database of the arc home, in a
  bucket `read`: the rumor id, and the UTC time at which a client marked
  it read.
- arc MUST keep a received index in a bucket `received`: the id of each
  message of the inbox. arc writes an id in the transaction that admits
  the message to the inbox: when a message arrives, and when
  `arc contacts add` releases held mail. The first open of an older
  database builds the index once, from the stored seals. So a mark checks
  ids against the index, and decrypts nothing.
- Only a message of the inbox can be read: a received message of kind 14
  that names this citizen in a `p` tag. Held mail of a stranger
  ([the strangers spec](../strangers/SPEC.md), section 3.4) is not in the
  inbox, and cannot be marked. `arc contacts add <stranger>` moves it to
  the inbox with the same id (`docs/strangers/SPEC.md:592`); from then on
  it can be marked, and it is unread until a client marks it.
- A mark and its check are one transaction. A mark of a message that is
  read already changes nothing: the first time stays.
- When a message leaves the inbox, arc deletes its read row and its index
  entry in the same transaction. The sweep of expired mail also deletes
  each read row whose id is not in the index.
- arc MUST NOT mark a message read on its own. `arc message inbox`,
  `arc message watch` and `arc sync` read the mail and mark nothing.

### 3.2 The commands

- `arc message read <id>...` marks messages read. With `-`, arc reads the
  ids from standard input, one on each line, so a client can mark a whole
  conversation.
- If an id is not 64 lowercase hex digits, arc MUST refuse the whole
  call, and mark nothing. The error names each such id.
- arc marks each other id that is in the inbox, and skips each id that is
  not. It names each skipped id on standard error:
  `skipped 2 ids that are not in the inbox: <id>, <id>`. The call
  succeeds.
- `--json` prints one line for each id: `{"id":…,"read_at":…}` for a
  marked id, and `{"id":…,"error":"not in the inbox"}` for a skipped one.
- `--import <name>` marks the ids as an import, and records `<name>` in
  the bucket `read-imports` of the arc home, in the same transaction.
  `arc message read --imported <name>` prints `yes` or `no`, and marks
  nothing.
- `arc message read --check` prints nothing, marks nothing, and exits 0:
  a client probes for the command with it. An older arc fails on it.
- `arc message inbox --unread` lists only the messages that are not read,
  and `--limit <n>` lists at most the oldest n of them.
- There is no `arc message unread` until a client needs one.

### 3.3 The message line

The message line of [the client programs spec](../clients/SPEC.md),
section 3.2, gets one field:

| Field | Value |
| --- | --- |
| `read_at` | The time at which a client of this arc home marked the message read, RFC 3339, UTC. Absent while it is unread. For an id that an import marked, it is the time of the import. |

`arc message inbox --json`, `arc message watch --json` and
`arc message held --json` print it. A held message never has it.

The clients spec also says, for a program that reads mail: mark each
message after you handle it, with `arc message read`, at least once. A
second mark changes nothing, so a program that crashes between the
handling and the mark marks again when it starts.

### 3.4 The SDK

- `arcprogram.Message` gets `ReadAt time.Time`, zero while unread.
- `arcprogram.Arc.Read(ctx, ids)` runs `arc message read --json -`, with
  the ids on standard input, and returns the ids that it skipped.
- `arcprogram.Arc.CanRead(ctx)` runs `arc message read --check`.

### 3.5 arc-dm

- `arc-dm ls` and `open` take the read state from `read_at`.
- Only `open` and `read` mark, through `Arc.Read`. `arc-dm watch` marks
  nothing, as `arc message watch` marks nothing.
- `open` marks only the ids that it printed, and only after the writes of
  them succeeded. A message that arrives between the listing and the mark
  is not among them, and stays unread. When standard output is not a
  terminal, `open` marks nothing: a program that reads its output marks
  with `arc-dm read`.
- `read <peer>` marks the messages of the conversation that its own
  listing holds, by the same rule.
- arc-dm imports its old `read.json` into each arc home that it uses:
  1. It asks `arc message read --imported arc-dm`. If the answer is
     `yes`, it does nothing.
  2. Under the lock that it takes on `read.json` now
     (`conversations.go:411`), it reads the file. A missing file and an
     empty file are an empty set.
  3. It runs `arc message read --import arc-dm -` with each id of the
     file. arc marks the ids that are in this home's inbox, skips the
     others, and records the import in the same transaction.
  4. It leaves `read.json` in place, and never writes it again. Another
     arc home that uses the same state directory imports from it on its
     own first run, and marks the ids of its own inbox.
- A crash at any point of the import is safe. A mark and the record of
  the import are one transaction, and a second import marks again with
  no change: the first read time stays.
- The flag `-state` and `DM_STATE` then name only where the old file is.

## 4. Behavior

1. An agent runs `arc message inbox --json --unread --limit 20`.
2. It answers each message, and after each answer it runs
   `arc message read <id>`.
3. Zim runs `arc-dm ls`: the conversation with Otto shows no unread.
4. Zim runs `arc-dm open otto` on a terminal: arc-dm shows the messages,
   and marks the ones that it printed and that were still unread.

## 5. Failures

| Cause | What happens | What the user sees |
| --- | --- | --- |
| An id that is not 64 hex digits | arc refuses the call, and marks nothing. | `2 ids are not message ids: <id>, <id>` |
| Ids that are not in the inbox | arc marks the others, and skips them. | `skipped 2 ids that are not in the inbox: <id>, <id>` on standard error, or an `error` field with `--json` |
| arc-dm with an arc older than this proposal | `arc message read --check` fails, and arc-dm stops. | `arc-dm needs arc v0.35.0 or newer, which has arc message read; this arc has none` (the release that ships this sets the version) |
| Two clients mark at once | Each mark is a transaction; the first time stays. | Nothing. |
| An old `read.json` that does not parse | arc-dm stops, and leaves the file. | The path and the parse error. |
| A crash during an import | The next run imports again. | Nothing: the read times stay. |
| Two runs of arc-dm import at once | The second waits for the lock, then finds the import recorded. | Nothing. |

## 6. Security

- The read state stays in the arc home, with the mail. It goes to no
  relay, no storage citizen and no other device.
- A client that can run arc with the home can mark, as it can now read
  and send. The read state gives it no new right.
- A sender learns nothing: no event leaves the machine. Receipts are the
  other proposal, and stay off by default.
- A mark decrypts nothing and does not wait on a watch for long: it reads
  the received index and writes one row for each id, in one short
  transaction.
- Cost: for each received message, one id in the index; for each read
  message, one id and one time.

## 7. Compatibility

- An older arc prints no `read_at`. A client program that does not know
  the field ignores it, as the clients spec, section 3.5, requires.
- An older arc-dm keeps its own `read.json` and does not see marks of
  other clients, as today. A newer arc-dm reads that file and never
  writes it.
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
| `TestAReadMarkIsSharedByEachClient` | An agent marks with `arc message read`; `arc-dm ls` shows no unread. `arc-dm open` on a terminal marks; `arc message inbox --json` shows `read_at`, and `--unread` leaves the message out. |
| `TestReadSkipsWhatIsNotInTheInbox` | An unknown id and a sent message are skipped and named on standard error and in `--json`; the other ids of the call are marked. A badly formed id refuses the whole call. |
| `TestHeldMailIsMarkableOnceAccepted` | A held message of a stranger is skipped by `arc message read`. After `arc contacts add <stranger>` moves it to the inbox, with the same id, the same call marks it. |
| `TestAcceptRacingReadIsWhole` | `arc contacts add` and `arc message read` at once, many times: each run marks the message or skips it, and never leaves a mark without an index entry. |
| `TestTheFirstReadTimeStays` | A second mark keeps the first time. |
| `TestListingMarksNothing` | `arc message inbox`, `arc message watch`, `arc sync` and `arc-dm watch` leave each message unread. |
| `TestOpenMarksOnlyWhatItPrinted` | A message that arrives after `arc-dm open` listed the conversation, and before it marks, stays unread. An arc wrapper delivers it when arc-dm runs `arc message read`. |
| `TestOpenThroughAPipeMarksNothing` | `arc-dm open` with its output in a pipe leaves each message unread. |
| `TestEachHomeImportsTheOldReadState` | Two arc homes share one state directory. Each imports `read.json` on its first run, and marks the ids of its own inbox; the file stays as it was. An empty file is an empty set. |
| `TestAKilledImportIsSafe` | arc-dm killed during an import, then run again: each id is marked once, with the time of the first mark. |
| `TestTheVersionErrorNamesTheVersion` | With an arc that fails `arc message read --check`, arc-dm names the version that it needs. |
| `TestTheReadStateFollowsTheInbox` (`core/mail`) | A message that leaves the inbox leaves the read state; the sweep deletes a read row with no index entry. |
| `TestReadMarksAtOnce` (`core/mail`) | Two marks at once keep one time. |

## 9. Decisions of the review

Otto reviewed this proposal twice on 2026-10-18:

1. No `--mark-read` on `arc message inbox --json`: the call that read is
   the one that marks.
2. No sync between devices yet: the read state is local only (section 0).
3. No `arc message unread` until a client asks for it.
4. `arc-dm open` marks only the ids that it printed, after it wrote them,
   and only on a terminal; `arc-dm watch` marks nothing (section 3.5).
5. A call marks the ids of the inbox and names each id that it skipped;
   only a badly formed id refuses the whole call (section 3.2). This
   replaces the refusal of the first review.
6. The import leaves `read.json` in place, read only, because it serves
   each arc home that uses the state directory. Each home imports the ids
   of its own inbox, and arc records the import. This replaces the move
   to `read.json.moved` of the first review.
7. The error for an older arc names the version that arc-dm needs, after
   a probe with no side effect, `arc message read --check`.
8. A mark checks a received index, and decrypts nothing.
