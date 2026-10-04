# Test vectors for the message rumor

`message_rumor.json` holds test vectors for the message rumor of
`core/mail`. A test vector is input data and the exact output that the input
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

## How the file was made

The file was calculated with the Python standard library, not with the Go
code. Do not change the file to agree with a change in the Go code. If the
Go code does not agree with the file, the wire format changed.

## How the Go code uses the file

```sh
go test ./core/mail -run '^TestMessageRumorVectors$'
```

The test sends each message with a fixed clock, and reads the rumor back
from the outbox. The test fails if the file is missing or holds no vectors.
