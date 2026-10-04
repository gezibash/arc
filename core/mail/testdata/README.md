# Test vectors for mail

These files hold test vectors for the message rumor and the
acknowledgement of `core/mail`. A test vector is input data and the exact output that the input
must give. A second implementation of ARC reads this file and checks its own
code against it.

All keys are test values. The secret key of the sender is the SHA-256 hash of
the text `arc test vector alice`. Never use this key for real mail.

A message rumor is an unsigned Nostr event, as NIP-17 defines:

- The kind is 14.
- The `pubkey` is the public key of the sender.
- The `created_at` is the time of the send, in seconds since 1970.
- The tags are one `p` tag with the public key of the recipient.
- The `content` is the text of the message.
- The `id` is the SHA-256 hash of the NIP-01 serialization.

The rumor has no signature. The seal of `core/private` proves the author.
See `../../private/testdata/README.md`.

Each vector has these fields:

| Field | Meaning |
| --- | --- |
| `description` | A short text about the vector. |
| `sender_secret` | The secret key of the sender, as 64 hex characters. |
| `recipient` | The public key of the recipient, as 64 hex characters. |
| `text` | The text of the message. |
| `unix` | The time of the send, in seconds since 1970-01-01T00:00:00Z. |
| `rumor` | The rumor: `id`, `pubkey`, `created_at`, `kind`, `tags` and `content`. |
| `serialized` | The exact NIP-01 serialization of the rumor: `[0,pubkey,created_at,kind,tags,content]`. |

To check a vector:

1. Calculate the public key of `sender_secret`. The result must be
   `rumor.pubkey`.
2. Make the rumor from the inputs.
3. Serialize the rumor as NIP-01 defines. The result must be equal to
   `serialized`, byte for byte.
4. Calculate the SHA-256 hash of `serialized`. The result must be
   `rumor.id`.

The second vector has a line break, a tab, quotation marks, a backslash,
non-ASCII letters and HTML characters in its text. NIP-01 escapes only some
characters. Use this vector to check how your code escapes JSON.

## The time of the vectors

The time of each vector is in the year 2100. The Go test sends each message
with a fixed clock. A wrap expires 7 days after the clock time, and the event
store refuses a wrap that expired by the wall clock. With a time in 2100, the
recorded wraps do not expire. The time is also larger than 2^31 seconds. Use
a 64-bit integer for `created_at`.

## ack_rumor.json

An acknowledgement tells the sender that the recipient got a message. When
the sender opens a valid acknowledgement, the outbox of the sender shows the
message as `delivered`. A valid acknowledgement is an unsigned Nostr event:

- The kind is 3274.
- The `pubkey` is the public key of the recipient of the message.
- The tags are one `e` tag with the id of the message rumor.
- The `content` is empty.
- The recipient wraps it to the sender, as a message is wrapped.

The file has these sections:

| Field | Meaning |
| --- | --- |
| `description` | A short text about the file. |
| `message` | The message that the vectors acknowledge: `sender_secret`, `recipient_secret`, `text`, `unix` and the rumor `id`. It is the first vector of `message_rumor.json`. |
| `vectors` | The acknowledgements. |

Each vector has these fields:

| Field | Meaning |
| --- | --- |
| `description` | A short text about the vector. |
| `author_secret` | The secret key of the author of the acknowledgement. |
| `unix` | The time of the acknowledgement, in seconds since 1970. |
| `rumor` | The acknowledgement rumor: `id`, `pubkey`, `created_at`, `kind`, `tags` and `content`. |
| `serialized` | The exact NIP-01 serialization of the rumor. |
| `delivers` | `true` if the acknowledgement marks the message delivered. |

The first vector is the acknowledgement that the recipient makes. The other
vectors must not mark the message delivered:

- An acknowledgement by another citizen than the recipient of the message.
  Without this check, any citizen can mark a message delivered.
- An acknowledgement with no `e` tag.

## ack_wraps.json

This file holds gift wraps of the acknowledgements in `ack_rumor.json`, in
the relay form. The sender of the message opens each wrap. The first wrap
is the wrap that the Go code of the recipient made. A seal and a wrap use
random keys, so this file is recorded once.

Each vector has these fields:

| Field | Meaning |
| --- | --- |
| `description` | A short text about the vector. It is equal to the description in `ack_rumor.json`. |
| `sender_secret` | The secret key of the sender of the message. The sender opens the wrap. |
| `wrap` | The gift wrap event. |
| `rumor` | The acknowledgement rumor that the wrap holds. It is equal to the rumor in `ack_rumor.json`. |
| `acknowledges` | The value of the `e` tag: the id of the acknowledged message. It is empty if the rumor has no `e` tag. |
| `delivers` | `true` if the acknowledgement marks the message delivered. |

To check a vector:

1. Put the message of `ack_rumor.json` in the outbox of the sender.
2. Open `wrap` with `sender_secret`, as `../../private/testdata/README.md`
   describes. The result must be `rumor`.
3. Read the `e` tag. The value must be `acknowledges`.
4. If `delivers` is `true`, the outbox must show the message as delivered.
   If `delivers` is `false`, the outbox must not show the message as
   delivered.

## How the files were made

The Python standard library calculated `message_rumor.json` and
`ack_rumor.json`. The Go code did not. Do not change these files to agree
with a change in the Go code. If the Go code does not agree with them, the
wire format changed.

This command records `ack_wraps.json`:

```sh
go test ./core/mail -run '^TestAckRumorVectors$' -update
```

## How the Go code uses the files

```sh
go test ./core/mail -run 'Vectors'
```

`TestMessageRumorVectors` sends each message with a fixed clock, and reads
the rumor back from the outbox. `TestAckRumorVectors` sends the message to
the recipient, and compares the acknowledgement that the recipient makes
with the first vector. Then the sender opens it, and the message shows as
delivered. `TestAckWrapVectors` gives each recorded wrap to the sender, and
checks `delivers`. A test fails if a file is missing or holds no vectors.
