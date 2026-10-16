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

`arc-dm` replaces each control character of a message, except a newline
and a tab, before it prints the message. A sender cannot move the cursor
or change the terminal.

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
names, control characters, and a watch that notifies.
