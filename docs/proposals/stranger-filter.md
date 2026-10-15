Status: proposed

# A filter for mail from strangers

No code implements this proposal. Each claim about ARC names a file and a
line, at commit `9408258`. Each number has a tag:

- *measured*: a run on a real machine. The source is given.
- *arithmetic*: a calculation from other numbers. The inputs are given.
- *estimated*: a judgement. No run supports it.

## 0. Bad news first

- The proof of work of a relay does not stop a sender who knows ARC. The
  relay checks NIP-13 work only when `--wrap-auth` is on and no key
  authenticated on the connection
  (`adapters/relay/limits/limits.go:123`). The ARC relay transport
  answers the NIP-42 challenge of an inbox relay with a one-time key
  ([the delivery spec](../delivery/SPEC.md), section 6.2). Any sender can
  do the same, and skip the work. `--wrap-pow` without `--wrap-auth` has
  no effect (`cmd/arc/main.go:490`, `cmd/arc/main.go:491`).
- No ARC sender makes proof of work today. Only the relay limits import
  `nip13`. A relay that asks for work from every sender refuses each ARC
  client.
- The delivery spec says that couriers "keep room for contacts" (sections
  4.4 and 6.2). No term and no code defines a contact. That text belongs
  to the mesh link, which is not built.
- NIP-13 hashes the whole event, so the cost of the work grows with the
  size of the wrap. The largest mail costs about 40 s of one core at
  20 bits (section 6.2).
- The relay form of the largest mail is exactly `MaxWrap`, 55,149 bytes
  (`core/private/size_test.go:77`). A `nonce` tag makes it larger. A
  build must change a protocol limit (section 3.5).

## 1. The problem

Anyone can send a gift wrap to a citizen. `core/mail` opens each wrap that
names the citizen (`core/mail/mail.go:429`), saves its seal
(`core/mail/mail.go:472`), and shows it in the inbox. It acknowledges each
message (`core/mail/mail.go:492`). It has no notion of a contact or a
stranger.

A one-time key signs each gift wrap
([the delivery spec](../delivery/SPEC.md), section 3.4). A relay therefore
cannot limit mail by its author. It sees only the wrap. The author is
inside the seal, and only the recipient can open the seal.

Thus:

- A sender with a list of keys can put text in front of each citizen, at
  no cost to the sender.
- Each message makes the recipient sign and send an acknowledgement. Each
  signature can be a round trip to a remote signer
  ([the devices spec](../devices/SPEC.md)). One spam message thus costs
  the recipient more work than it cost the sender.

Public posts are not the problem. A citizen pulls them, and an install
needs consent.

## 2. Terms

| Term | Meaning |
| --- | --- |
| contact | A key from which a citizen takes mail without proof of work. Section 3.1 defines the set. |
| stranger | Each key that is not a contact. |
| held mail | Mail from a stranger without enough proof of work. The device keeps it apart from the inbox. |
| work | The NIP-13 difficulty of the ID of one gift wrap, in bits. |

## 3. Proposed rules

### 3.1 Contacts

The contacts of a citizen are the union of:

1. The key of the citizen, and each key on its device list.
2. Each key on the contact list of the citizen.

The contact list:

- It is self data ([the delivery spec](../delivery/SPEC.md), section
  4.11). A device seals it as a draft with the `d` tag `arc-contacts`. It
  syncs through the storage citizens of the citizen, and never goes to a
  relay. It needs no new kind.
- When the citizen sends a rumor of kind 14, or of the private kind of an
  app, the device MUST add each recipient to the list before it seals the
  rumor.
- A call request (3272), a reply (3273) and an acknowledgement (3274) MUST
  NOT add a key. A device sends replies and acknowledgements without a
  person, and a call to a provider is not consent to its mail.
- The citizen can add and remove a key by hand.

ARC does not use the NIP-02 follow list (kind 3). It is public, so it
shows each relay who the citizen talks to. ARC publishes no follow list
today.

A citizen without storage citizens keeps the list on each device. The
devices then can disagree.

### 3.2 Where the check runs

- The recipient's device MUST run the check in `core/mail`, after it
  unwraps the gift wrap, and before it saves the seal. Only the seal names
  the author.
- `core/mail` gets the check as an injected interface, as the store gets
  the device list (`core/store/store.go:135`). Core reads no file.
  Runtime code reads the contact list and gives the answer.
- A relay takes no part. It cannot see the author.
- The sender's device does not learn the result. It sees an
  acknowledgement, or none.

### 3.3 What the check decides

The check applies to a rumor of kind 14, and to the private kind of an
app: the `default` arm of `open` (`core/mail/mail.go:489`).

| Author | Work of the wrap | Result |
| --- | --- | --- |
| contact | any | The seal goes to the store and the inbox. The device acknowledges it, as today. |
| stranger | at least 20 bits | The seal goes to the store and the inbox. The device acknowledges it. |
| stranger | less than 20 bits | The mail is held (section 3.4). The device sends no acknowledgement and no refusal. |

The check does not apply to:

- A call request (3272). The gate of the app decides who calls it, from
  the `allow` list of its Arcfile (`runtime/gate/gate.go:53`). A provider
  serves strangers by design.
- A reply (3273) or an acknowledgement (3274). They count only when they
  match an outbox entry to their author (`core/mail/mail.go:692`,
  `core/mail/mail.go:715`). Other ones change nothing, as today.
- A live call (kind 21059). It does not go through the mail.

Work on a wrap counts once. It does not make the author a contact. Each
message from a stranger needs its own work.

### 3.4 Held mail

- The device MUST keep held mail in a bucket of the mail database, apart
  from the store. The inbox reads each seal in the store
  (`core/mail/mail.go:906`), so a held seal must not be there.
- The bucket keeps at most 200 seals (estimated). Past that, the device
  drops the oldest. A held seal also goes when its wrap expires: 7 days
  by default (`core/private/private.go:39`).
- Held mail makes no notification, and `arc message` does not show it.
- The citizen lists held mail, and accepts a stranger. To accept a
  stranger puts the key on the contact list.
- When a key joins the contacts, on any device, the device MUST move the
  held mail of that key to the store, and acknowledge each message. If
  the wrap has not expired, the sender's outbox then shows it as
  delivered.
- If the device gets another wrap of a held seal, and that wrap has
  enough work, the device MUST move the seal to the store and acknowledge
  it.

The name of the bucket is `held`. The word "requests" means call requests
in `core/mail`: the bucket of received call requests is called `requests`
on disk (`core/mail/mail.go:55`), and PR #305 uses the word for them too.

Why held mail gets no answer:

- Each answer is a wrap that the recipient's signer must sign. An answer
  to each spam message gives a spammer a way to load the signer.
- An answer tells the stranger that the key is in use.

The sender is not left without a result. Its outbox shows the message as
expired after 7 days ([the delivery spec](../delivery/SPEC.md), section
4.2). A newer sender makes work when it can be a stranger (section 3.6),
so its mail is held only when the work is missing.

### 3.5 Proof of work

- Work follows NIP-13: a `nonce` tag on the gift wrap, and leading zero
  bits in its ID.
- The difficulty for strangers is 20 bits. It is a protocol constant, not
  a setting, so that each sender knows it.
- The recipient MUST count the work that the ID has, not the target in
  the `nonce` tag.
- A sender makes work only on the relay form of the wrap. The relay form
  and the courier form are two events with two IDs, so work on both costs
  twice. A stranger's courier-form wrap is held until the relay form of
  the same seal arrives.
- `MaxWrap` grows by 38 bytes, to 55,187 bytes (arithmetic: the tag
  `,["nonce","18446744073709551615","20"]` holds 38 bytes, with the
  largest nonce of 20 digits). This changes section 3.4 of the delivery
  spec. Without the change, a relay refuses the largest mail with work.

### 3.6 The sender

- A sender MUST make work when it holds no rumor from the recipient of a
  kind that adds a contact (kind 14, or the private kind of an app). Such
  a rumor shows that the recipient wrote to the sender, so the sender is
  a contact of the recipient (section 3.1).
- An acknowledgement from the recipient does not stop the work. A
  recipient acknowledges mail with work from a stranger, and the stranger
  stays a stranger.
- The sender makes the work before it gives the wrap to a transport, and
  keeps the wrap with its nonce in the outbox, so that each later sync
  sends the same wrap.

### 3.7 Group messages

[The group messages proposal](dm-groups.md) seals one rumor with several
`p` tags to each member.

- The author of the seal decides the check. The `p` tags MUST NOT make a
  key a contact, because any author can write any `p` tag.
- A member who sends to a group seals to each other member, so each
  member becomes a contact of that sender (section 3.1). Groups need no
  other rule.
- A new member who writes to a group makes work for each member that has
  not written to it (section 3.6). A group has at most 10 participants,
  so that is at most 9 wraps.

## 4. Commands

These names are proposed. The build can change them.

| Command | Does |
| --- | --- |
| `arc contacts` | Lists the contact list. |
| `arc contacts add <citizen>` | Adds a key, and moves its held mail to the inbox. |
| `arc contacts rm <citizen>` | Removes a key. Mail that is in the inbox stays there. |
| `arc message held` | Lists held mail: author, time and text. |

The filter is on by default. `arc message` prints how many held messages
wait, on standard error, so the change is visible.

## 5. Failures

| Failure | What happens |
| --- | --- |
| A stranger sends mail without work | The mail is held. The sender gets no acknowledgement. Its outbox shows the message as expired after 7 days, unless the citizen accepts the stranger first. |
| The work of a wrap is less than its `nonce` tag claims | The recipient counts the work of the ID. The mail is held. |
| More than 200 held seals | The device drops the oldest held seal. |
| Two devices of a citizen have different contact lists | One device can hold mail that the other shows. When the lists sync, the first device moves the mail to the inbox. |
| An older relay gets a wrap with work above 55,149 bytes | The relay refuses it with `invalid:`. The sender sees the refusal for that relay. |

## 6. Security and cost

### 6.1 What each party can do

| Party | Can | Cannot |
| --- | --- | --- |
| stranger | Send mail with work, and reach the inbox. Fill the held bucket with mail without work, and push out older held mail. | Reach the inbox without work. Learn whether its mail was held or lost. |
| relay | Drop mail, as today. | See the author, so it cannot apply the check. |
| contact | Send without work. | — |

A stranger can push real held mail out of the bucket with 200 cheap
messages. That mail then waits for the sender to try again. The work
rule keeps the inbox clean, and the bucket bounds the disk. Neither
stops a stranger from filling the bucket.

The device still unwraps each wrap, because only the seal names the
author. That is two NIP-44 decryptions in the signer for each wrap, as
today. The filter removes the signature of the acknowledgement for each
held message.

### 6.2 Cost of the work

Measured on an Apple M3 Max with `nip13.DoWork` of `fiatjaf.com/nostr`
(a fork at `github.com/gezibash/nostrlib`), on 2026-10-15:

| Wrap | One attempt, one core | 16 bits, one core | 20 bits, one core | 20 bits, 16 cores |
| --- | --- | --- | --- | --- |
| short message, 1,659 bytes | 1.32 µs (measured) | 86 ms (arithmetic) | 1.38 s (arithmetic) | 0.65 s, mean of 5 runs (measured) |
| largest mail, 54,962 bytes | 38.6 µs (measured) | 2.5 s (arithmetic) | 40 s (arithmetic) | — |

The arithmetic is one attempt times 2^16 or 2^20, the expected number of
attempts.

- At 20 bits, one core makes about 2,600 short messages to strangers each
  hour (arithmetic: 3,600 s / 1.38 s). A sender with many machines makes
  more. The work raises the cost of bulk mail; it does not stop a sender
  who pays.
- A phone is slower than this laptop. No run on a phone exists.
- 16 bits costs a sender 86 ms for each short message, which does not
  slow bulk mail (estimated). That is why the constant is 20.
- The largest mail to a stranger costs about 40 s of one core. A first
  message to a stranger is usually short (estimated).

## 7. Compatibility

- An older recipient opens all mail, as today. It ignores the `nonce`
  tag.
- An older sender makes no work. Its mail to a newer citizen that does
  not have it as a contact is held, and shows as expired in its outbox
  after 7 days. Before, the same mail showed as delivered. This is the
  visible change for older senders.
- NIP-17 clients outside ARC make no work for gift wraps. Their first
  mail to an ARC citizen is held.
- An older device of a citizen shows each message, held or not, because
  it has no filter. It ignores the draft `arc-contacts`.
- An older relay takes a wrap with work up to 55,149 bytes. It refuses a
  larger one (section 5).
- The relay flags `--wrap-auth` and `--wrap-pow` do not change.

The build changes normative sections of
[the delivery spec](../delivery/SPEC.md), which is `partial`:

- Section 3.4: `MaxWrap` becomes 55,187 bytes.
- Section 4.2: held mail gets no acknowledgement.
- Section 6.2: contacts, work, and held mail.
- Sections 4.4 and 6.2: the contacts of a courier are the contacts of
  section 3.1.

It ships as a breaking commit.

## 8. Proof that a build needs

- Two citizens on a relay, neither a contact of the other: a message
  without work is held, gets no acknowledgement, and is not in the inbox.
- The same with 20 bits of work: the message is in the inbox and gets an
  acknowledgement. The test makes the work with a real `nip13` search,
  not with a fixed nonce.
- A wrap whose `nonce` tag claims 20 bits and whose ID has fewer is held.
- `arc contacts add` moves held mail to the inbox, and the sender's
  outbox shows it as delivered.
- A citizen that sends a message adds the recipient to its contact list.
  The reply of the recipient, without work, reaches the inbox.
- A newer sender makes work for a first message, and none after the
  recipient wrote to it.
- A call request from a stranger reaches the provider without work.
- A group message from a stranger, with a contact in its `p` tags and no
  work, is held.
- The 201st held seal pushes out the oldest.
- The largest mail with work fits the new `MaxWrap`.
- A contact added on one device releases held mail on another device
  after a sync through a storage citizen.

## 9. Not in this proposal

- The relay check of section 0. A relay that asks for work from each
  sender, whatever the authentication, makes contacts make work too. That
  is a separate change to `adapters/relay/limits`.
- A difficulty that the recipient publishes. A fixed constant needs no
  lookup before a send. A published value can come later, in the NIP-17
  relay list.
- A per-byte difficulty, so that a large first message costs less.
