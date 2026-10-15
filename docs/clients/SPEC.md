# Client programs

- Status: built
- Layers: sdk
- Owns: sdk/arcprogram, sdk/arcprogram/arcprogramtest
- Proof: go test -count=1 -run '^(TestMessages|TestGroupMessages|TestSendRefusesANameAsTheRecipient|TestWatch|TestWatchFailsWithABadSince)$' ./sdk/arcprogram/
- Unverified: No test runs a client program against a relay other than a relay on the same machine. No test reads the outbox of a citizen that sent a request or a private event of an app kind. No test runs a client program on Windows, where arc cannot get an interrupt.

The package `sdk/arcprogram` runs the arc program for a client program. It
reads the JSON lines that `arc message` prints. The package
`sdk/arcprogram/arcprogramtest` runs a relay on this machine for the tests
of a client program.

## 1. Purpose

A client program is a program of an app that a person runs, such as
`arc-transfer` or `arc-dm`. It holds no key. It asks arc to sign, seal,
send and read, so arc keeps the key of the citizen, and its consent rules.

This spec defines:

- how a client program runs arc, in an arc home,
- the JSON lines of `arc message`: the received messages, the sent
  messages, and the queued message,
- how a client program sends a message and watches for messages,
- the relay on this machine that the tests of a client program use.

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
- A failure MUST hold the last 4 KiB that arc wrote on standard error. If
  arc wrote nothing, it holds the error of the process.

### 3.2 The message line

`arc message inbox --json` prints one message line for each received
message, oldest first. `arc message watch --json` prints one message line
for each message as it arrives.

| Field | Value |
| --- | --- |
| `id` | The id of the rumor: 64 hex digits. It is the same on each machine. |
| `from` | The public key of the sender: 64 hex digits. |
| `name` | The petname of the sender, that arc makes from the key. |
| `at` | The `created_at` of the rumor, in RFC 3339, in UTC. It is the clock of the sender. |
| `text` | The content of the message. |
| `members` | The public keys of the members: the sender first, then the recipients in the order of the sender. |

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

The outbox also holds requests, and private events of the kinds of apps.
`arc message outbox --json` MUST NOT print them. It prints kind 14 only.

### 3.4 The queued line

`arc message send --json` prints one queued line, with the fields `id`,
`to`, `state` and `members`. For a group message, `to` is the first
recipient.

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
  `go build`, and runs `arc relay serve` on a free port of `127.0.0.1`,
  with no STUN server.
- `Citizen` makes a new arc home, a new key, and adds the relay.

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
| arc is not on `PATH` | The process does not start. | The error of the process. |
| The arc home has no key | arc fails. | What arc wrote, such as how to make a key. |
| The citizen has no relay, at a watch | arc fails. | `arc message watch needs a relay: run arc relay add <url>`. |
| A bad `--since` | arc fails. | What arc wrote. |
| An arc older than this spec, at `Outbox` | arc refuses the flag `--json`. | `unknown flag: --json`. |
| A line that is not JSON | The package stops. | A failure that names the command. |
| A recipient that is not a key | The package refuses it before it runs arc. | `a recipient is a public key of 64 hex digits`. |

## 6. Security

- The client program runs arc with the rights of the person. arc does what
  its commands do, with the consent rules of arc. The client program gets
  no key.
- The text of a message comes from its sender. A client program that shows
  it on a terminal MUST remove control characters, so a sender cannot move
  the cursor or change the terminal.
- The petname comes from the key, not from the sender. Two keys can have
  the same petname. A client program that selects a peer by name MUST
  refuse a name that selects more than one peer.
- Each call runs one arc process. A watch holds one arc process and one
  connection to each relay of the citizen.

## 7. Compatibility

- `arc message inbox --json` and `arc message watch --json` existed before
  this spec, with the same fields.
- `arc message outbox --json` is new. A client program that reads the
  outbox needs an arc of this version or newer.
- `members` is a new field of each line. An older arc prints none, so the
  package gives an empty list. A client program then takes the sender and
  this citizen, or this citizen and the recipient, as the members.
- An older arc refuses a send to several keys: `a recipient is 64
  characters of hex`.
- `Send` took one key. It takes a list now, a breaking change for a client
  program in another repository.
- The code that runs arc was `client.Arc` in the transfer app. The transfer
  app now uses this package.

## 8. Proof

| Test | What it proves |
| --- | --- |
| `TestMessages` | Two messages cross a relay. The inbox of the recipient and the outbox of the sender hold both, with the keys, names, times and states. A text that starts with `-` arrives whole. A citizen that sent nothing has an empty outbox. |
| `TestGroupMessages` | A send to two keys gives a queued line, message lines and sent lines that name the three members, and one sent line for each recipient. |
| `TestSendRefusesANameAsTheRecipient` | `Send` refuses a recipient that is not a key. |
| `TestWatch` | A watch gives a message that arrives while it runs, and returns with no error at the end of the context. |
| `TestWatchFailsWithABadSince` | A watch returns what arc says about a bad `--since`. |

The tests of the dm app, `go test ./apps/dm/...`, use this package and the
test relay. No test runs a client program against a relay on another
machine.
