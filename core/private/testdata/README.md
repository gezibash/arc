# Test vectors for private mail

These files hold test vectors for `core/private`. A test vector is input
data and the exact output that the input must give. A second implementation
of ARC, for example a port to Swift, reads these files and checks its own
code against them.

All keys are test values. Each secret key is the SHA-256 hash of the text
`arc test vector <name>`, for example `arc test vector alice`. Never use these
keys for real mail.

All keys are 64 hex characters. A public key is the 32-byte x-only key of
BIP-340. An event is a Nostr event in the JSON form of NIP-01.

## route_tags.json

A route tag names a recipient for one UTC day. A courier sees the route tag,
but cannot find the key of the recipient from it.

The route tag of a recipient for a day is:

1. Write the day as `YYYY-MM-DD` in UTC.
2. Calculate HMAC-SHA256. The key is the 32 bytes of the public key of the
   recipient. The message is the bytes of `arc-route-v1`, one zero byte, and
   the day.
3. Keep the first 16 bytes of the result, as 32 lowercase hex characters.

Each vector has these fields:

| Field | Meaning |
| --- | --- |
| `description` | A short text about the vector. |
| `recipient` | The public key of the recipient. |
| `time` | The time, in RFC 3339 form. Some times have an offset other than UTC. |
| `unix` | The same time, in seconds since 1970-01-01T00:00:00Z. |
| `today` | The UTC day of `time`. |
| `route_tag` | The route tag of `recipient` for `today`. |
| `days` | Nine UTC days: 7 days before `today`, to 1 day after `today`. |
| `route_tags` | The route tag for each day in `days`, in the same order. |

A recipient asks for all nine route tags, because a wrap lives for up to
7 days and the clock of a sender can be 1 day ahead.

Convert `time` to UTC before you calculate the day. For example,
`2026-10-05T01:30:00+02:00` is on the UTC day `2026-10-04`.

## wraps.json

A message is a rumor inside a seal inside a gift wrap, as NIP-59 defines.
The rumor is an unsigned event. The seal (kind 13) encrypts the rumor with
NIP-44, and the author signs the seal. The wrap (kind 1059) encrypts the seal
with NIP-44 under a one-time key.

A seal and a wrap use a random one-time key, a random nonce and a random
`created_at`. Thus the same input gives a different seal and a different
wrap each time. These vectors are recorded once. Use them to test how your
code opens a wrap. Do not use them to compare the bytes of a wrap that your
code makes.

The file has these sections:

| Field | Meaning |
| --- | --- |
| `description` | A short text about the file. |
| `keys` | The secret key and the public key of each test citizen. |
| `open` | Wraps that the recipient must open. |
| `refuse` | Wraps and seals that the recipient must refuse. |

Each vector in `open` has these fields:

| Field | Meaning |
| --- | --- |
| `description` | A short text about the vector. |
| `recipient_secret` | The secret key of the recipient. |
| `form` | `relay`: the wrap has a `p` tag with the public key of the recipient. `courier`: the wrap has a `w` tag with a route tag, and no `p` tag. |
| `route_day` | Only for the `courier` form. The UTC day of the route tag in the `w` tag. |
| `expires` | The value of the `expiration` tag of the wrap, in seconds since 1970. |
| `wrap` | The gift wrap event. |
| `seal` | The seal that the wrap holds, exactly. |
| `seal_plaintext` | The exact text that the seal decrypts to. This text is the JSON of the rumor. |
| `author` | The public key of the author. The seal proves the author. |
| `rumor` | The rumor that the seal holds: `id`, `pubkey`, `created_at`, `kind`, `tags` and `content`. |

To check an `open` vector:

1. Decrypt `wrap.content` with NIP-44. Use `recipient_secret` and
   `wrap.pubkey`. The result must be the JSON of `seal`.
2. Make sure that the seal has kind 13, has no tags, and has a valid id and
   signature.
3. Decrypt `seal.content` with NIP-44. Use `recipient_secret` and
   `seal.pubkey`. The result must be `seal_plaintext`.
4. Read the rumor from `seal_plaintext`. Calculate its id from the NIP-01
   serialization. The rumor must be equal to `rumor`.
5. Make sure that the `pubkey` of the rumor is equal to the `pubkey` of the
   seal. This is `author`.
6. For the `courier` form, calculate the route tag of the recipient for
   `route_day`. The result must be equal to the value of the `w` tag.

Each vector in `refuse` has these fields:

| Field | Meaning |
| --- | --- |
| `description` | A short text about the vector. |
| `recipient_secret` | The secret key of the citizen that tries to open the event. |
| `wrap` | A gift wrap to open. Each vector has a `wrap` or a `seal`. |
| `seal` | A seal to open. Each vector has a `wrap` or a `seal`. |
| `error` | The class of the error. |

The error classes are:

| Class | Meaning |
| --- | --- |
| `wrap-does-not-open` | The NIP-44 decryption of the wrap fails. The key is wrong, or the ciphertext changed. |
| `seal-does-not-open` | The NIP-44 decryption of the seal fails. |
| `rumor-author-mismatch` | The `pubkey` of the rumor is not equal to the `pubkey` of the seal. Without this check, any author can claim to be another author. |

Your code must refuse each `refuse` vector. If your code gives other error
names, map them to these classes. The Go code has no error values for these
classes. The Go test compares the text of the error.

The wrap with a changed ciphertext has a valid signature. Thus your code
gets to the NIP-44 check, also if it checks the signature of a wrap first.

## How the files were made

`route_tags.json` and `../../mail/testdata/message_rumor.json` were
calculated with the Python standard library, not with this package. Do not
change them to agree with a change in the Go code. If the Go code does not
agree with them, the wire format changed.

`wraps.json` was recorded with this command:

```sh
go test ./core/private -run '^TestWrapVectors$' -update
```

The command writes `wraps.json` again, with new random seals and wraps. The
rumor of each `open` vector must stay equal to the first vector of
`../../mail/testdata/message_rumor.json`. `TestWrapVectorsHoldTheMessageRumor`
checks this.

## How the Go code uses the files

```sh
go test ./core/private -run 'Vector'
```

The test fails if a file is missing or holds no vectors.
