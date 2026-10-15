# Direct messages app

Direct messages are private messages, as NIP-17 defines. The program
`arc-dm` is the messaging app of a person: it groups the messages by peer,
counts the unread messages, and posts a notification when a message comes.

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
arc-dm read [peer]             # mark a conversation read, or all of them
arc-dm watch                   # print and notify each message as it arrives
```

A peer is a public key, a petname, or the start of either. A name that
selects more than one conversation is refused. To write to a citizen for
the first time, give its public key, as `arc whoami` prints it.

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

A message goes to one citizen. A group with a shared list of members needs
a change to the protocol; see [the proposal](../../docs/proposals/dm-groups.md).

## Tests

```sh
go test -count=1 ./apps/dm/...
```

The tests build arc and `arc-dm`, run a relay on this machine, and run the
commands as a person does: conversations, unread counts, peer names,
control characters, and a watch that notifies.
