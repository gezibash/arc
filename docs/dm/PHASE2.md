# DM Phase 2

Phase 1 (`docs/dm/SPEC.md`) proved the shape: sealed bodies, a provider
that stores ciphertext, a persistent inbox, receipts, and a block list.
Phase 2 makes DM something a person opens every day. Every feature below
keeps the Phase 1 invariant: the provider never sees plaintext.

The document has three parts. Part A is the experience: what a citizen
sees and types. Part B is the features that produce it, each with its
storage, wire, and CLI changes. Part C is the core work the features need,
numbered C8 onward to follow `docs/dm/CORE.md`.

## Part A. The experience

### A1. `arc dm` is the home screen

```
$ arc dm
3 unread in 2 conversations

  jolly-volta      2 unread   12s ago   ↳ Agreed. Ship the ACL as the sharing…
  aqua-bohr        1 unread   4h ago    Can you look at the relay cap before…
  neat-earnshaw    —          2d ago    Per-notebook ACL is the same feature…

arc dm open jolly-volta        read the conversation
arc dm send jolly-volta        reply
```

Names, not hex. A preview line, opened on this machine. Unread counts.
Relative times. The most active conversation first.

### A2. A conversation reads like a conversation

```
$ arc dm open jolly-volta
── jolly-volta ── 6 messages, 2 unread ──────────────────────────────

  neat-earnshaw  2026-09-14 19:22
  Read your business page. Per-notebook ACL and per-page sharing are the
  same feature. Build it once — agreed.
                                                          ✓ read 19:22

  jolly-volta  19:23  ↳ reply
  Agreed. Ship the ACL as the sharing primitive and I will sell it as
  "share a notebook".
                                                    👍 neat-earnshaw

  jolly-volta  19:31  📎 acl-draft.md (4.1 KiB)
  Draft attached. Tear it apart.
```

Opening a conversation marks it read. Replies show what they reply to.
Reactions and attachments sit inline. The `--raw` flag prints the Phase 1
tab-separated lines instead.

### A3. Sending is one line

```
$ arc dm send jolly-volta "Looks right. One nit on section 3."
$ arc dm send jolly-volta --file acl-review.md
$ arc dm send jolly-volta --reply-to 01M2GN… "Yes."
$ arc dm send devs "Standup moved to 10."           # a multi-recipient message
$ echo "long body" | arc dm send jolly-volta         # stdin still works
```

Short bodies go on the command line. Long bodies come from a file or from
stdin. A recipient can be a name, a key prefix, hex, or a saved list.

### A4. Live

```
$ arc dm watch
watching as neat-earnshaw. Ctrl+C to stop.
19:40:02  jolly-volta   Draft attached. Tear it apart.            📎 1
19:40:41  aqua-bohr     Can you look at the relay cap before…
```

`watch` prints each message as it lands, opened, with names. It is also
the hook for a desktop notification, see F9.

### A5. React, retract, mute

```
$ arc dm react 01M2GN… 👍
$ arc dm retract 01M2GN…            # unsend within 10 minutes
$ arc dm mute aqua-bohr             # stays readable, stops counting as unread
```

### A6. Take it somewhere

```
$ arc dm export jolly-volta > jolly.md          # markdown transcript, opened locally
$ arc dm journal 01M2GN… arc/decisions/2026-09-14-acl-as-sharing
$ arc dm search "relay cap"                      # searches opened messages on this machine
```

`journal` writes one message into the ARC journal as a page, with the
sender and time in the frontmatter. `export` and `search` never touch
the provider beyond a `thread` pull.

## Part B. Features

Each feature lists what changes in the provider, on the wire, and in the
CLI, and which core item it depends on. Storage rules from Phase 1 hold:
one file per message, append-only receipts, no plaintext.

### F1. Conversations view

The home screen (A1).

- **Provider.** New command `conversations [--limit n]`. For every peer
  with at least one message in the caller's mailbox, return one line:
  `<peer>\t<last id>\t<last t>\t<unread>\t<muted>\t<last body token>`.
  Sorted by last id descending. Unread counts inbound messages with no
  `read` receipt and not muted.
- **CLI.** `arc dm` with no arguments runs `conversations` and renders
  A1. Needs `petnames` and `preview` output filters (C9) and a root
  command in the manifest (path `[]`, already supported by
  `InterfaceManifest`).

### F2. Conversation view and mark-as-read

`open <peer>` (A2).

- **Provider.** `thread` gains `--bodies`: each line carries the sealed
  body token. `thread --bodies` also writes `read` receipts for every
  inbound message it returns, so opening a conversation marks it read.
  The Phase 1 `thread` without `--bodies` is unchanged.
- **Provider.** `read` and `thread --bodies` include reactions and
  attachment names on the line, see F5 and F6.
- **CLI.** `open` is a manifest command over `thread --bodies` with
  output filters `open`, `petnames`, and a new `conversation` renderer
  (C9). `--raw` skips the renderer.

### F3. Send from the command line, a file, or stdin

(A3.)

- **CLI.** `send` takes an optional trailing positional `text`. If
  present, it is the body. `--file <path>` reads the body from a file.
  Otherwise stdin. All three go through `seal_to`. This needs `seal_to`
  to accept a body from an argument, not only stdin (C10).
- **Provider.** Unchanged.

### F4. Multi-recipient messages

`send devs "..."` where `devs` is a saved list, or `send a,b,c "..."`.

- **CLI.** `arc dm list add devs jolly-volta aqua-bohr` stores a
  recipient list in `~/.arc/dm/lists/devs` on this machine. `send`
  expands a list or a comma-separated set to N recipients and seals the
  body N+1 times: one token per recipient and one for the sender. This
  needs `seal_to` to accept a variable number of targets from one
  argument (C10).
- **Wire.** The `send` header gains `--to <hex,hex,...>`. The body is
  N+1 tokens, one per line, in the same order.
- **Provider.** One message id, one file per mailbox, `to` becomes a
  list. A group is its own conversation: `conversations` keys it on
  every other participant, sorted and comma-joined, and `thread` takes
  that key in any order. A group message never appears in a one-to-one
  thread with one of its members. A sender who lists themselves in
  `--to` still keys the conversation on the others only. A reply to a
  multi-recipient message goes to the same set by default.
- **Not a group.** There is no group identity, no membership, no
  history for a late joiner. That is Phase 3.

### F5. Reactions

`react <id> <emoji>` (A5).

- **Provider.** A reaction is a receipt: `{"t":..,"id":..,"event":"reaction","by":<hex>,"value":"👍"}`
  appended to the receipts file of every mailbox that holds the message.
  The provider validates `value` as one to four grapheme clusters.
  `read` and `thread --bodies` append reactions to the message line.
- **What leaks.** The reaction value is plaintext metadata. The provider
  sees who reacted with what. This is stated in the CLI help.

### F6. Attachments

`send --attach <path>` and `fetch <id> <name>` (A2, A3).

- **CLI.** The file is sealed to each recipient and to the sender with
  the `sealedbox` package and sent as base64 in the request body after
  the message tokens. Each attachment is one line per recipient:
  `attach:<name>:<sealed-v1 token>`. 16 MiB cap on the plaintext, the
  same budget as the journal.
- **Provider.** Attachments live in `mailboxes/<pk>/blobs/<id>/<name>`
  as sealed bytes. The message file lists `attachments: [{name, bytes}]`.
  `fetch <id> <name>` returns the sealed token; the CLI opens it and
  writes the file.
- **Storage budget.** Attachments count toward the mailbox byte budget
  (F10).

### F7. Retract

`retract <id>` within a window (A5).

- **Provider.** The sender may retract for `DM_RETRACT_WINDOW` seconds,
  default 600. The provider overwrites the body in every recipient copy
  with an empty string and appends a `retracted` receipt. The sender's
  own copy keeps its body. After the window, `retract` fails with
  `too_late`. A recipient who already read the message still sees
  "retracted" in place of the body, and `status` shows they read it
  before the retraction. A recipient who already purged their copy is
  skipped. Every step is idempotent, so a retry after a failure
  completes the retraction.
- **Event.** The provider emits `dm.retracted` to every other holder, so
  a `watch` that already printed the body learns to drop it.
- **Invariant.** This is the one place the provider rewrites a message
  file. Only `retract` rewrites, and only the body.

### F8. Mute

`mute <peer>` and `unmute <peer>` (A5).

- **Provider.** `muted` file next to `blocked`, one key per line. Muted
  peers still deliver. `conversations` marks them and counts no unread.
  `inbox --unread` skips them. `watch` (F9) skips them.

### F9. Watch and notifications

`watch` (A4).

- **Provider.** On every stored `send`, the provider emits an event to
  each recipient: topic `dm.new`, body = the recipient's sealed token,
  meta `{id, from, to, t}`. `to` is the conversation key from that
  recipient's point of view, so `arc dm open <to>` reaches the
  conversation for a group as well as a pair. On `react`, topic
  `dm.reaction`; on `retract`, topic `dm.retracted`; both carry
  `{id, from, to}`. This needs exec providers to emit events (C8).
- **CLI.** `watch` is a manifest command with `invoke.mode: "events"`
  and `topics: ["dm.*"]` (C11). It prints each event through `open` and
  `petnames`. `--notify` also posts a desktop notification with the
  sender name and the first line, through `osascript` on macOS and
  `notify-send` on Linux. The body never goes into the notification
  daemon unopened.
- **Delivery.** An event reaches a recipient only while that recipient
  runs `watch`. Nothing is queued. The inbox is the source of truth.

### F10. Mailbox byte budget

- **Provider.** `DM_MAILBOX_BUDGET`, default 512 MiB per mailbox,
  counts message files and blobs. A `send` that would push any recipient
  over the budget fails with `too_large mailbox <peer> full`. The
  recipient frees space with `purge --before <id>`, which deletes
  message files and blobs older than the id from the caller's own
  mailbox. This is the one delete a user can ask for; the other is the
  undo of a `send` that failed part way.
- **Counter.** Each mailbox keeps its byte count in a `usage` file that
  every write, retract, purge, and undo adjusts, so a `send` checks the
  budget without a walk. A missing or malformed counter is rebuilt from
  a walk. Only bytes that actually left the disk are subtracted. One
  provider process owns a `DM_ROOT`.

### F11. Receipt privacy

- **Provider.** `settings receipts on|off`, stored in
  `mailboxes/<pk>/settings.json`. With receipts off, `read` and
  `thread --bodies` still record `read` receipts for the owner's own
  unread count, but `status` shows only `delivered` to senders.

### F12. Export, journal, search

(A6.) All three run on the caller's machine.

- **CLI.** `export <peer>` pulls `thread --bodies`, opens every token,
  and prints a markdown transcript. `journal <id> <journal address>`
  opens one message and calls `arc journal write` with the sender, time,
  and id in the frontmatter. `search <query>` greps the local plaintext
  cache (F13). None of these add provider commands.

### F13. Local plaintext cache

`open`, `watch`, and `export` decrypt on the caller's machine. Doing that
again on every `arc dm` is slow for large inboxes and makes `search`
impossible.

- **CLI.** An opt-in cache at `~/.arc/dm/cache/<my pk>/<id>.json`, holding
  the opened body and metadata. Every cache file is sealed to the owner's
  own key with the `sealedbox` package, so the cache on disk is
  ciphertext too. `arc dm cache on|off|clear`. Default off. With the
  cache on, `conversations` previews and `search` read from it, and
  `open` only fetches ids newer than the cache.

## Part C. Core changes

### C8. Exec providers can emit events

**Now.** An exec provider replies on stdout with `reply` or `error` lines
tied to a request id. A citizen can emit an event to any peer
(C5), but only from an in-VM handler. The exec handler
(`cmd/exec-provider/main.go`) has no stdout op for it.

**Required.** A new stdout line:

```json
{"op":"event","to":"<hex pubkey>","topic":"dm.new","meta":{},"body":"..."}
```

The exec handler turns it into `Handler.emit_event/3` and the agent sends
it. The line needs no request id. A malformed line is logged and dropped.
The journal and dm providers document the op in their READMEs.

**Tests.** `cmd/exec-provider/exec_test.go`: a fixture runtime prints an event line; the
observer sees it sent to `to`.

### C9. Output filters: chain, `petnames`, `preview`, `conversation`

**Now.** A command may set one output filter, `open` (C3).

**Required.**

- `output.filter` accepts a list: `["open", "petnames"]`, applied in
  order. A string still works.
- `petnames` replaces every 64-hex public key in the output with its
  petname. `identity.Name` is deterministic, so this needs no
  lookup. `--hex` on any command disables it.
- `preview:<n>` truncates the text after the last tab on each line to `n`
  characters, on one line, with `…`.
- `conversation` renders `thread --bodies` lines as A2. It lives in
  `toolbox.Conversation`. It reads the line format, so the format
  is now part of the provider's interface and must not change without a
  manifest version bump.

### C10. `seal_to` from arguments and variable targets

**Now.** `seal_to` seals the stdin body to a fixed list of argument names.

**Required.**

- `input.body` on a stdin command names an optional positional or option
  that supplies the body instead of stdin, and `input.file` names an
  option whose value is a path to read. Precedence: `body` argument,
  then `file`, then stdin.
- A `seal_to` entry may name an argument that holds a comma-separated
  set of peers. The CLI resolves each one and seals once per peer. The
  header template gets `{{peer|pubkey}}` extended so a comma-separated
  value renders as comma-separated hex.
- Saved lists: `~/.arc/dm/lists/<name>`, one peer per line, expanded
  before resolution. This is a CLI feature, not a provider one.

### C11. Event subscription commands

**Now.** `arc listen` prints every inbox message and event. There is no
way for a manifest command to say "run a listener for these topics".

**Required.** `invoke.mode: "events"` with `topics: [glob]`. The CLI
starts the agent, filters incoming events by topic, applies the
command's output filters to each, and prints. Ctrl+C stops it.
`--notify` posts a desktop notification per event. The provider that
serves the tool must be the event source; events from other peers are
ignored.

### C12. Manifest interface version 2

F4 changes the `send` body, F2 changes the `thread` line, and F5 and F6
add fields to it. Bump `interfaces.cli.version` to 2. The CLI refuses to
render a v2 manifest with a v1 renderer and tells the citizen to run
`arc tool update dm`.

## Part D. What Phase 2 does not do

- **Groups as identities.** A group with a keypair, membership, and
  history for late joiners. F4 is fan-out, not a group.
- **Forward secrecy for stored mail.** A stored message is sealed to the
  recipient's long-term X25519 key. Rotating that key means keeping old
  private keys in the key store to open old mail. Phase 3.
- **Multiple hosts serving one mailbox.**
- **Server-side search.** Impossible by design. F12 searches locally.
- **Presence and typing indicators.**
- **Editing a sent message.** Retract and resend.

## Part E. Order of work

Each step is one pull request with tests. `mise run check` passes on
every one.

1. C9 `petnames`, `preview`, chained filters. Immediately improves Phase 1
   output with no provider change.
2. C10 body from argument and file. `send peer "text"` and `--file`.
3. F1 conversations, F8 mute, F11 receipt privacy. Provider plus home
   screen. Manifest version stays 1.
4. F2 `thread --bodies` and the `conversation` renderer. `arc dm open`.
5. F5 reactions, F7 retract.
6. C12 manifest v2, then F4 multi-recipient and F6 attachments, which
   both change the `send` body.
7. C8 exec events, C11 event commands, F9 watch and notifications.
8. F10 mailbox budget and purge.
9. F13 local cache, then F12 export, journal, search.

## Part F. Open questions

- **Reactions in plaintext.** Sealing a reaction to every reader is
  possible but turns a 1 byte emoji into a 100 byte token per reader.
  Proposal: plaintext, stated in help. Revisit if anyone objects.
- **Retract window.** 600 seconds default. Long enough to catch a wrong
  recipient, short enough that a recipient can rely on what they read.
- **Multi-recipient reply default.** Reply to all, or reply to sender?
  Proposal: reply to all, with `--to` to narrow. Matches the mental model
  of a thread.
- **Cache location.** `~/.arc/dm/cache` on the client is fine on a
  laptop. On a shared host every user must have their own `~`. Proposal:
  document it, do not solve it.

## Part G. Status

All nine steps landed on 2026-09-14 on branch `feat/sealed-box`, in the
order of Part E, one commit each with tests. Every step was run live
through a local relay with three persona keys before its commit.

Deviations from the plan above:

- The `conversation` and `markdown` renderers live in `toolbox/filters.go`,
  not the CLI app, because the output filter pipeline runs in `arc_data`.
- Attachments are capped at 4 MiB of plaintext, not 16 MiB, so a message
  to several peers stays inside the 64 MiB request line.
- `journal` is not a command. `arc dm read <id> | arc journal write <addr>`
  does the job and needs no code.
- The cache and search are generic: `arc cache on|off|clear|status|search
  <tool>`, driven by a `cache` output filter any tool may declare.
- Events carry a `topic` and a small meta map. `arc dm watch` prints meta
  as `key=value`, so a reaction reads without a body.
- Interface v2 is enforced: an older `arc` refuses a newer manifest with
  advice to update.
