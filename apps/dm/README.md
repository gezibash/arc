# Direct messages app

Direct messages are private messages, as NIP-17 defines. The program
`arc-dm` is the messaging app of a person: it groups the messages by their
members, counts the unread messages, and posts a notification when a message comes.

`arc-dm` holds no key. It drives `arc message`, so arc signs, seals and
sends each message, and keeps the mail. A message that another program
sent with `arc message send` is in the same conversation. The JSON lines
that `arc-dm` reads are in [the client programs spec](../../docs/clients/SPEC.md).

The app has no manifest and no service.

## Commands

```sh
arc-dm                         # the conversations, newest first
arc-dm open <peer>             # one conversation; it marks its messages read
arc-dm send <peer> [text...]   # send a message; with no text, read standard input
arc-dm send <peer>,<peer> ...  # send one message to a group
arc-dm reply <id> [text...]    # reply to a message, in its conversation
arc-dm send -reply <id> <peer> [text...]  # the same, with the peer named
arc-dm read [peer]             # mark a conversation read, or all of them
arc-dm watch                   # print and notify each message as it arrives
```

A peer is a public key or a petname, the start of either, or the end of a
petname. The eight hex digits at the end of a petname come from a hash of
the key, not from the start of the key, so `tidal-picard-c614cd5b` is found
by `c614cd5b` and by the start of its key alike. A member of a group is a
peer too: `arc-dm open <member>` with no direct messages opens the one
group that you share, or names the groups when you share several. Flags
can come before or after the command. A name that selects more than one conversation is refused.
To write to a citizen for the first time, give its public key, as
`arc whoami` prints it. `arc-dm help` lists the commands.

```text
$ arc-dm
2026-10-15 14:02  chief-landau-268744ba  2 unread
  bring the map
2026-10-15 13:40  royal-pythagoras-1c77da22
  you: see you there
```

Each command takes these flags:

| Flag | Default | Meaning |
| --- | --- | --- |
| `-arc` | `arc` | The arc program. |
| `-home` | the home that arc picks | The arc home. |
| `-state` | `DM_STATE`, or `~/.local/state/arc-dm` | The directory of the read state. |

## Replies

`arc-dm open` and `arc-dm watch` show a short id after the time of each
message: six hex digits, or more when two messages of this machine start
with the same six. The short id is the start of the id of the message. (The
eight hex digits at the end of a petname are different: they come from a
hash of the key.)

```sh
arc-dm reply 1c77 yes, I have it
replied to 1c77da in chief-landau-268744ba
```

`arc-dm reply` takes at least four digits of an id, and sends to each
member of the conversation of that message. It needs no peer. It refuses
an id that names no message, or more than one. The confirmation names the
conversation, so a mistyped id that answers into a group shows at once.
`arc-dm send -reply <id> <peer>` refuses an id of another conversation.

A reply shows a quote line between its header and its text:

```text
2026-10-17 14:02  1c77da  you  delivered
  bring the map
2026-10-17 14:05  3f9a2c  chief-landau-268744ba  new
↳ 1c77da  you: bring the map
  yes, I have it
```

- The quote comes from the messages of this machine, never from the
  reply. The sender of a reply names an id, and can name any id.
- The quote line starts at column 0. Each line of a text starts with two
  spaces, so a quote that a sender types into a text shows inside the
  text.
- The quote is the first line of the parent that is not empty, escaped as
  `sdk/terminal` does, and cut so that the line takes at most 72 columns:
  a CJK ideograph or an emoji takes two. A parent of only spaces shows
  `(blank)`.
- The id on a quote line is at most 12 digits, then `…`.
- `-reply` can also come after the peer, before the text:
  `arc-dm send ada -reply 1c77 yes`. Each other word after the peer is
  text, so `arc-dm send ada -home is where the heart is` sends that text.
  After `--`, each word is text. An empty `-reply=` is refused.
- A parent that this machine does not hold shows
  `↳ 77aa01  (not on this device)`. A message that you sent from another
  device, or a held message of a stranger, is not held.
- A parent of another conversation shows
  `↳ 5e0c12  (in another conversation)`, and no text of it.
- A reply shows the direct parent only, one level.
- `arc-dm watch` adds each message that it receives. When a reply names a
  parent that the watch does not hold, it reads the outbox, in case you
  sent the parent after the watch started: at most once each 5 s,
  because a sender can name many ids. The reply, and each message after
  it, waits for that read, at most 5 s, so the order stays and the quote
  is not lost.
- arc sorts messages by the clock of each sender, so a reply can show
  above its parent.
- An `arc-dm` older than replies shows a reply as a plain message. A newer
  `arc-dm` with an older arc shows no quotes, and `arc-dm reply` fails with
  `unknown flag: --reply`.

## Where messages come from

`arc-dm` reads the store of this machine. Three commands fill it:
`arc sync`, `arc serve`, and `arc-dm watch`. A watch holds a watch on each
relay, so with `arc-dm watch` running, `arc-dm` shows each message as soon
as it arrives.

The conversation holds the messages that this machine received and sent.
A message that the citizen sent from another device is not in it.

## Unread messages

The read state stays on this device, in `read.json` in the state
directory. It holds the id of each received message that `arc-dm open` or
`arc-dm read` showed. Other devices of the citizen keep their own state.

The state holds ids, not a time. The time of a message is the clock of its
sender, and a message that someone carries by hand can arrive days after
newer messages. That message is still unread.

`arc-dm watch` marks no message read.

## Notifications

`arc-dm watch` prints each new message, and runs a notifier with two
arguments: the petname of the sender, and the first line of the text.

| `-notify` | Notifier |
| --- | --- |
| not set, on macOS | `osascript`: a notification of the system. |
| not set, on Linux | `notify-send`, if it is on `PATH`. |
| `none` | No notification. |
| a program | The program, as `<program> <sender> <text>`. |

A notifier that fails does not stop the watch.

## Safety

`arc-dm` escapes a message before it prints it, as `sdk/terminal` does:
each control character except a newline and a tab, each embedding,
override and isolate of right-to-left text, the line and paragraph
separators, invisible characters such as U+200B, tag characters outside
the three subdivision flags, and each backslash. ESC shows as `\x1b`. A
sender cannot move the cursor, change the terminal, reorder a line, or
hide text. A byte that is not UTF-8 is already U+FFFD when arc-dm gets
the message, so that byte is lost; each other character shows. A
notification through `notify-send`, by that name or by a path to it, also
escapes `&`, `<` and `>`, because many notification servers read the body
as markup. `arc-dm` escapes its own error line too, because it can hold
what arc wrote about a relay.

## Groups

Several peers, separated by commas, name a group:

```sh
arc-dm send ada,cy meet at noon
```

arc sends one message that each member sees, with the keys of all members
([the group messages spec](../../docs/groups/SPEC.md)). A group has at most
10 participants, with you.

- A conversation is the set of its members. A message to a different set
  of members makes a new conversation, as NIP-17 says.
- `arc-dm send` in a group sends the reply to each member.
- `arc-dm ls` shows the sender before the text of a received group
  message. A sent message shows `delivered to 1 of 2` until each member
  acknowledged it.
- arc prints the petname of each sender and recipient. A member that never
  sent you a message, and that you never wrote to, gets the petname that
  arc makes from its key.
- A member on an older arc sees a direct message from the sender. A reply
  from that member goes to the sender only.

## Tests

```sh
go test -count=1 ./apps/dm/...
```

The tests build arc and `arc-dm`, run a relay on this machine, and run the
commands as a person does: conversations, groups, unread counts, peer
names, control characters, replies, and a watch that notifies. A test
that needs a hostile reply sends it with `arc message send --arctest-tag`,
which only a test build of arc has.
