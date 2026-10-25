# Client programs

- Status: built
- Layers: sdk
- Owns: sdk/arcprogram, sdk/arcprogram/arcprogramtest, sdk/terminal
- Proof: go test -count=1 -run '^(TestMessages|TestGroupMessages|TestSendRefusesANameAsTheRecipient|TestWatch|TestWatchFailsWithABadSince|TestEscape|TestEscapeLine|TestEscapeLineCut|TestEscapeData|TestEscapeJSON|TestStrip|TestStripCases|TestStripKeepsOneSelector|TestStripAndEscapeAgreeAfterABackslash|TestEscapeLineCutNeverPassesTheLimit|TestBlank|TestATerminalShowsNoEscapeOfASender|TestHeldAndWatchedTextIsEscaped|TestServeTakesMailFromAWatch|TestALiveReplyIsEscapedWhereverItGoes|TestAnErrorLineCannotReachTheTerminal|TestARelayRefusalIsEscapedOnOneLine|TestARingRefusalIsEscapedOnOneLine|TestAStorageAnswerIsEscaped|TestNoticeIsCutAndEscaped|TestTextOfOtherPartiesIsEscapedAtThePrint|TestTheEscapeCheckFindsRawPrints|TestSlogTextHandlerQuotesHostileText|TestReplies|TestAReleaseBuildHasNoTestTags|TestAReplyQuotesItsParent|TestAReplyInAGroup|TestAQuoteComesFromTheStore|TestAHostileParentIsOneEscapedLine|TestAReplyToAnUnheldMessageShowsTheIdOnly|TestAReplyAcrossConversationsShowsNoText|TestATagThatIsNoReplyShowsAPlainMessage|TestReplyRefusals|TestAWatchQuotesTheParent|TestShortIDs|TestAReplyToYourOwnMessage|TestSendReplyChecksTheIdBeforeTheText|TestAWatchReadsTheInboxOnlyAtItsStart|TestTheOutboxIsReadAtMostOnceEachRefreshFromTheEndOfTheLast|TestTheWatchForgetsMissedIds|TestALiveAddDoesNotSortTheStore|TestAWatchHoldsAReplyUntilItReadsTheOutbox|TestTheHeldQueueIsCapped|TestOneSendersJunkDoesNotHideAValidReply|TestRotatingKeysDoNotHideAValidReply|TestQuickHonestRepliesKeepQuotesAndOrder|TestASenderOverItsQuotaPrintsItsOldest|TestSmallHoldersWaitForTheRead|TestManyKeysDoNotHideAnHonestSender|TestAnEarlyReplyMarksNoParentMissed|TestASealedKeyAsksOnTheTerminal|TestAHangupEndsEachArc|TestSIGTERMEndsEachArc|TestAKilledArcDmLeavesNoArcOnLinux|TestAShownMissingIdIsRefused|TestOnlyPlainHeadsShowEarly|TestAFailedReadMarksNothingMissed|TestAWatchReadTimesOut|TestAfterAFailedReadNothingShowsAsMissing|TestAnAlwaysHungOutboxShowsRepliesPlain|TestAWatchSavesShownIdsAtItsEnd|TestACallEndsWhenArcEnds|TestACallOutlivesTheEndOfOtherThreads|TestAPanicInAWatchEndsItsArc|TestAWatchOutlivesTheEndOfOtherThreads|TestReplyRefusesAnIdShownAsMissing|TestABusyGroupDoesNotHideAValidReply|TestAnOverflowReadsAnOldOutbox|TestCraftedCollisionsStayFast|TestASecondInterruptEndsTheWatch|TestTheEndOfAWatchReadsTheOutbox|TestALiveFloodIsLinear|TestAWatchShowsAHeldReplyAtItsEnd|TestAddIsNotQuadratic|TestAnIdIsCheckedBeforeArcRuns|TestWidth|TestEscapeLineFit|TestFlagsAfterThePeer|TestArcChecksAReplyBeforeTheText|TestIndexIsNotQuadratic|TestABlankParentQuotesAsBlank|TestRepliesShowSoonWhileTheOutboxHangs|TestTakeNeverReads|TestReadsRecoverAfterAFailure|TestReadsBackOffFromTheEndOfTheLastRead|TestACompactionDuringAReadKeepsALateReplyWaiting|TestASaveRemovesTheTemporaryFileOfAKilledSave|TestAKilledWatchKeepsTheIdsThatAReadShowed|TestAWarningOfArcShowsOnceAndAnErrorIsOneLine|TestEscapeLineData)$' ./sdk/arcprogram/ ./sdk/terminal/ ./cmd/arc/ ./runtime/citizen/ ./internal/architecture/ ./apps/dm/...
- Unverified: No test runs a client program against a relay other than a relay on the same machine. No test reads a reply from another NIP-17 client. No test reads the outbox of a citizen that sent a request or a private event of an app kind. No test runs a client program on Windows, where arc cannot get an interrupt.

The package `sdk/arcprogram` runs the arc program for a client program. It
reads the JSON lines that `arc message` prints. The package
`sdk/arcprogram/arcprogramtest` runs a relay on this machine for the tests
of a client program. The package `sdk/terminal` makes the text of a sender
safe to show on a terminal.

## 1. Purpose

A client program is a program of an app that a person runs, such as
`arc-transfer` or `arc-dm`. It holds no key. It asks arc to sign, seal,
send and read, so arc keeps the key of the citizen, and its consent rules.

This spec defines:

- how a client program runs arc, in an arc home,
- the JSON lines of `arc message`: the received messages, the sent
  messages, and the queued message,
- how a client program sends a message and watches for messages,
- how a client program sends a reply, and shows the parent of a reply,
- the relay on this machine that the tests of a client program use,
- how arc and a client program show the text of a sender on a terminal.

Programs outside this repository read the JSON lines. Without a rule, a
change of a field in arc breaks them with no error. Before this spec, each
client program had its own copy of the code that runs arc.

This spec does not define an event on the wire. The mail layer of
[the delivery spec](../delivery/SPEC.md) carries each message. This spec
does not give a client program a way to read the keys of the arc home.

## 2. Terms

- **Client program**: a program that runs arc as a child process, and reads
  what arc prints.
- **Arc home**: the directory that holds the keys, the store and the mail of
  one citizen on one machine. The default is the home that arc picks.
- **JSON line**: one JSON object, then one newline. arc does not escape
  HTML characters in it.
- **Message line**: the JSON line of one received message.
- **Sent line**: the JSON line of one message that this citizen sent.
- **Queued line**: the JSON line of the message that `arc message send`
  put in the outbox.
- **Message**: a NIP-17 private message, kind 14, as
  [the delivery spec](../delivery/SPEC.md), section 4.2, defines it.
- **Members**: the sender of a message, then each recipient, as
  [the group messages spec](../groups/SPEC.md) defines them. A direct
  message has two members.

## 3. Rules

### 3.1 Running arc

- The program is `arc` on `PATH`, unless the client program names another.
- If the client program names an arc home, the command MUST start with
  `--home <home>`, before the arc command.
- A client program MUST NOT read the key files of the arc home.
- A failure MUST hold the last 4 KiB that arc wrote on standard error, on
  one line: the lines joined with `; `, with no line that starts with
  `arc: warning: `. If arc wrote nothing else, it holds the error of the
  process.
- A warning is a line that starts with `arc: warning: `, such as the
  warning of two homes, which arc writes at each command. A client program
  that gives a writer for warnings MUST get each warning once in the
  program, on one line. arc escaped the line: the client escapes each
  control character that is left, and keeps each backslash as one, so no
  escape of arc is escaped twice (`terminal.EscapeLineData`). An older client program gets each warning
  inside the text of a failure, and none on a success.

### 3.2 The message line

`arc message inbox --json` prints one message line for each received
message, oldest first. `arc message watch --json` prints one message line
for each message as it arrives. `arc message held --json` prints one
message line for each message that arc holds from a stranger
([the strangers spec](../strangers/SPEC.md), section 3.4).

| Field | Value |
| --- | --- |
| `id` | The id of the rumor: 64 hex digits. It is the same on each machine. |
| `from` | The public key of the sender: 64 hex digits. |
| `name` | The petname of the sender, that arc makes from the key. |
| `at` | The `created_at` of the rumor, in RFC 3339, in UTC. It is the clock of the sender. |
| `text` | The content of the message. |
| `members` | The public keys of the members: the sender first, then the recipients in the order of the sender. |
| `reply_to` | The id of the parent of a reply: 64 hex digits, as the sender chose it. Absent for a plain message. |

### 3.3 The sent line

`arc message outbox --json` prints one sent line for each recipient of each
message that this citizen sent from this machine, oldest first. A group
message has one sent line for each recipient, with the same `id`.

| Field | Value |
| --- | --- |
| `id` | The id of the rumor. |
| `to` | The public key of the recipient. |
| `name` | The petname of the recipient. |
| `at` | The time when arc put the message in the outbox, in RFC 3339, in UTC. |
| `text` | The content of the message. |
| `state` | `pending`, `delivered` or `expired`, for this recipient. |
| `members` | The public keys of the members: this citizen first, then each recipient. |
| `reply_to` | The id of the parent of a reply. Absent for a plain message. |

The outbox also holds requests, and private events of the kinds of apps.
`arc message outbox --json` MUST NOT print them. It prints kind 14 only.

### 3.4 The queued line

`arc message send --json` prints one queued line, with the fields `id`,
`to`, `state` and `members`, and `reply_to` for a reply. For a group
message, `to` is the first recipient.

### 3.5 Changes to a line

- arc MUST NOT remove, rename or change the type of a field of a line,
  except in a breaking release.
- arc MAY add a field. A client program MUST ignore a field that it does
  not know.
- A line is at most 1 MiB. A message holds at most 24 KiB, and JSON writes
  a byte as at most six.

### 3.6 Sending

- Each recipient MUST be a public key: 64 lowercase hex digits. The package
  refuses a name before it runs arc.
- With more than one recipient, the package sends one group message. arc
  gets the keys separated by commas.
- The text MUST go on standard input, so a text that starts with `-` is
  not a flag. arc removes the newlines at the end of the text.
- arc refuses a text that a person cannot see, as `terminal.Blank` finds
  it: a text of only spaces, control characters, format characters
  (Unicode class Cf, such as U+200B and the tag characters), combining
  marks (classes Mn and Me), the Hangul fillers U+115F, U+1160, U+3164 and
  U+FFA0, and the blank Braille pattern U+2800:
  `a message holds at least one character that a person can see`.
- `SendReply` sends a reply: `arc message send --json --reply <id>`. The
  id MUST be 64 lowercase hex digits; the package refuses another id
  before it runs arc. arc refuses a parent that this machine does not
  hold, and recipients that are not the members of the parent, without
  this citizen ([the delivery spec](../delivery/SPEC.md), section 3.4,
  "Replies").

### 3.7 Watching

- A watch runs `arc message watch --json`. It can add `--since`.
- arc can print a message twice, for example after a restart. The package
  MUST give each id at most once in one watch.
- At the end of the context, the package MUST interrupt arc, wait at most
  5 s, then kill it. The end of the context is not a failure.
- If arc stops before the end of the context, the watch fails, with what
  arc wrote on standard error.

### 3.8 The test relay

- `arcprogramtest.Start` builds `github.com/gezibash/arc/cmd/arc` with
  `go build -tags arctest`, and runs `arc relay serve` on a free port of
  `127.0.0.1`, with no STUN server. With the tag, a first message to a
  stranger makes 8 bits of proof of work, not 20
  ([the strangers spec](../strangers/SPEC.md), section 3.5). A release
  build has no tag.
- `Citizen` makes a new arc home, a new key, and adds the relay.

### 3.9 Showing text on a terminal

The text of a message comes from its sender. A terminal acts on some
characters: it moves the cursor, clears the screen, changes the title,
reorders a line, or breaks a line without a newline. Other characters show
nothing, but an agent that reads the text gets them. So a sender could
forge a line, hide a part of the text from a person and give it to an
agent, or make `file.exe` look like `file.png`.

- arc, and each client program that shows the text of a sender on a
  terminal, MUST show it as `terminal.Escape` gives it:

  | Character | Shown as |
  | --- | --- |
  | A newline or a tab | itself |
  | Each other control character of U+0000 to U+001F, and U+007F | `\x` and two lowercase hex digits, such as `\x1b` for ESC |
  | A control character of U+0080 to U+009F | `\u` and four lowercase hex digits, such as `\u0085`, so it looks different from the byte 0x85 |
  | A byte that is not UTF-8 | `\x` and its two lowercase hex digits, such as `\xff` |
  | An embedding, override or isolate (U+202A to U+202E, U+2066 to U+2069) | `\u` and four lowercase hex digits, such as `\u202e` |
  | A line or paragraph separator (U+2028, U+2029) | `\u2028`, `\u2029` |
  | An invisible character: U+200B, U+FEFF, U+00AD, U+2060 to U+2064, U+206A to U+206F, the interlinear annotation characters U+FFF9 to U+FFFB, the Mongolian vowel separator U+180E, the Mongolian free variation selector U+180B, the combining grapheme joiner U+034F, and the Hangul fillers U+115F, U+1160, U+3164 and U+FFA0 | `\u` and four lowercase hex digits, such as `\u200b` |
  | A musical format character (U+1D173 to U+1D17A) | `\U` and eight lowercase hex digits, such as `\U0001d173` |
  | A tag character (U+E0000 to U+E007F), except in the flags of England, Scotland and Wales | `\U` and eight lowercase hex digits, such as `\U000e0068` |
  | A variation selector (U+FE00 to U+FE0F) that does not follow a visible character that is not a mark, or that follows another selector | `\u` and four lowercase hex digits, such as `\ufe0f` |
  | A variation selector of U+E0100 to U+E01EF that does not follow an ideograph (Unicode property Ideographic) | `\U` and eight lowercase hex digits, such as `\U000e0100` |
  | A backslash | two backslashes, so a typed `\x1b` shows as `\\x1b` |
  | Each other character, including the joiners U+200C and U+200D, and the marks U+200E, U+200F and U+061C | itself |

- One variation selector after a visible character stays, because emoji
  use it, and one of U+E0100 to U+E01EF after an ideograph, because names
  use it. A backslash counts as a visible character, whether it is written
  as one backslash or as two, so Escape and Strip keep the same selectors.
  Any other selector selects nothing, and a sender could use it to hide
  text.
- The marks U+200E, U+200F and U+061C stay, because Arabic and Hebrew
  text uses them, and they cannot reorder a line. The joiners U+200C and
  U+200D stay, because emoji and many scripts use them.
- The flags of England, Scotland and Wales are U+1F3F4, the tags of
  `gbeng`, `gbsct` or `gbwls`, then U+E007F. Only these whole sequences
  keep their tags.
- A JSON decoder already gives U+FFFD for a byte that is not UTF-8. So the
  text of a message holds no such byte when arc shows it: the byte is lost
  when arc decodes the message. `terminal.Escape` escapes such a byte for
  other text.
- Under a header line, each line of the text MUST start with at least two
  spaces, so no line of the text looks like a header.
- Text that must stay on one line, such as a title, an id, a reason or a
  NOTICE, MUST show as `terminal.EscapeLine` gives it: as the table says,
  and a newline as `\n` and a tab as `\t`.
- The JSON lines hold the text as the sender sent it. A program that reads
  them and shows the text on a terminal applies this section.
- arc applies this section to all other text that another party chooses:
  - the stored reply and the error of a provider, in `arc call results`;
  - the title, summary, id and manifest of an offer, and the help of an
    installed app: titles, ids, kind names and the group relay on one
    line;
  - the reasons why a relay refused mail or sealed data;
  - the text of a relay NOTICE: on one line, and cut to 300 characters of
    output, the ellipsis included, never inside an escape or a flag, by
    `terminal.EscapeLineCut`;
  - the lines of `arc relay ring`, which hold what a relay answered;
  - the message, the plan and the state that a storage citizen gives
    `arc keys storage add`;
  - each line that arc writes on standard error with text from another
    party, such as the reason of a relay, the reason why a relay closed a
    subscription, or the error of a member of a list: each such value on
    one line;
  - the line that arc prints for an error, `arc: ` and the error: on one
    line, so a refusal cannot forge a second line.
- The programs of this repository escape the text of another party where
  they print it, and not where they make an error. An error keeps the text
  as it came, such as the message of a storage refusal. The log of the
  operator of an app is a terminal too: an error in it shows on one line,
  as `terminal.EscapeLine` gives it.
- A live reply of `arc call` is escaped wherever it goes, because a
  program or an agent that reads it through a pipe has no terminal either:
  - on a terminal, as `terminal.Escape` gives it;
  - through a pipe or into a file, a reply that is valid JSON as
    `terminal.EscapeJSON` gives it: each character that the table escapes,
    inside a string, becomes a JSON escape, so the JSON keeps its value;
  - through a pipe or into a file, any other reply as `terminal.EscapeData`
    gives it: as the table says, but a backslash stays one backslash;
  - with `--raw`, as it came. On a terminal, arc then says on standard
    error that the reply can change the terminal.
- A text record of `arc session` over HTTP comes from the provider: on a
  terminal as `terminal.Escape` gives it, and through a pipe as
  `terminal.EscapeData` gives it.
- A value that an app shows ([the interface spec](../interface/SPEC.md),
  section 3.15), the text of its format templates, and a reply that does not fit the output of its app, are
  data that a program can read through a pipe. arc removes from them each
  character that the table escapes, except a backslash, as `terminal.Strip`
  does, and keeps the rest as it was. The first time that it removes a
  character in a command, it says so on standard error:
  `arc: the output held characters that could change the terminal or hide
  text; arc removed them`.

### 3.10 Showing a reply

A sender chooses the `reply_to` of its message, and can name any id: a
message that does not exist, a message that the reader never got, or a
message of another conversation. The text and the author of the parent
are not on the wire.

- A client program MUST take the text and the author of a parent only
  from a message of its own store: the received and the sent messages of
  this machine. It MUST NOT take a quote from the text of the reply.
- It MUST show the text of the parent only when the parent has the same
  members as the reply. Otherwise it shows the id of the parent only.
- A parent that the store does not hold shows its id only.
- `arc-dm` shows the parent on one quote line, between the header and the
  text of the reply: `↳`, the short id, the author, and the first line of
  the parent that is not empty, as `terminal.EscapeLineFit` gives it, with
  the line at most 72 columns of a terminal, as `terminal.Width` counts
  them, and at most 3 characters for each column. `terminal.Width` counts
  one cluster at a time: a character with its marks and selectors, an
  emoji with one skin tone, pictographs joined by U+200D, or a flag. A cluster takes two
  columns when it is a flag or a lone regional indicator, when it holds
  U+FE0F, or when its first character is wide or fullwidth, such as a CJK
  ideograph or most emoji. A cluster that starts with a mark or a format
  character takes none, each other one, and each spacing mark adds one.
  Width is meant to count no fewer columns than a terminal shows;
  terminals differ, so it is checked for the cases of `TestWidth`, not
  proven for all text. The cut keeps each cluster as Width finds it, and
  the limit of characters cuts a pile of marks that takes no column.
  Known limits: a sequence joined by U+200D with a symbol outside the
  emoji blocks, such as U+2194, and Hangul jamo can be cut apart; a base
  with up to 215 marks fits the limit of characters. A parent of only spaces shows `(blank)`. The short id of a quote line is at most
  12 digits, then `…`: a sender can name an id that matches a held id in
  63 digits. The quote line starts at column 0, where
  no line of a text starts (section 3.9), so a quote typed into a text
  never looks like a quote. Its other forms are
  `↳ <id>  (not on this device)` and `↳ <id>  (in another conversation)`.
- `arc message inbox` and `arc message watch` add `reply to` and the first
  6 digits of the parent to the header of a reply. They show no quote.
- A build of arc with the tag `arctest` has the flag
  `arc message send --arctest-tag`, which adds a tag and skips the checks
  of a reply, so the tests of a client program can send a hostile reply. A
  release build refuses the flag.

## 4. Behavior

### 4.1 Reading the mail of this machine

1. The client program calls `Inbox` or `Outbox`.
2. The package runs `arc message inbox --json` or
   `arc message outbox --json`, and reads one line at a time.
3. arc reads the store of this machine. It does not go to a relay. `arc
   sync`, `arc serve` and a watch fill the store.

### 4.2 Sending a message

1. The client program calls `Send` with a list of public keys and a text.
2. The package runs `arc message send --json <key>,<key>`, with the text
   on standard input.
3. arc seals the message, puts it in the outbox, and sends it to the
   relays at once. A relay that fails does not fail the send.
4. The package returns the queued line.

### 4.3 Watching for messages

1. The client program calls `Watch` with a function.
2. The package runs `arc message watch --json`. arc writes `watching` on
   standard error for each relay that it watches.
3. arc keeps each message that it opens in the store of this machine, and
   prints its message line.
4. The package gives each new id to the function. If the function fails,
   the package interrupts arc and returns the failure.

## 5. Failures

| Cause | What happens | What the user sees |
| --- | --- | --- |
| arc is not on `PATH` | The process does not start. The error wraps `arcprogram.ErrNoArc`. | `the arc program is not installed: no "arc" on PATH`, and where to get arc. |
| The arc home has no key | arc fails. | What arc wrote, such as how to make a key. |
| The citizen has no relay, at a watch | arc fails. | `arc message watch needs a relay: run arc relay add <url>`. |
| A bad `--since` | arc fails. | What arc wrote. |
| An arc older than this spec, at `Outbox` | arc refuses the flag `--json`. | `unknown flag: --json`. |
| A line that is not JSON | The package stops. | A failure that names the command. |
| A recipient that is not a key | The package refuses it before it runs arc. | `a recipient is a public key of 64 hex digits`. |
| A text that a person cannot see, such as only spaces or U+200B | arc refuses it, and sends nothing. | `a message holds at least one character that a person can see`. |
| A reply to a message that this machine does not hold | arc refuses it, and sends nothing. | `mail: no message <id> on this device`. |
| A reply to other members than the members of the parent | arc refuses it, and sends nothing. | `mail: the reply goes to the members of its parent`. |
| A reply, with an arc older than replies | arc refuses the flag. | `unknown flag: --reply`. |

## 6. Security

- The client program runs arc with the rights of the person. arc does what
  its commands do, with the consent rules of arc. The client program gets
  no key.
- The text of a message comes from its sender. A client program that shows
  it on a terminal MUST escape it as section 3.9 says, so a sender cannot
  move the cursor, change the terminal, reorder a line, or forge a line.
- The `reply_to` of a line comes from the sender, and can name any id. A
  client program that shows the parent MUST follow section 3.10, so a
  sender cannot put words in the mouth of another member, or move the
  text of one conversation into another.
- The petname comes from the key, not from the sender. Two keys can have
  the same petname. A client program that selects a peer by name MUST
  refuse a name that selects more than one peer.
- `arcprogram.Petname` gives the petname of a key that no line named, as
  arc makes it. The eight hex digits at the end of a petname come from a
  hash of the key, not from the start of the key.
- Each call runs one arc process. A watch holds one arc process and one
  connection to each relay of the citizen.

## 7. Compatibility

- `arc message inbox --json` and `arc message watch --json` existed before
  this spec, with the same fields.
- `arc message outbox --json` is new. A client program that reads the
  outbox needs an arc of this version or newer.
- An arc or a client program older than the musical format characters,
  U+034F and U+180B in the table of section 3.9 shows them as they came,
  and Strip keeps them. An arc-dm older than the column count can make a
  quote line of wide text wider than 72 columns.
- `reply_to` is a new field of each line. An older arc prints none, so a
  client program shows no quote. An older client program ignores it, as
  section 3.5 requires, and shows a reply as a plain message. `SendReply`
  needs an arc with `--reply`. `Send` does not change.
- `members` is a new field of each line. An older arc prints none, so the
  package gives an empty list. A client program then takes the sender and
  this citizen, or this citizen and the recipient, as the members.
- An older arc refuses a send to several keys: `a recipient is 64
  characters of hex`.
- `Send` took one key. It takes a list now, a breaking change for a client
  program in another repository.
- The code that runs arc was `client.Arc` in the transfer app. The transfer
  app now uses this package.
- An arc older than section 3.9 prints the text of a message as the sender
  sent it, with only its first line indented, and sends a text of only
  spaces. It also prints provider replies, offers and relay notices as they
  arrive. An `arc-dm` older than section 3.9 shows each control character
  as U+FFFD, and shows embeddings, overrides and isolates as they are.
  Update arc and each client program; the JSON lines do not change.

## 8. Proof

| Test | What it proves |
| --- | --- |
| `TestMessages` | Two messages cross a relay. The inbox of the recipient and the outbox of the sender hold both, with the keys, names, times and states. A text that starts with `-` arrives whole. A citizen that sent nothing has an empty outbox. |
| `TestGroupMessages` | A send to two keys gives a queued line, message lines and sent lines that name the three members, and one sent line for each recipient. |
| `TestSendRefusesANameAsTheRecipient` | `Send` refuses a recipient that is not a key. |
| `TestWatch` | A watch gives a message that arrives while it runs, and returns with no error at the end of the context. |
| `TestWatchFailsWithABadSince` | A watch returns what arc says about a bad `--since`. |
| `TestEscape` | `terminal.Escape` escapes each control character, embedding, override, isolate and backslash, and keeps a newline, a tab, the marks U+200E and U+200F, and other text. |
| `TestEscapeLine` | `terminal.EscapeLine` writes a newline as `\n` and a tab as `\t`. |
| `TestEscapeLineCut` | The cut counts characters of output, and never splits an escape or a flag. |
| `TestEscapeData` | `terminal.EscapeData` escapes as the table says, and keeps one backslash. |
| `TestEscapeJSON` | `terminal.EscapeJSON` writes the hidden characters inside JSON strings as JSON escapes; the JSON keeps its value. |
| `TestStrip` | `terminal.Strip` removes what the table escapes, and keeps a backslash, a mark, a joiner and a flag. |
| `TestStripCases` | The cases of a probe by Alfred: CRLF, BOM, bad UTF-8, NUL, selectors, annotation characters, an override, and the marks. |
| `TestStripKeepsOneSelector` | Strip keeps one selector after a visible character, and `Stripped` counts what it removes. |
| `TestStripAndEscapeAgreeAfterABackslash` | After a backslash, Strip and Escape keep the same selector. |
| `TestEscapeLineCutNeverPassesTheLimit` | For each limit from 1 to 40, a cut is at most the limit, the ellipsis included. |
| `TestBlank` | `terminal.Blank` finds a text of only spaces, control and format characters, and no other. |
| `TestATerminalShowsNoEscapeOfASender` | A message with ESC, U+202E, a typed `\x1b` and a forged header crosses a relay. The inbox and the outbox of arc show it escaped, with each line indented. The JSON line holds the text as sent. arc refuses a text of only spaces, of U+200B and U+2060, or of tags. |
| `TestHeldAndWatchedTextIsEscaped` | A held message of a stranger, and a message that `arc message watch` prints, show ESC, U+2028 and tags escaped. |
| `TestServeTakesMailFromAWatch` | The text and the reply of a store-and-forward call show escaped in `arc call results`. |
| `TestALiveReplyIsEscapedWhereverItGoes` | A live reply shows escaped through a pipe, on a terminal, through `| cat` and `| tee`, and in a file. `--raw` writes it as it came, and warns on a terminal. |
| `TestAnErrorLineCannotReachTheTerminal` | The error line stays one line and shows ESC, an override, a tag, selectors and a typed escape as escapes. |
| `TestARelayRefusalIsEscapedOnOneLine` | A relay that refuses the wrap and closes each subscription with a hostile reason cannot change the terminal or forge a line through `arc message send`. |
| `TestARingRefusalIsEscapedOnOneLine` | A relay that answers `arc relay ring show` and `clear` with a hostile refusal cannot change the terminal or forge a line. |
| `TestAStorageAnswerIsEscaped` | The message, plan and state of a storage citizen show escaped, each on its line. |
| `TestNoticeIsCutAndEscaped` | arc shows a relay NOTICE on one line, escaped, and cut to 300 characters of output. |
| `TestTextOfOtherPartiesIsEscapedAtThePrint` | A check of the source of arc, the SDK and the apps: each print or write of an error, or of a field that another party fills, such as `Message`, `ID` or `State`, goes through `sdk/terminal` or a function that returns the result of an escape. A print of text that arc writes itself carries `//arc:raw-ok` and a reason. Each slog logger uses `slog.TextHandler`, inline or held in a variable; no code logs with the default slog logger or imports `log` or `log/slog` under another name. |
| `TestTheEscapeCheckFindsRawPrints` | The check finds each print of a package of hostile prints, and no escaped print. |
| `TestReplies` | `SendReply` sends a reply across a relay. The queued line, the message line and the sent line name the parent; a plain message names none. A parent that is not an id is refused before arc runs. |
| `TestAReleaseBuildHasNoTestTags` | A release build of arc refuses `--arctest-tag`. |
| `TestAReplyQuotesItsParent` | `arc-dm reply` with 4 digits of a short id, and `arc-dm send -reply`, send a reply; the confirmation names the conversation. The other side shows the quote line at column 0 from its own sent text. `arc message inbox` names the parent. |
| `TestAReplyInAGroup` | A reply to a group message goes to each member of the group, and not to a direct conversation. The confirmation names each member. |
| `TestAQuoteComesFromTheStore` | A quote line typed into a text shows inside the text, at column 2. The one line at column 0 is the parent from the store. |
| `TestAHostileParentIsOneEscapedLine` | A parent with ESC, U+202E, U+200B and a first line of 5,000 characters gives one escaped quote line of at most 72 columns. 100 CJK ideographs fit in 72 columns. A parent that starts with empty lines quotes its first line with text. An id that matches a held id in 63 digits shows 12 digits and `…`, inside 72 characters. |
| `TestAReplyToAnUnheldMessageShowsTheIdOnly` | A reply to an id that the reader does not hold shows `(not on this device)`. |
| `TestAReplyAcrossConversationsShowsNoText` | A direct reply that names a group message shows `(in another conversation)` and no text of the group. arc refuses to send such a reply. |
| `TestATagThatIsNoReplyShowsAPlainMessage` | Two `e` tags, or an id in capitals, give a plain message. A relay hint after the id still gives a reply. |
| `TestReplyRefusals` | arc-dm refuses an id that is not one, an id of no message, and `send -reply` into another conversation. arc refuses an unheld parent and an id in capitals. Nothing is sent. |
| `TestAWatchQuotesTheParent` | `arc-dm watch` shows the quote of a reply to a message sent after the watch started, and the id only for an unheld parent. |
| `TestAReplyToYourOwnMessage` | A reply to a message that the citizen sent quotes it on both sides: arc finds the parent in the outbox. |
| `TestSendReplyChecksTheIdBeforeTheText` | `arc-dm send -reply` with a wrong id fails at once, with standard input open. |
| `TestAWatchReadsTheInboxOnlyAtItsStart` | Through the CLI, 20 replies to ids that no message has make a watch read the outbox after its start, and the inbox not at all. |
| `TestTheOutboxIsReadAtMostOnceEachRefreshFromTheEndOfTheLast` | With an outbox that takes 2 s and replies to unknown ids each 100 ms for 14 s, the watch reads at once, then at most once each 7 s; each read starts at least 5 s after the last one ended, and no take waits 1 s. |
| `TestTheHeldQueueIsCapped` | A watch holds at most 4,096 messages, and loses none; each sender keeps its order. |
| `TestOneSendersJunkDoesNotHideAValidReply` | One junk reply, one valid reply, then 254 junk replies of one sender: the junk past 64 shows at once, oldest first, and the valid reply gets its quote. |
| `TestRotatingKeysDoNotHideAValidReply` | 16 keys that take turns to send 255 junk replies within 1 s of a read hide no valid reply. |
| `TestQuickHonestRepliesKeepQuotesAndOrder` | 20 quick replies of one sender all get their quotes, in order. |
| `TestASenderOverItsQuotaPrintsItsOldest` | A sender with 100 held messages shows its 36 oldest at once, in order, and holds 64. |
| `TestSmallHoldersWaitForTheRead` | Past 256 waiting replies with the read blocked, none shows early: the read comes within a second, and each reply gets its quote. |
| `TestManyKeysDoNotHideAnHonestSender` | 300 keys with one junk reply each, within 1 s of a read, hide none of the 30 waiting replies of an honest sender. |
| `TestAnEarlyReplyMarksNoParentMissed` | A reply that shows early because its sender is over the quota marks no parent missed. |
| `TestASealedKeyAsksOnTheTerminal` | A key that a passphrase seals asks on the terminal of arc-dm, without echo, and arc-dm works. |
| `TestAHangupEndsEachArc` | A hangup, to arc-dm alone or to its process group, ends arc-dm and each arc that it runs. |
| `TestSIGTERMEndsEachArc` | SIGTERM to arc-dm alone ends each arc that it runs. |
| `TestAKilledArcDmLeavesNoArcOnLinux` | On Linux, a killed arc-dm leaves no arc behind. |
| `TestAShownMissingIdIsRefused` | A short id that arc-dm showed as missing does not name a later message that starts with it, unless that id is here now. |
| `TestOnlyPlainHeadsShowEarly` | At the hard cap with the read blocked, no message of a sender whose oldest held reply waits shows early. |
| `TestAFailedReadMarksNothingMissed` | After a read that fails, a waiting reply shows with no quote line, no id is shown as missing, and a later reply to the same parent gets its quote. |
| `TestAWatchReadTimesOut` | A read on a hung arc ends after 10 s with a warning, and shows the held reply with no quote line; a reply after it shows at once with no quote line; after a read that works, a later reply gets its quote. |
| `TestAfterAFailedReadNothingShowsAsMissing` | After a failed read, no reply shows as "(not on this device)", not even one that a sender over its quota pushes out, and no id is recorded. |
| `TestAnAlwaysHungOutboxShowsRepliesPlain` | With an outbox that always hangs, a reply to a message of this citizen shows after one read, with no quote line. |
| `TestRepliesShowSoonWhileTheOutboxHangs` | With an outbox that always hangs, each of 12 replies, 1 s apart, shows within 40 s with no quote line, and none shows as missing. A watch that reads for each reply passes 40 s by its seventh reply. |
| `TestTakeNeverReads` | A message that arrives does not wait for a read of the outbox that blocks. |
| `TestReadsRecoverAfterAFailure` | After a failed read, a reply shows at once with no quote line; after a read that works, replies wait for reads again, and a sender over its quota shows its oldest as "(not on this device)". |
| `TestReadsBackOffFromTheEndOfTheLastRead` | With an outbox that takes 2 s and fails, the next read starts at least 5 s after the last one ended. |
| `TestACompactionDuringAReadKeepsALateReplyWaiting` | A flood of 0, 60, 130 or 200 messages of one sender during a read does not put a reply that came during the read into what that read prints: the reply to a message sent after the read started is not marked missing, and gets its quote from the next read. With 100 messages before the read, 36 of them printed early, and 100 during it, the read does not slice past the end of the queue. |
| `TestASaveRemovesTheTemporaryFileOfAKilledSave` | A save of the shown ids removes the temporary file that a killed save left, and neither the lock file nor a file of another path. |
| `TestAKilledWatchKeepsTheIdsThatAReadShowed` | After a timer read shows an id as missing, with no message after it, a watch killed with SIGKILL has saved the id, and `arc-dm reply` refuses it. |
| `TestAWatchSavesShownIdsAtItsEnd` | The ids that a watch showed as missing are saved at its end, and `arc-dm reply` refuses them. |
| `TestACallEndsWhenArcEnds` | A call returns when arc ends, though a child of arc holds its output open. |
| `TestACallOutlivesTheEndOfOtherThreads` | On Linux, with one P, a call's arc lives on while other goroutines end their threads between the start of arc and the wait for it. |
| `TestAPanicInAWatchEndsItsArc` | A function of a watch that panics, or that calls `runtime.Goexit`, with a caller that recovers, leaves no arc running. |
| `TestAWatchOutlivesTheEndOfOtherThreads` | On Linux, a watch's arc lives on while other threads of the program end: the thread that started it stays held. |
| `TestReplyRefusesAnIdShownAsMissing` | Through the CLI, arc-dm reply refuses the id of a parent that arc-dm open showed as missing. |
| `TestABusyGroupDoesNotHideAValidReply` | 600 messages behind a valid reply that waits do not push it out unquoted, and each sender keeps its order. |
| `TestAnOverflowReadsAnOldOutbox` | An overflow reads the outbox 1 s after the end of the last read. |
| `TestCraftedCollisionsStayFast` | Ids that share 10 digits get 11-digit short ids, and a flood of such pairs stays linear. |
| `TestASecondInterruptEndsTheWatch` | A second interrupt ends a watch at once while its last read of the outbox hangs: the watch shows its held reply, and the hung arc ends. |
| `TestTheEndOfAWatchReadsTheOutbox` | At its end, a watch reads the outbox once for a reply that waits, and prints it with its quote. |
| `TestAWatchShowsAHeldReplyAtItsEnd` | An interrupt while a reply waits for the outbox still prints the reply with the quote of its parent. |
| `TestALiveFloodIsLinear` | 4 times the messages added one at a time, each with its short id, take less than 9 times as long. |
| `TestTheWatchForgetsMissedIds` | A watch remembers at most 1,024 ids that a read of the outbox did not find. |
| `TestALiveAddDoesNotSortTheStore` | At 100,000 messages, a watch adds one message in under 0.6 percent of the time of a whole index: no sort. |
| `TestAWatchHoldsAReplyUntilItReadsTheOutbox` | After a reply to an id that no message has, a reply within 5 s to a message sent from another process after the watch started still shows its parent, after the first reply. |
| `TestAddIsNotQuadratic` | 4 times the messages added to a watch take less than 9 times as long. |
| `TestAnIdIsCheckedBeforeArcRuns` | `arc-dm reply zz`, `send -reply zz` and `send -reply=` fail on the id with no arc program. |
| `TestWidth` | `terminal.Width` counts two columns for a CJK ideograph, a fullwidth letter, an emoji, a heart with U+FE0F, a family joined by U+200D, a skin tone, a flag and a lone regional indicator; two for Devanagari ka with a spacing vowel sign; none for marks; one for a heart alone; two for letters around a joiner. |
| `TestEscapeLineFit` | 100 CJK ideographs, or 100 hearts with U+FE0F, fit in 72 columns as 35 and an ellipsis. A letter with 300 marks is left out. A cut keeps flags and letters with their marks whole, and never cuts an escape. |
| `TestFlagsAfterThePeer` | `arc-dm send <peer> -reply <id> text` sends a reply, not the text `-reply <id> text`. After the peer, each word but `-reply` is text: `-5 degrees`, `-h`, `-home is where the heart is`, `-arc X everything` and `-state of the art`. After `--` each word is text, and an empty `-reply=` is refused. |
| `TestArcChecksAReplyBeforeTheText` | `arc message send --reply` with a bad or unheld id fails at once, with standard input open. |
| `TestIndexIsNotQuadratic` | 4 times the messages take less than 9 times as long to index: one sort. |
| `TestABlankParentQuotesAsBlank` | A parent of only spaces, such as U+3000, quotes as `(blank)`. |
| `TestShortIDs` | A short id grows past 6 digits where two ids share them, and a prefix of two messages is refused. |
| `TestSlogTextHandlerQuotesHostileText` | `slog.TextHandler` writes ESC, an override, an invisible character, a tag, a newline and U+2028 of a message, a value and an error as escapes. |

The tests of the dm app, `go test ./apps/dm/...`, use this package and the
test relay. No test runs a client program against a relay on another
machine.
