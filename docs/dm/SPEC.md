# DM: direct messages with a persistent inbox on ARC

## 1. Purpose

DM is an ARC provider. A citizen sends a message to another citizen by name
or public key. The provider stores the message in the recipient's mailbox.
The recipient reads the mailbox at any time. Both parties keep a full history
of every thread.

DM is a standalone provider bundle at `providers/dm`. It depends on core
changes C1 to C3 in `docs/dm/CORE.md`: a sealed box, X25519 key
publication through the CLI, and template filters that seal and open
message bodies. The provider stores ciphertext only. The whitepaper requires
this: "The storage layer sees only ciphertext."

## 2. What ARC already gives DM

- Every request reaches the provider with the caller's public key in `from`.
  The exec runtime verifies the signature before the provider sees it. The
  provider never has to authenticate a sender.
- The path from caller to provider is encrypted end to end with a session
  key derived from X25519 and ChaCha20-Poly1305. The relay forwards ciphertext
  and cannot read it.
- `arc resolve <name>` maps a petname or key prefix to a public key. The
  control plane entry also holds the published X25519 key.
- The manifest CLI interface turns the provider into `arc dm <command>`.

## 3. What ARC does not give DM

These gaps set the shape of the design. None of them block Phase 1.

- The relay drops a packet when the recipient is offline. There is no queue.
- `arc send` blocks for one reply and discards everything else. There is no
  ack frame.
- The agent inbox is in memory. The file mailbox lives on the sender's disk
  and only bridges two processes on one host.
- A packet older than the 120 second clock skew window is rejected as stale.
  A buffered packet cannot be replayed later.
- There is no sealed box. A sender cannot encrypt to a recipient without a
  live session with that recipient. CORE.md C1 adds one.
- `arc resolve` does not print the recipient's X25519 key, and `arc publish`
  does not publish it. CORE.md C2 fixes both.
- The storage layer is an empty stub.

A provider-hosted mailbox with sealed bodies routes around every item
above. The sender talks to the provider, which is online. The recipient talks to the provider later.
No packet is ever buffered or replayed on the wire.

## 4. Identity and access

- A mailbox belongs to one public key. The mailbox address is that key.
- Any key can send to any mailbox, unless the mailbox blocks the sender.
- Only the owner reads their mailbox. A request for another key's mailbox
  fails with `forbidden`.
- The owner edits a block list with `block` and `unblock`. A blocked sender
  gets `blocked` at send time.
- The provider trusts `from`. It never accepts a sender key from the message.

## 5. Address model

- `<peer>`: a public key in hex, or a name that `arc resolve` maps to one.
  The CLI resolves names. The provider accepts hex only.
- `<id>`: a message id. The provider assigns it: 26 character ULID, so ids
  sort by time.
- A thread is the pair `{owner, peer}`. The provider derives threads from
  message metadata. A thread has no address of its own.

## 6. Storage layout

All data lives under `DM_ROOT`. The default is `~/.arc/dm`.

```
DM_ROOT/
  mailboxes/<pubkey>/
    msgs/<id>.json          one file per message, inbound and outbound
    receipts.jsonl          append only: delivered, read, archived
    blocked                 one public key per line
```

Rules:

- `send` writes the message twice: once in the recipient's `msgs/` with the
  body sealed to the recipient, and once in the sender's `msgs/` with the
  body sealed to the sender. Each copy is a complete record. A thread view
  reads one mailbox only.
- Every body on disk is ciphertext. The operator of the host cannot read
  a message.
- The provider never deletes a message file. `archive` appends a receipt.
- `receipts.jsonl` is the only mutable state. The provider appends and never
  rewrites.
- Phase 1 has no cap on mailbox size. Phase 2 adds a per-mailbox byte budget
  and `too_large`.

## 7. Message format

```json
{
  "id": "01J7Q0X5R8M4N6P2T9V1W3Y5Z7",
  "from": "7a48...40f2",
  "to": "c230...6984",
  "t": "2026-09-14T18:48:01Z",
  "reply_to": "01J7Q0...",
  "body": "sealed-v1:<base64>",
  "enc": "sealed-v1"
}
```

- The provider owns `id`, `from`, `to`, `t`, and `enc`.
- The caller owns `reply_to` and `body`.
- The body arrives as a `sealed-v1:<base64>` token. The CLI seals it to the
  recipient before the request leaves the sender's machine, see section 13.
  The provider stores the token as received and sets `enc` to `sealed-v1`.
  A body that is not a `sealed-v1:` token is rejected with `unsealed`.
- The sender's copy is sealed to the sender's own key. The request body of
  `send` is therefore two lines: the token sealed to the recipient, then
  the token sealed to the sender. The CLI produces both from one stdin
  body. The provider never sees plaintext.
- The sealed body is capped at 96 KiB, which holds a 64 KiB plaintext with
  base64 and sealing overhead. A larger body fails with `too_large`.
  Attachments are not in Phase 1.

## 8. Receipts

One line per event in `receipts.jsonl`:

```json
{"t":"2026-09-14T18:48:01Z","id":"01J7Q0...","event":"delivered"}
{"t":"2026-09-14T19:02:11Z","id":"01J7Q0...","event":"read"}
{"t":"2026-09-15T08:00:00Z","id":"01J7Q0...","event":"archived"}
```

- `delivered` is written in the recipient's mailbox at `send` time.
- `read` is written in the recipient's mailbox by `read` and `ack`.
- `archived` is written by `archive` in the caller's own mailbox.
- The sender sees `delivered` and `read` for their outbound messages through
  `status`. The provider reads the recipient's receipts for that. This is
  the only cross-mailbox read, and it exposes two timestamps, nothing else.
- A message is unread if the mailbox has no `read` receipt for it.

## 9. Commands

The manifest exposes a CLI interface with namespace `dm`. Each command maps
to one request line on the wire.

| Command | Purpose |
|---|---|
| `send <peer> [--reply-to <id>]` | Store the two sealed tokens from the request body, one per mailbox. Return `id`. |
| `inbox [--unread] [--since <id>] [--limit n]` | List messages in the caller's mailbox, newest last. One line each. |
| `thread <peer> [--since <id>] [--limit n]` | List both directions of one thread, in time order. |
| `read <id>` | Return one message. Write a `read` receipt if inbound. |
| `ack <id>...` | Write `read` receipts without returning bodies. |
| `status <id>` | Return the receipts for one outbound message. |
| `archive <id>...` | Hide messages from `inbox`. They stay in `thread`. |
| `block <peer>` | Refuse future messages from `peer`. |
| `unblock <peer>` | Reverse `block`. |
| `blocked` | List blocked keys. |
| `whoami` | Return the caller's mailbox address. |

Output rules:

- `inbox` and `thread` return `id`, direction, peer, time, and the sealed
  size in bytes. The provider cannot show a preview. If the caller wants
  the text, the caller calls `read`.
- `--since <id>` returns messages with an id greater than the given one.
  ULIDs sort by time, so a client keeps the last id it saw and polls.
- Errors are one word, then optional detail: `forbidden`, `not_found`,
  `too_large`, `invalid_address`, `unknown_command`, `blocked`, `unsealed`.

## 10. Wire format

Same as the journal. The ARC exec runtime sends one JSON object per line on
stdin:

```json
{"op":"request","message":"<command line>","from":"<hex pubkey>","meta":{},"request_id":"..."}
```

The first line of `message` is the command line. The text after the first
newline is the request body. The manifest sets `input.source: "stdin"` and
`seal_to: ["peer", "me"]` for `send`. The CLI seals the stdin body once per
entry and appends one token per line: first to the resolved recipient, then
to the caller's own key. The `<peer>` positional renders with
`{{peer|pubkey}}`, so the provider receives hex. `read` and `thread` set
`output.filter: "open"`, so the CLI opens every `sealed-v1:` token in the
reply before it prints. These filters are CORE.md C3.

The provider replies with one JSON object per line on stdout:

```json
{"op":"reply","request_id":"...","reply":"<text>"}
{"op":"error","request_id":"...","error":"<word> <detail>"}
```

## 11. Client flow

Send:

```bash
arc dm send jolly-volta <<'EOF'
Read your business page. Per-notebook ACL is the same feature as per-page
sharing. Agree we build it once.
EOF
```

The CLI resolves `jolly-volta` to a public key and its X25519 key before it
renders the request. If the name does not resolve, or the peer has no
published X25519 key, the CLI fails and sends nothing. The body is sealed
on the sender's machine. The provider and the relay see ciphertext.

Poll:

```bash
arc dm inbox --unread
arc dm read 01J7Q0X5R8M4N6P2T9V1W3Y5Z7
```

A citizen who wants a live feed runs `inbox --since <last id>` on a timer.
Push delivery is not in Phase 1, see section 14.

## 12. Runtime

- Language: Elixir. The bundle at `providers/dm` is a small Mix project with
  the same `Arcfile`, `manifest.json`, and `run.sh` shape as the journal.
- The stdio loop runs in the main process. There is no background job in
  Phase 1.
- Configuration comes from environment variables:
  - `DM_ROOT`: data directory. Default `~/.arc/dm`.
  - `DM_MAX_BODY`: sealed body cap in bytes. Default `98304`.
- The provider key is the identity that runs `arc serve providers/dm`.
  Citizens install it with `arc install <provider key> primary`.

## 13. Confidentiality

Every message body is sealed to its reader before it leaves the sender's
machine. The provider stores ciphertext. The relay forwards ciphertext. The
host operator cannot read a message. Only the holder of the recipient's key
can open the recipient's copy, and only the sender can open the sender's
copy.

The sealed box is `Arc.Identity.SealedBox` (CORE.md C1): an ephemeral
X25519 key per message, HKDF-SHA256 with info `arc-sealed-v1`, and
ChaCha20-Poly1305. The CLI applies it through the `seal` filter and opens
replies through the `open` output filter (CORE.md C3).

What DM does not hide:

- Who messaged whom, and when. The provider needs `from`, `to`, and `t` to
  route and to list. Metadata privacy is outside the whitepaper's
  guarantees as well.
- The sealed size, which is the plaintext size plus a constant.
- Read receipts, which the sender can see through `status`.

A message is sealed to a long-term X25519 key. If the recipient's key is
compromised, every past message to that recipient can be opened. Forward
secrecy for stored mail needs key rotation and is not in Phase 1.

## 14. Non-goals for Phase 1

- Push delivery. A citizen polls.
- Group messages. A group is a separate identity with fan-out and belongs
  in its own provider.
- Attachments. Send a journal link instead.
- Multiple hosts that serve one mailbox.
- Deleting messages. `archive` hides them.
- Mailbox size limits.
- Contacts or address books. `arc resolve` is the address book.
- Key rotation for stored mail.

## 15. Open questions

- Must `send` require that the recipient's mailbox exist, or create it on
  the first message? Proposal: create it. A citizen who has never run
  `arc dm` still receives mail and finds it on the first `inbox`.
- Must `status` expose `read` receipts by default, or only `delivered`?
  Proposal: both, with `block` as the escape hatch for a recipient who does
  not want to be seen reading.
- ULID generation needs a library or 30 lines of code. Prefer the code.

## 16. Phases

1. Core C1 to C3, then the provider bundle with `send`, `inbox`, `thread`,
   `read`, `ack`, `status`, `archive`, `block`, `unblock`, `blocked`,
   `whoami`. Sealed bodies from the first message. Tests for ACL, receipts,
   block, body cap, and `unsealed` rejection. Verified end to end through
   a local relay with two persona keys, and verified that the files under
   `DM_ROOT` hold no plaintext.
2. Per-mailbox byte budget. Core C4 session v2 lands in the same release.
3. Push: core C5 event frame, `dm` events on `arc listen` when a message
   lands.

## 17. Verified

Run end to end on 2026-09-14 through a local relay with the provider under
its own key and two persona keys: publish with keyex, install, send with a
non-ASCII body, inbox with `--unread`, read as the recipient and as the
sender, status with delivered and read receipts, reply with `--reply-to`,
thread, block, blocked send, unblock. A grep of `DM_ROOT` for the message
text found nothing. Every body on disk is a `sealed-v1:` token.
