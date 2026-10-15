Status: proposed

# Stamps and a filter for mail from strangers

No code implements this proposal. Each claim about ARC names a file and a
line, at commit `9b1893f`. Each number has a tag:

- *measured*: a run on a real machine. The source is given.
- *arithmetic*: a calculation from other numbers. The inputs are given.
- *estimated*: a judgement. No run supports it.

This proposal has two parts that work together:

- A **filter** on the recipient's device. Only the device sees the author
  of a gift wrap, so only the device can tell a contact from a stranger.
- **Stamps** for the relay. A stamp is an anonymous, single-use token that
  the recipient gives to its contacts. An inbox relay takes a gift wrap with
  a valid stamp, or with proof of work, and refuses the others. The relay
  does not learn which contact sent the wrap.

## 0. Bad news first

- **The relay check of today does not stop a sender who knows ARC.** The
  relay checks NIP-13 work only when `--wrap-auth` is on and no key
  authenticated on the connection (`adapters/relay/limits/limits.go:123`).
  The relay transport answers the NIP-42 challenge of an inbox relay with a
  new random key (`runtime/citizen/session.go:114`). Any sender can do the
  same, and skip the work. `--wrap-pow` without `--wrap-auth` has no effect
  (`cmd/arc/main.go:490`, `cmd/arc/main.go:491`). Section 3.10 replaces
  this check for kind 1059.
- **No ARC sender makes proof of work or stamps today.** Only the relay
  limits import `nip13`. A relay that enforces the rule of section 3.10
  refuses each older ARC sender, and each NIP-17 client outside ARC, that
  writes to a citizen who published a stamp key.
- **Blind stamps hide nothing from the recipient.** The recipient opens the
  seal and sees the author of each wrap. The blind signature hides from the
  relay which contact sent a wrap. It keeps what a relay sees today: the
  relay cannot link two wraps to one sender. A relay that works with the
  recipient learns the author, as today.
- **A stamp tells the relay one fact.** The relay learns that a wrap came
  from someone that the recipient gave a stamp to, or from someone who paid
  work. Today the relay does not learn this.
- **A stamp is large.** The tag holds 485 bytes (section 3.14). A short
  message wrap holds 1,659 bytes (measured, section 6.3), so the stamp adds
  29% (arithmetic: 485 / 1,659). `MaxWrap` grows from 55,149 to 55,634
  bytes. That changes a protocol limit in section 3.4 of
  [the delivery spec](../delivery/SPEC.md).
- **A stamp is readable on the relay.** An ARC relay serves a gift wrap to
  any reader: kind 1059 is not on the list of sealed kinds
  (`adapters/relay/sealed/sealed.go:64`). A reader can copy a stamp from a
  wrap, and spend it first at another inbox relay of the recipient. A stamp
  cannot be bound to its wrap, because the stamp exists before the wrap
  (section 3.9). A copied stamp works at most once on each relay.
- **A contact can pass stamps on.** A contact who sells or leaks stamps
  lets a spammer reach the inbox relays. The limit of section 3.8 bounds
  this to 300 wraps on each relay in each key period (estimated). The
  recipient's device still holds that mail, because the author is not a
  contact. Removing a contact stops new stamps only. The old stamps work
  until their key period ends: at most 32 days (arithmetic: 30 + 2).
- **Issuance is asynchronous.** A stamp request is private mail. The first
  message to a contact can go out before the stamps arrive, and then it
  costs work if every inbox relay of the recipient enforces the rule.
- **A citizen without storage citizens issues stamps from one machine.** The
  stamp key is self data (section 3.7). Self data syncs only through storage
  citizens. Without them, only the machine that holds the citizen key can
  answer a stamp request.
- **A new dependency.** Issuance and unblinding use
  `github.com/cloudflare/circl/blindsign/blindrsa`. `go.mod` does not name
  it today.
- **The courier form gets no stamp.** A stamp on a courier-form wrap would
  name the recipient that the route tag hides. A relay that enforces the
  rule takes a courier-form wrap only with work (section 3.10).
- **Live calls keep the gap.** Kind 21059 stays outside this proposal. The
  bypass of the first item stays open for it.
- The delivery spec says that couriers "keep room for contacts" (sections
  4.4 and 6.2). No term and no code defines a contact. That text belongs
  to the mesh link, which is not built. Section 3.1 defines the term.

## 1. The problem

Anyone can send a gift wrap to a citizen. `core/mail` opens each wrap that
names the citizen (`core/mail/mail.go:551`, `core/mail/mail.go:552`), saves
its seal (`core/mail/mail.go:589`), and shows it in the inbox. It
acknowledges each message (`core/mail/mail.go:609`). It has no notion of a
contact or a stranger.

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
- Each spam wrap takes space on the inbox relays, and the recipient's
  device downloads it and decrypts it twice.

A filter on the device stops the first two. Only a check on the relay
stops the third. The relay cannot see the author, so it needs a proof that
does not name the author: a stamp, or work.

Public posts are not the problem. A citizen pulls them, and an install
needs consent.

## 2. Terms

| Term | Meaning |
| --- | --- |
| contact | A key from which a citizen takes mail without proof of work. Section 3.1 defines the set. |
| stranger | Each key that is not a contact. |
| held mail | Mail from a stranger without enough proof of work. The device keeps it apart from the inbox. |
| work | The NIP-13 difficulty of the ID of one gift wrap, in bits. |
| stamp | A token of RFC 9578, token type 2, that a citizen issues. One stamp lets one gift wrap into each inbox relay of that citizen, once. ARC already uses "token" for the token of a device (kind 20274), so this proposal says "stamp". |
| stamp key | An RSA key pair of 2048 bits that issues the stamps of one citizen for one key period. It is not the citizen key. |
| key period | The 30 days in which one stamp key issues stamps. |
| issuer | The citizen who makes a stamp: the recipient of the mail that the stamp pays for. |
| reply stamp | A stamp that a citizen makes of its own key and puts in a rumor, so that the recipient can answer without work (section 3.12). |

In the examples, Bob is the recipient and the issuer, and Alice is the
sender.

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
- A call request (3272), a reply (3273), an acknowledgement (3274), a
  stamp request (3277) and a stamp reply (3278) MUST NOT add a key. A
  device sends these without a person. A call to a provider is not consent
  to its mail.
- The citizen can add and remove a key by hand.

ARC does not use the NIP-02 follow list (kind 3). It is public, so it
shows each relay who the citizen talks to. ARC publishes no follow list
today.

A citizen without storage citizens keeps the list on each device. The
devices then can disagree.

The contact list is also the attester of the stamps: a device issues
stamps only to a key on it (section 3.8).

### 3.2 Where the filter runs

- The recipient's device MUST run the filter in `core/mail`, after it
  unwraps the gift wrap, and before it saves the seal. Only the seal names
  the author.
- `core/mail` gets the filter as an injected interface, as the store gets
  the device list (`core/store/store.go:137`). Core reads no file.
  Runtime code reads the contact list and gives the answer.
- The relay cannot run the filter. It cannot see the author. It runs the
  stamp rule of section 3.10, which does not need the author.
- The sender's device does not learn the result of the filter. It sees an
  acknowledgement, or none.

### 3.3 What the filter decides

The filter applies to a rumor of kind 14, and to the private kind of an
app: the `default` arm of `open` (`core/mail/mail.go:606`).

| Author | Work of the wrap | Result |
| --- | --- | --- |
| contact | any | The seal goes to the store and the inbox. The device acknowledges it, as today. |
| stranger | at least 20 bits | The seal goes to the store and the inbox. The device acknowledges it. |
| stranger | less than 20 bits | The mail is held (section 3.4). The device sends no acknowledgement and no refusal. |

The filter does not look at a stamp. A stamp proves to a relay that the
recipient gave it to someone. The device knows the author, and the contact
list decides.

The filter does not apply to:

- A call request (3272). The gate of the app decides who calls it, from
  the `allow` list of its Arcfile (`runtime/gate/gate.go:53`). A provider
  serves strangers by design.
- A reply (3273) or an acknowledgement (3274). They count only when they
  match an outbox entry to their author (`core/mail/mail.go:804`,
  `core/mail/mail.go:829`). Other ones change nothing, as today.
- A stamp request (3277) or a stamp reply (3278). Section 3.8 gives their
  rules: a device answers a request only from a contact, and keeps
  nothing from a stranger.
- A live call (kind 21059). It does not go through the mail.

Work on a wrap counts once. It does not make the author a contact. Each
message from a stranger needs its own work.

### 3.4 Held mail

- The device MUST keep held mail in a bucket of the mail database, apart
  from the store. The inbox reads each seal in the store
  (`core/mail/mail.go:1047`), so a held seal must not be there.
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
on disk (`core/mail/mail.go:63`).

Why held mail gets no answer:

- Each answer is a wrap that the recipient's signer must sign. An answer
  to each spam message gives a spammer a way to load the signer.
- An answer tells the stranger that the key is in use.

The sender is not left without a result. Its outbox shows the message as
expired after 7 days ([the delivery spec](../delivery/SPEC.md), section
4.2). A newer sender makes work when it can be a stranger (section 3.11),
so its mail is held only when the work is missing.

### 3.5 Proof of work

- Work follows NIP-13: a `nonce` tag on the gift wrap, and leading zero
  bits in its ID.
- The difficulty is 20 bits. It is a protocol constant, not a setting, so
  that each sender knows it. The device filter and the relay rule use the
  same constant.
- The recipient and the relay MUST count the work that the ID has, not the
  target in the `nonce` tag.
- A sender makes work only on the relay form of the wrap. It does not
  post the courier form to a relay that enforces the rule (section
  3.11). The relay form and the courier form are two events with
  two IDs, so work on both costs twice. A stranger's courier-form wrap is
  held until the relay form of the same seal arrives.
- A wrap MUST NOT carry both a `nonce` tag and a `stamp` tag. Either one
  admits the wrap to an inbox relay.

### 3.6 Stamps

A stamp is a token of RFC 9578, token type 2 (`0x0002`): a publicly
verifiable token, signed with blind RSA of 2048 bits. The blind signature
is RSABSSA-SHA384-PSS-Deterministic of RFC 9474: SHA-384, MGF1 with
SHA-384, a salt of 48 bytes, and the identity preparation function.

The stamp has the token structure of RFC 9577, section 2.2:

| Field | Bytes | Value |
| --- | --- | --- |
| `token_type` | 2 | `0x0002` |
| `nonce` | 32 | random, chosen by Alice |
| `challenge_digest` | 32 | SHA-256 of the challenge below |
| `token_key_id` | 32 | SHA-256 of the DER `SubjectPublicKeyInfo` of the stamp key, with the RSASSA-PSS OID, as RFC 9578, section 6.5, defines |
| `authenticator` | 256 | the RSA-PSS signature of the first four fields |

The total is 354 bytes (arithmetic: 2 + 32 + 32 + 32 + 256).

The challenge binds a stamp to one issuer and one key period. It is the
`TokenChallenge` of RFC 9577, section 2.1:

| Field | Value |
| --- | --- |
| `token_type` | `0x0002` |
| `issuer_name` | the 32 bytes of Bob's citizen key |
| `redemption_context` | SHA-256 of `"arc-stamp-v1"`, a zero byte, and the start of the key period as decimal Unix seconds |
| `origin_info` | empty |

Each stamp of Bob for one key period has the same challenge. The challenge
therefore does not tell two stamps apart.

Why these choices:

- The token is publicly verifiable. A relay verifies a stamp with Bob's
  public stamp key, and holds no secret of Bob. A relay cannot make
  stamps.
- The signature is blind. Bob signs a blinded message, and never sees the
  stamp that Alice makes from it. Bob's records of issuance therefore do
  not link a stamp on a relay to a contact.
- A relay verifies a stamp with `crypto/rsa.VerifyPSS` of the Go standard
  library. The relay needs no new dependency. Issuance and unblinding use
  `github.com/cloudflare/circl/blindsign/blindrsa`.

### 3.7 The stamp key

- Bob's stamp key MUST NOT be the citizen key. It is a new RSA key of 2048
  bits for each key period.
- A key period lasts 30 days. Its start is the time that the stamp key
  event (below) gives.
- The machine that holds the citizen key MUST make each stamp key, as it
  makes the data roots ([the devices spec](../devices/SPEC.md), section
  3.5). Thus two devices never make two keys for one period.
- That machine MUST keep each private stamp key as self data: a draft with
  the `d` tag `arc-stamp-keys`, sealed under the newest data root. It
  syncs to Bob's devices through his storage citizens, so any device that
  is online can issue stamps.
- The machine MUST make the key of the next period at least 7 days before
  the current period ends (estimated), so that a signer that is offline
  for a few days does not stop issuance.
- When a device is revoked, the machine MUST make a new stamp key at once,
  start a new period, and drop the current key from the event without a
  grace period. The revoked device holds the old private key and could
  make stamps with it. It gets no new data root, so it cannot open the new
  key ([the devices spec](../devices/SPEC.md), section 4.3).

Bob publishes his public stamp keys in a new replaceable event of kind
10276:

| Field | Value |
| --- | --- |
| `kind` | 10276 |
| `pubkey` | Bob's citizen key |
| `tags` | one to three tags `["key", <base64 DER SubjectPublicKeyInfo>, <start as decimal Unix seconds>]` |
| `content` | empty |

- The tags name the key of the previous period while its grace period
  lasts, the key of the current period, and the key of the next period
  when it exists.
- The grace period is 2 days after the end of a period (estimated). A
  stamp of the previous key then still redeems. It covers clocks that
  differ, and wraps that wait in an outbox.
- The citizen key signs the event, through the signer. The signer MUST
  refuse to sign kind 10276 for any device, as it refuses kind 10275
  ([the devices spec](../devices/SPEC.md), section 3.5). Else a device
  could publish a key that only it holds, and become a second issuer.
- The event is public, and has no NIP-70 tag. A relay reads it to verify stamps. The
  build MUST NOT add kind 10276 to the sealed kinds
  (`adapters/relay/sealed/sealed.go:64`).
- Bob MUST publish the event to each relay of his NIP-17 relay list (kind
  10050), and to his indexers, as he publishes his relay lists today.
- The event is replaceable because a relay needs only the newest set of
  keys. The event keeps the previous key while its grace period lasts.

Kind 10276 is free: [the kind registry](../KINDS.md) has no row for it,
the ARC kinds of section 3.7 of the delivery spec end at 10275, and the
kind table of the NIPs README names no such kind (checked on
2026-10-15). It
follows the ARC pattern of kinds that end in 272 to 276.

### 3.8 Issuance

Bob is the issuer. Alice is the client. Bob's contact list is the
attester. Issuance runs over private mail, in two new private rumor kinds.
Both kinds are free: [the kind registry](../KINDS.md) has no row for 3277
or 3278, and the kind table of the NIPs README names neither (checked on
2026-10-15).

**The stamp request, kind 3277.** Alice sends it to Bob.

- Tags: one `p` tag with Bob's key, and one reply stamp (section 3.12).
- Content: the JSON object
  `{"key": "<64 hex token_key_id>", "requests": ["<base64 TokenRequest>", ...]}`.
- Each `TokenRequest` is the structure of RFC 9578, section 6.1: the
  token type, the last byte of the key ID, and the blinded message of 256
  bytes. It holds 259 bytes, and 348 characters in base64 (arithmetic).
- A request MUST hold at most 64 entries. A rumor of kind 3277 then holds
  at most about 22,464 bytes of entries (arithmetic: 64 × 351, with quotes
  and a comma for each), below `MaxMailContent` of 24,576 bytes. The limit
  is 70 entries (arithmetic: 24,576 / 351).
- A request that pays with work, not with a stamp, MUST hold at most 8
  entries (estimated). Work grows with the size of the wrap (section 6.3).
- Alice keeps the nonce, the challenge digest and the blind inverse of
  each entry in her mail database until the reply comes, or the request
  expires.

**The stamp reply, kind 3278.** One of Bob's devices sends it to Alice.

- Tags: one `p` tag with Alice's key, and one `e` tag with the ID of the
  request rumor.
- Content: the JSON object
  `{"stamps": ["<base64 blind signature>", ...], "left": <n>}`. The blind
  signatures follow the order of the requests. `left` is the number of
  stamps that Bob still issues to Alice in this key period.
- The reply holds at most 64 signatures of 256 bytes: about 22,208 bytes
  (arithmetic: 64 × 347).

**Bob's device MUST:**

- Answer a request only when its author is a contact (section 3.1) at the
  time of the answer. A device sends nothing to a stranger, as for held
  mail (section 3.4). A stranger asks again after Bob accepts it.
- Answer only for a key that the newest kind 10276 of Bob names, and for
  which the device holds the private key.
- Issue at most 300 stamps to one contact in one key period (estimated).
  That is 10 wraps a day: 5 messages and 5 acknowledgements (arithmetic:
  300 / 30). Past the limit, the reply holds fewer signatures, or none.
- Count the stamps by the ID of the request rumor, and keep the count as
  self data with the stamp key. Two devices that answer the same request
  make the same signatures, because the RSA signature of a blinded message
  is deterministic. The count of that request then is the same on each.
  Between two syncs, two devices can each answer different requests. The
  worst case is the limit times the number of devices.
- Not acknowledge a request with kind 3274. The reply is the answer.

**Alice's device MUST:**

- Ask Bob for stamps only when Bob published a kind 10276, and Alice holds
  a rumor of kind 14 or an app kind from Bob (Alice is then a contact of
  Bob, section 3.11), or holds a stamp reply from Bob.
- Ask when she holds fewer than 16 unused stamps for Bob's current key
  (estimated), and ask for stamps of the next key when the event names
  one.
- Keep at most one open request to Bob for each key. A request expires
  with its wrap, after 7 days. Alice can then ask again.
- Check each stamp after it unblinds it, with Bob's public key from the
  event. A signature that fails is dropped. RFC 9474 finalization makes
  this check.
- Keep each unused stamp in the mail database, by Bob's key and the key
  ID, and drop it when its period and grace period end.
- Ignore a stamp reply that matches no open request.

### 3.9 Redemption

A stamp rides on the relay form of a gift wrap of kind 1059, as one tag:

```text
["stamp", "<base64 of the 354 bytes of the stamp>"]
```

- Base64 is the standard alphabet of RFC 4648, section 4. 354 bytes give
  472 characters, with no padding (arithmetic: 354 / 3 × 4).
- One wrap MUST carry at most one `stamp` tag.
- The same wrap goes to each inbox relay of Bob, with the same stamp. Each
  relay redeems it once.
- A stamp cannot be bound to its wrap. Alice picks the nonce before Bob
  signs, and she makes the wrap later. A stamp that a reader copies from
  one relay therefore works once at each other relay (section 6.1).

An inbox relay checks a stamp in this order. Each cheap check comes before
the RSA check.

1. The wrap is the relay form: exactly one `p` tag. The `p` tag names
   Bob.
2. The tag decodes to exactly 354 bytes, and `token_type` is `0x0002`.
3. The newest kind 10276 of Bob on this relay names a key whose
   `token_key_id` equals the one of the stamp. The time of the relay is
   after the start of that key, and before its end plus the grace period.
4. `challenge_digest` equals the SHA-256 of the challenge for Bob and the
   start of that key (section 3.6).
5. `crypto/rsa.VerifyPSS` with SHA-384 and a salt of 48 bytes accepts the
   `authenticator` over the SHA-384 of the first 98 bytes of the stamp.
6. The nonce is not in the spent set of that key, or it is there with the
   ID of this same wrap.

If each check passes, the relay MUST add the nonce and the wrap ID to the
spent set of that key, and take the wrap. The same wrap that comes again
is a duplicate, not a second use of the stamp. The outbox sends the same
wrap again on each sync (delivery spec, section 4.2), so this rule is
necessary.

The relay keeps one spent set for each stamp key of each citizen. It
MUST keep the set until the end of the key period plus the grace period,
and then drop it. A stamp of that key no longer passes check 3 then.

### 3.10 The relay rule

An ARC relay has a mode for gift wraps of kind 1059: `off`, `advisory` or
`enforce`. The mode moves in that order, as senders update.

| Mode | What the relay does |
| --- | --- |
| `off` | It takes wraps as today. This is the default. |
| `advisory` | It runs the rule below, takes each wrap, and counts the wraps that `enforce` would refuse. It lists the tag `arc-stamps` in its NIP-11 document. |
| `enforce` | It runs the rule below, and refuses. It lists the tags `arc-stamps` and `arc-stamps-enforce` in its NIP-11 document. |

The rule, for a wrap of kind 1059:

1. A relay-form wrap whose `p` tag names a citizen with a kind 10276 on
   this relay:
   1. A valid, unused stamp (section 3.9): the relay takes the wrap.
   2. Else, a wrap ID with at least 20 bits of work: the relay takes the
      wrap.
   3. Else, in `enforce`, the relay refuses it with
      `pow: this citizen takes mail with a stamp, or with 20 bits of work`.
2. A courier-form wrap (a `w` tag, no `p` tag): the relay cannot see the
   recipient, so a stamp cannot apply. In `enforce`, the relay takes it
   only with 20 bits of work.
3. A relay-form wrap to a key with no kind 10276 on this relay: the
   recipient did not ask for the rule. The relay takes it as today.

Authentication MUST NOT admit a wrap in cases 1 and 2. This closes the
bypass of section 0: a random key that answers the NIP-42 challenge no
longer replaces the work. `--wrap-auth` and `--wrap-pow` keep their meaning
only for kind 21059 and for case 3.

The `pow:` prefix is a machine-readable prefix of NIP-01. The relay
transport tries again with authentication only after `auth-required:`
(`adapters/transport/relay/relay.go:405`), so an older ARC sender reports
the refusal and does not loop.

The recipient's device still runs the filter of section 3.3 in each mode.
Only the device sees the author.

Revocation of a contact: Bob removes the key from his contact list. His
devices then issue no more stamps to it. Its stamps work until their key
period and grace period end. The device filter holds its mail at once.

### 3.11 The sender

A newer sender, for each recipient of a rumor of kind 14 or of an app
kind:

1. If it holds an unused stamp of the recipient for a key that is valid
   now, it MUST put the stamp on the relay form of the wrap, and make no
   work.
2. Else, it MUST make work when it is a stranger to the recipient: it holds
   no rumor from the recipient of a kind that adds a contact (kind 14, or
   the private kind of an app). Such a rumor shows that the recipient
   wrote to the sender, so the sender is a contact of the recipient
   (section 3.1).
3. Else, it MUST make work when every inbox relay of the recipient lists
   `arc-stamps-enforce`. If one inbox relay does not enforce, the wrap
   reaches the recipient through it.
4. Else, it sends the wrap with neither.

More rules for the sender:

- An acknowledgement from the recipient does not make the sender a
  contact. A recipient acknowledges mail with work from a stranger, and
  the stranger stays a stranger.
- The sender makes the work, or picks the stamp, before it gives the wrap
  to a transport. It keeps the wrap with its tag in the outbox, so that
  each later sync sends the same wrap.
- The sender MUST NOT post its courier-form copy to a relay that lists
  `arc-stamps-enforce`. The relay form still goes there. The sender posts its courier form to its own relays today
  (`core/mail/mail.go:355`), and the relay form also to the inbox relays
  of the recipient (`core/mail/mail.go:357`).
- An acknowledgement (3274), a reply (3273) and a stamp reply (3278) pay
  the same way: a stamp of the recipient, or work. The reply stamp of
  section 3.12 usually gives the stamp.

### 3.12 Reply stamps

Bob's acknowledgement to Alice goes to Alice's inbox relays. If those
relays enforce the rule, Bob needs a stamp of Alice, or work. A provider
that answers a call has the same need. A provider is not a contact of its
callers, so it gets no stamps through issuance.

A citizen holds its own private stamp key, so it can make a stamp of its
own key with no blinding: it signs the token input directly. This is a
reply stamp.

- A rumor of kind 3272 or 3277 MUST carry one reply stamp of its author,
  when the author published a kind 10276.
- A rumor of kind 14 or of an app kind MUST carry one reply stamp for each
  recipient from whom the author holds no stamp request in the current key
  period. That recipient has no stamps of the author yet. Later
  acknowledgements use stamps that the recipient asked for.
- The tag inside the rumor is `["stamp", <64 hex of the recipient>, <base64 stamp>]`.
  It holds 552 bytes (arithmetic: 10 + 66 + 1 + 474 + 1, with quotes,
  commas and brackets).
- If the reply stamps would make the rumor pass `MaxRumor` of 28,672
  bytes, the author leaves them out. The recipient then answers with work.
- The recipient MUST use a reply stamp only on a wrap to the author of the
  rumor. It uses it first for the answer to that rumor.
- A relay cannot tell a reply stamp from an issued stamp. The author knows
  which reply stamp it gave to whom, but the author sees the author of each
  wrap anyway.

### 3.13 Group messages

[The group messages spec](../groups/SPEC.md) seals one rumor with several
`p` tags to each member.

- The author of the seal decides the filter. The `p` tags MUST NOT make a
  key a contact, because any author can write any `p` tag.
- A member who sends to a group seals to each other member, so each
  member becomes a contact of that sender (section 3.1). Groups need no
  other rule for the filter.
- Each member has its own inbox relays and its own stamp key. The relay
  form of the wrap to each member MUST carry a stamp of that member, or
  work. A group message to 9 members thus needs 9 stamps, one of each
  member.
- A new member who writes to a group makes work for each member whose
  stamps it does not hold (section 3.11). A group has at most 10
  participants, so that is at most 9 wraps.
- The rumor of a group message is the same for each member. A reply stamp
  for a member is thus readable by each other member. A member can spend it
  first on a relay of the author. The loss is one stamp of the author.

### 3.14 Sizes

| Item | Bytes | Tag |
| --- | --- | --- |
| A stamp | 354 | arithmetic: 2 + 32 + 32 + 32 + 256 |
| A stamp in base64 | 472 | arithmetic: 354 / 3 × 4 |
| The tag `,["stamp","…"]` on a wrap | 485 | arithmetic: 11 + 472 + 2 |
| The tag `,["nonce","18446744073709551615","20"]` on a wrap | 38 | arithmetic: the largest nonce has 20 digits |
| A reply stamp tag inside a rumor | 552 | arithmetic: section 3.12 |
| A `TokenRequest` in base64 | 348 | arithmetic: 4 × ⌈259 / 3⌉ |
| A blind signature in base64 | 344 | arithmetic: 4 × ⌈256 / 3⌉ |
| A public stamp key, DER | 342 | RFC 9578, section 6.5, example |

`MaxWrap` grows by the larger of the two tags, to 55,634 bytes
(arithmetic: 55,149 + 485). A wrap carries one tag or the other, never
both (section 3.5). This changes section 3.4 of the delivery spec. Without
the change, a relay refuses the largest mail with a stamp. The content of
the wrap does not change, so it stays below 65,535 bytes, which the Bolt
event store holds.

## 4. Commands

These names are proposed. The build can change them.

| Command | Does |
| --- | --- |
| `arc contacts` | Lists the contact list. |
| `arc contacts add <citizen>` | Adds a key, and moves its held mail to the inbox. |
| `arc contacts rm <citizen>` | Removes a key. Mail that is in the inbox stays there. No new stamps go to the key. |
| `arc message held` | Lists held mail: author, time and text. |
| `arc stamps` | Lists, for each recipient, the unused stamps that this citizen holds. Lists, for each contact, the stamps issued in this key period. Names the current and the next stamp key. |
| `arc relay serve --stamps off\|advisory\|enforce` | Sets the mode of section 3.10. The default is `off`. |

The filter is on by default. `arc message` prints how many held messages
wait, on standard error, so the change is visible. A newer citizen
publishes a stamp key by default, at its next `arc serve` or `arc sync`.

## 5. Failures

| Failure | What happens |
| --- | --- |
| A stranger sends mail without work | An inbox relay in `enforce` refuses it with `pow:`. A relay in another mode takes it, and the device holds it. The sender gets no acknowledgement. Its outbox shows the message as expired after 7 days, unless the citizen accepts the stranger first. |
| The work of a wrap is less than its `nonce` tag claims | The relay and the device count the work of the ID. The relay refuses the wrap in `enforce`. The device holds it. |
| A stamp was spent on this relay by another wrap | The relay applies case 1.2 of section 3.10: work, or a refusal in `enforce`. The same wrap still reaches the other inbox relays. |
| A stamp of a key that the relay does not know | The relay has an older kind 10276 of Bob. Check 3 fails. Bob's device publishes the event again at its next sync. |
| A stamp of a key whose period and grace period ended | Check 3 fails. A newer sender drops such stamps before it uses them. |
| Bob does not answer a stamp request | The request expires after 7 days. Alice sends with work when each inbox relay of Bob enforces, and asks again. |
| Bob's limit for Alice is reached | The reply holds fewer stamps, and `left` is 0. Alice sends with work until the next period. |
| Bob's signer is offline when a period ends | The next key was published 7 days before. If the signer stays offline past that, no key is valid, and contacts send with work. |
| No device of Bob holds the stamp key | No device answers a stamp request. Contacts send with work. |
| More than 200 held seals | The device drops the oldest held seal. |
| Two devices of a citizen have different contact lists | One device can hold mail that the other shows. When the lists sync, the first device moves the mail to the inbox. |
| An older relay gets a wrap with a stamp or work above 55,149 bytes | The relay refuses it with `invalid:`. The sender sees the refusal for that relay. |
| An older sender writes to a citizen whose inbox relays enforce | Each relay refuses the wrap with `pow:`. The older sender reports the refusal for each relay, and its outbox shows the message as expired after 7 days. |

## 6. Security and cost

### 6.1 What each party can do

| Party | Can | Cannot |
| --- | --- | --- |
| stranger | Send mail with work, and reach the inbox. Fill the held bucket with mail without work on relays that do not enforce. | Reach the inbox without work. Reach an enforcing inbox relay without a stamp or work. Learn whether its mail was held or lost. |
| contact | Send with stamps, up to the limit of section 3.8. Give stamps to someone else. | Get more stamps than the limit from one device of Bob. Make a stamp without Bob. |
| reader of a relay | Copy a stamp from a wrap, and spend it once on each other inbox relay of the recipient before the real wrap arrives. | Spend it twice on one relay. Make a stamp. |
| relay | Drop mail, as today. Learn that a wrap has a stamp or work. | See the author. Link two stamps to one contact. Make a stamp: it holds only public keys. |
| relay with Bob | Learn the author of each wrap, as today: Bob opens the seal. | Learn more through the stamps. |
| revoked device of Bob | Make stamps with the key that it held, until the signer makes a new key. | Open the new key: it gets no new data root. |

A stranger can push real held mail out of the bucket with 200 cheap
messages, on a relay that does not enforce. That mail then waits for the
sender to try again. The rule of section 3.10 moves this cost to the
stranger on each enforcing relay.

A copied stamp is a small loss. The real wrap carries the same stamp to
each inbox relay. If a reader spends it first on one relay, the real wrap
is refused there, and arrives through the others. Bob reads all his inbox
relays.

The device still unwraps each wrap that a relay takes, because only the
seal names the author. That is two NIP-44 decryptions in the signer for
each wrap, as today. The filter removes the signature of the
acknowledgement for each held message.

### 6.2 Cost of a stamp

Measured on an Apple M3 Max with Go 1.27.1 and the standard library
`crypto/rsa`, on 2026-10-15. Three runs of `go test -bench . -benchtime 3s`
for each operation. The PR that adds this proposal holds the benchmark.

| Operation | Who | Time | Tag |
| --- | --- | --- | --- |
| Make an RSA key of 2048 bits | Bob's signer, each 30 days | 43 to 57 ms | measured |
| One private-key operation (SignPSS, SHA-384, salt 48) | Bob's device, for each stamp | 0.95 to 1.07 ms | measured |
| VerifyPSS of one stamp | the relay, for each wrap | 32 to 36 µs | measured |
| Issue 64 stamps | Bob's device | about 64 ms | arithmetic: 64 × 1.0 ms |
| Verify stamps on one core | the relay | about 30,000 a second | arithmetic: 1 s / 33 µs |

SignPSS stands in for the blind signature of RFC 9474. Each is one RSA
private-key operation. The blind signature has no hash and no salt to
make, so it is not slower (estimated). No run used `circl`.

Each stamp request and each reply is one private message, so it costs one
signature of the seal through Bob's signer, as an acknowledgement does
today.

The spent set of a relay holds 64 bytes for each redeemed stamp: the
nonce and the wrap ID. A citizen with 50 contacts that each use the full
limit fills 960,000 bytes on each inbox relay in one period (arithmetic:
50 × 300 × 64). Most contacts use far fewer stamps (estimated). The relay
drops the set when the period and grace period end.

A sender can send stamps that fail, to load the relay. The cheap checks of
section 3.9 come first, so only a stamp with a real key ID and challenge
reaches the RSA check. The rate limit of each IP address (`--rate`) still
applies.

### 6.3 Cost of the work

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
- A stamp makes the work unnecessary for a contact. Without stamps, an
  enforcing relay would make each contact pay this cost for each message.

## 7. Compatibility

- An older recipient opens all mail, as today. It ignores the `nonce` and
  `stamp` tags. It publishes no kind 10276, so no relay applies case 1 of
  section 3.10 to its mail, and no newer sender asks it for stamps.
- An older sender makes no work and no stamps.
  - Its mail to a newer citizen that does not have it as a contact is
    held, and shows as expired in its outbox after 7 days. Before, the
    same mail showed as delivered.
  - On an inbox relay in `enforce`, each of its wraps to a citizen with a
    kind 10276 is refused with `pow:`, contact or not. Its transport tries
    again only after `auth-required:`, so it reports the refusal.
  - These are the visible changes for older senders. A relay operator
    therefore stays in `advisory` until the counts of section 3.10 show
    few such wraps.
- NIP-17 clients outside ARC make no work and no stamps for gift wraps.
  Their first mail to an ARC citizen is held. An enforcing relay refuses
  it.
- An older device of a newer citizen shows each message, held or not,
  because it has no filter. It ignores the draft `arc-contacts` and the
  draft `arc-stamp-keys`. It opens a stamp request as the private kind of
  an app: it saves the seal and acknowledges it (`core/mail/mail.go:606`),
  and does not show it, because the inbox lists only kind 14
  (`core/mail/mail.go:1059`). It sends no stamp reply.
- An older relay takes a wrap with a stamp or work up to 55,149 bytes. It
  refuses a larger one (section 5). It ignores the `stamp` tag, and keeps
  the bypass of section 0.
- `--wrap-auth` and `--wrap-pow` keep their meaning for kind 21059, and
  for wraps to citizens without a kind 10276.

The build changes normative sections of
[the delivery spec](../delivery/SPEC.md), which is `partial`:

- Section 3.4: `MaxWrap` becomes 55,634 bytes, and a wrap carries at most
  one `nonce` or `stamp` tag.
- Section 3.7: kinds 3277, 3278 and 10276.
- Section 4.2: held mail gets no acknowledgement. A stamp request gets no
  acknowledgement.
- Section 6.2: contacts, work, held mail, stamps, and the relay rule. The
  sentence on NIP-42 authentication or proof of work is replaced.
- Sections 4.4 and 6.2: the contacts of a courier are the contacts of
  section 3.1.

It also changes [the devices spec](../devices/SPEC.md), which is `built`:
the signer makes and rotates the stamp key, signs kind 10276, refuses to
sign kind 10276 for a device, and makes a new stamp key when it revokes a
device.

It ships as a breaking commit.

The build adds rows for kinds 3277, 3278 and 10276 to
[the kind registry](../KINDS.md), with the code that names them. This
proposal adds no row. `TestTheKindRegistryMatchesTheCode`
(`internal/specs/specs_test.go:391`) refuses a row that no code, manifest
or reserved list names.

## 8. Proof that a build needs

The filter:

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
- A call request from a stranger reaches the provider without work.
- A group message from a stranger, with a contact in its `p` tags and no
  work, is held.
- The 201st held seal pushes out the oldest.
- A contact added on one device releases held mail on another device
  after a sync through a storage citizen.

The stamps:

- The test vector of RFC 9474, appendix A.3 (RSABSSA-SHA384-PSS-Deterministic),
  passes through `circl` for issuance and through `crypto/rsa.VerifyPSS`
  for the check.
- Alice, a contact of Bob, asks for 64 stamps. One device of Bob answers.
  Alice's next message carries a stamp, and Bob's enforcing inbox relay
  takes it with no work.
- The same stamp on a second wrap to the same relay is refused. The same
  wrap sent again is a duplicate, not a refusal. The same wrap reaches a
  second inbox relay of Bob.
- A stamp of Bob on a wrap to Carol is refused: the challenge names Bob.
- A stamp of the previous key redeems in the grace period, and is refused
  after it.
- A device of Bob is revoked. The signer publishes a new stamp key at
  once. A stamp of the old key is refused. The revoked device cannot open
  the new key. The signer refuses to sign a kind 10276 that a device asks
  for.
- A request from a stranger gets no reply. A request past the limit of
  300 gets fewer stamps, and `left` is 0.
- Two devices of Bob answer the same request. Alice gets the same stamps
  twice, and keeps them once.

The relay rule:

- An enforcing relay refuses a wrap with no stamp and no work with
  `pow:`, also on a connection where a random key authenticated. This
  proves that the bypass of section 0 is closed.
- An enforcing relay takes a wrap with 20 bits of work and no stamp.
- An enforcing relay refuses a courier-form wrap without work.
- An enforcing relay takes a wrap to a key without a kind 10276, as
  today.
- A relay in `advisory` takes each wrap and counts the ones it would
  refuse.
- A program of an older arc, built from its tag, sends to a citizen on an
  enforcing relay, and reports the refusal.

The sender and the sizes:

- A newer sender uses a stamp when it holds one, and work only when it
  holds none and is a stranger, or each inbox relay enforces.
- Bob acknowledges a first message of a stranger with work. The
  acknowledgement reaches Alice's enforcing relay with the reply stamp of
  that message.
- A provider's reply reaches the caller's enforcing relay with the reply
  stamp of the call request.
- The largest mail with a stamp, and the largest mail with work, each fit
  the new `MaxWrap` of 55,634 bytes.

## 9. Decisions

Zim agreed these decisions. The build follows them.

1. **The name is "stamp".** ARC already uses "token" for the token of a
   device (kind 20274).
2. **The cryptography is RFC 9578, token type 2.** Blind RSA of 2048 bits
   (RFC 9474, RSABSSA-SHA384-PSS-Deterministic). It is publicly
   verifiable, so a relay holds no secret. Relays verify with
   `crypto/rsa.VerifyPSS` of the Go standard library. Issuance and
   unblinding use `github.com/cloudflare/circl/blindsign/blindrsa`.
   These options were rejected:
   - `github.com/cloudflare/pat-go`: its README says that it is not for
     production.
   - VOPRF, RFC 9578 token type 1: only the holder of the issuer secret can
     verify a token. Each relay would then hold Bob's secret, and could
     make stamps.
   - Blind Schnorr signatures on secp256k1, the curve of Nostr keys: the
     ROS attack forges signatures when an issuer runs many sessions at
     once. Issuance over mail runs many sessions at once.
   - The IETF drafts for anonymous rate-limited credentials (ARC): the
     work was discontinued at IETF 126. The name also clashes with ARC.
   - A secret stamp key that Bob shares with his contacts: a leak from one
     contact lets anyone make stamps, and exposes the gate of each
     contact.
3. **The roles.** The recipient (Bob) is the issuer, with a stamp key that
   is not the citizen key. The sender (Alice) is the client. Bob's inbox
   relays verify and redeem. Bob's contact list is the attester: Bob
   issues stamps only to contacts.
4. **The stamp key lives on each of Bob's devices** as sealed self data,
   as the data roots do. Any device that is online can issue.
5. **Issuance runs over private mail,** in two new private rumor kinds:
   the stamp request (3277) and the stamp reply (3278). It is
   asynchronous. A contact's request passes the stranger filter. A
   stranger's request needs proof of work to reach an enforcing relay, and
   gets no stamps. Issuance is in batches, with a limit for each contact:
   300 in a key period (estimated).
6. **Bob publishes the public stamp key in a new replaceable event,** kind
   10276. The key rotates every 30 days, with a grace period of 2 days
   (estimated) for stamps of the previous key.
7. **Redemption.** A stamp is a tag on the relay form of a gift wrap. The
   challenge binds it to Bob and the key period. Each relay keeps the
   spent nonces of each key period, and drops them when the period ends.
   The same wrap sent to several of Bob's relays redeems at each. A stolen
   stamp works at most once on each relay.
8. **The relay rule.** A valid unused stamp is accepted. Else 20 bits of
   NIP-13 work is accepted. Else the wrap is refused, but only in
   `enforce`. Relays move from `off` to `advisory` to `enforce`. The
   recipient's device still runs the stranger filter: only it sees the
   author. Revocation: Bob stops issuing to a contact, and its stamps end
   with the key period.
9. **Out of scope:** courier-form wraps (a stamp would reveal the hidden
   recipient; they rely on work and the device filter), live calls (kind
   21059), and payments.
10. **Size.** A stamp holds 354 bytes, and 472 in base64. `MaxWrap` grows
    for stamps as for the `nonce` tag. That changes section 3.4 of the
    delivery spec.

These rules are not in the list above. This proposal adds them to close
gaps that the decisions leave open:

- The machine that holds the citizen key makes each stamp key, as it makes
  the data roots (section 3.7). Two devices therefore never make two keys
  for one period.
- A revoked device forces a new stamp key at once (section 3.7).
- The relay rule applies only to recipients that published a kind 10276 on
  that relay, and to the courier form (section 3.10). Wraps that a sender
  posts to its own relays for other recipients go as today.
- Reply stamps (section 3.12) let a recipient acknowledge a first message,
  and let a provider reply to a caller, without work.

## 10. Not in this proposal

- A difficulty that the recipient publishes. A fixed constant needs no
  lookup before a send. A published value can come later, in the kind
  10276 event.
- A per-byte difficulty, so that a large first message costs less.
- The relay rule for live wraps of kind 21059. The bypass of section 0
  stays open for them.
- Stamps on the courier form, and on a mesh link.
- Payments for stamps, or stamps that a storage citizen sells.
- A read rule for gift wraps on an ARC relay, which would stop a reader
  from copying stamps.
