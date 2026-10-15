# Mail from strangers

- Status: partial
- Layers: core, runtime
- Owns: none
- Proof: go test -count=1 -run '^(TestTheWrapLimitsAgreeWithTheMeasurement|TestTheLargestMailFitsTheWrapLimit|TestAWrapWithWorkHasTheWorkInItsID|TestAMessageFromAStrangerWithoutWorkIsHeld|TestAMessageFromAStrangerWithWorkReachesTheInbox|TestAWrapThatClaimsWorkItDoesNotHaveIsHeld|TestAcceptingAStrangerReleasesItsHeldMail|TestASyncReleasesTheHeldMailOfANewContact|TestTheReplyOfARecipientNeedsNoWork|TestACallFromAStrangerIsNotHeld|TestAGroupMessageFromAStrangerIsHeld|TestTheHeldMailHasALimit|TestHeldMailExpiresWithItsWrap|TestAMessageFromAStrangerCrossesAStick|TestAHeldSealMovesWhenAWrapWithWorkArrives|TestAnOlderMessageFromTheRecipientCountsOnce|TestAnOldOutboxSeedsTheContacts|TestContactsReleaseHeldMail|TestASendAddsAContact)$' ./core/private/ ./core/mail/ ./cmd/arc/
- Remaining: the stamps of sections 3.8 to 3.13, and the steps of section 3.6 that use them
- Unverified: No test syncs the contact list between two devices through a storage citizen, or adds a contact on two devices at once. No test runs the program of an older arc as a sender or as a recipient. No run measures proof of work on a phone. `TestTheHeldMailHasALimit` fills a cap of 3, not of 200.

A citizen takes mail from its contacts. Mail from a stranger reaches the
inbox only with proof of work. Without it, the device holds the mail apart,
and sends no acknowledgement. Sections 3.8 to 3.13 add stamps, so that an
inbox relay can take the mail of contacts and refuse the rest. Stamps are
not built.

Each number has a tag:

- *measured*: a run on a real machine. The source is given.
- *arithmetic*: a calculation from other numbers. The inputs are given.
- *estimated*: a judgement. No run supports it.

## 1. Purpose

Before this spec, anyone could send a gift wrap to a citizen. The mail
layer opened each wrap that named the citizen, kept its seal, showed it in
the inbox, and acknowledged it. It had no notion of a contact.

A one-time key signs each gift wrap
([the delivery spec](../delivery/SPEC.md), section 3.4). A relay therefore
cannot limit mail by its author. The author is inside the seal, and only
the recipient opens the seal. Thus:

- A sender with a list of keys could put text in front of each citizen, at
  no cost to the sender.
- Each message made the recipient sign and send an acknowledgement. Each
  signature can be a round trip to a remote signer
  ([the devices spec](../devices/SPEC.md)). One spam message cost the
  recipient more work than it cost the sender.
- Each spam wrap takes space on the inbox relays, and the recipient's
  device downloads it and decrypts it twice.

This spec defines:

- the contacts of a citizen, and the contact list,
- the filter on the recipient's device, and held mail,
- proof of work on a wrap, and when a sender makes it,
- not built: stamps, which a citizen gives to its contacts, and the rule
  of an inbox relay that takes a wrap with a stamp or with work.

The filter stops the first two problems. Only a rule on the relay stops the
third. The relay cannot see the author, so it needs a proof that does not
name the author: a stamp, or work.

This spec does not cover live calls (kind 21059), stamps on the courier
form, or payments. Public posts are not the problem: a citizen pulls them,
and an install needs consent.

## 2. Terms

| Term | Meaning |
| --- | --- |
| contact | A key from which a citizen takes mail without proof of work. Section 3.1 defines the set. |
| stranger | Each key that is not a contact. |
| held mail | Mail from a stranger without enough proof of work. The device keeps it apart from the inbox. |
| work | The NIP-13 difficulty of the ID of one gift wrap, in bits. |
| stamp | Not built. A token of RFC 9578, token type 2, that a citizen issues. One stamp lets one gift wrap into each inbox relay of that citizen, once. ARC already uses "token" for the token of a device (kind 20274). |
| stamp key | Not built. An RSA key pair of 2048 bits that issues the stamps of one citizen for one key period. It is not the citizen key. |
| key period | Not built. The 30 days in which one stamp key issues stamps. |
| issuer | Not built. The citizen who makes a stamp: the recipient of the mail that the stamp pays for. |
| reply stamp | Not built. A stamp that a citizen makes of its own key and puts in a rumor, so that the recipient can answer without work. |

**Rumor**, **seal**, **gift wrap**, **relay form** and **courier form** are
the terms of [the delivery spec](../delivery/SPEC.md), sections 3.4 and
3.5. In the examples, Bob is the recipient and Alice is the sender.

## 3. Rules

### 3.1 Contacts

The contacts of a citizen are the union of:

1. The key of the citizen, and each key on its device list.
2. Each key on the contact list of the citizen.

The contact list:

- It is self data ([the delivery spec](../delivery/SPEC.md), section
  4.11). It syncs through the storage citizens of the citizen. It never
  goes to a relay, and `arc sync --dir` does not write it to a directory:
  a stick can reach a courier, and a draft names its citizen in a `p` tag.
  It needs no new kind.
- Each contact MUST be one draft. The store keeps one version of each `d`
  tag, the newest. One draft for the whole list would lose a key that two
  devices add at the same time.
- The event inside a draft has kind 30078 and the content
  `{"key": "<64 hex>", "contact": true}`. `false` removes the key. Of the
  drafts of one key, the newest event counts.
- The `d` tag is `arc-ct-` and a keyed value of the key: base64url of the
  first 16 bytes of HMAC-SHA256 with the content key of the app
  `arc-contacts` under the data root, of `arc-contact-v1`, a zero byte and
  the hex key. It holds 29 characters, the limit of a `d` tag
  ([the interface spec](../interface/SPEC.md), section 3.7.1). A draft that
  reaches a relay thus does not name the contact. A new generation of the
  data root gives a new `d` tag, so a key can have drafts under two tags.
- When the citizen sends a rumor of kind 14, or of the private kind of an
  app, the device MUST add each recipient to the list before it seals the
  rumor. A key that is on the list already gets no new draft.
- A call request (3272), a reply (3273) and an acknowledgement (3274) MUST
  NOT add a key. A device sends these without a person. A call to a
  provider is not consent to its mail.
- The citizen can add and remove a key by hand.
- The first sync with a contact list MUST put each recipient of a message
  in the outbox on the list, once, and record that it did. Before this
  spec, a send added no key. Without the seed, each correspondent would be
  a stranger after an update. A key that the citizen removes later stays
  off.

ARC does not use the NIP-02 follow list (kind 3). It is public, so it
would show each relay who the citizen talks to.

A citizen without storage citizens keeps the list on each device. The
devices then can disagree.

### 3.2 Where the filter runs

- The recipient's device MUST run the filter in `core/mail`, after it
  unwraps the gift wrap, and before it saves the seal. Only the seal names
  the author.
- `core/mail` gets the contact list as the injected interface
  `mail.ContactList`. Core reads no file. `runtime/citizen` keeps the list.
  Without a contact list, `core/mail` takes all mail and makes no work.
- The relay cannot run the filter. It cannot see the author.
- The sender's device does not learn the result of the filter. It sees an
  acknowledgement, or none.

### 3.3 What the filter decides

The filter applies to a rumor of kind 14, and to the private kind of an
app.

| Author | Work of the wrap | Result |
| --- | --- | --- |
| contact | any | The seal goes to the store and the inbox. The device acknowledges it. |
| stranger | at least 20 bits | The seal goes to the store and the inbox. The device acknowledges it. |
| stranger | less than 20 bits | The mail is held (section 3.4). The device sends no acknowledgement and no refusal. |

- A wrap of a seal that the store holds already is a duplicate. The filter
  does not hold it. The courier form of a message that the relay form
  admitted is such a wrap.
- Work on a wrap counts once. It does not make the author a contact. Each
  message from a stranger needs its own work.

The filter does not apply to:

- A call request (3272). The gate of the app decides who calls it, from
  the `allow` list of its Arcfile. A provider serves strangers by design.
- A reply (3273) or an acknowledgement (3274). They count only when they
  match an outbox entry to their author. Other ones change nothing.
- Not built: a stamp request (3277) or a stamp reply (3278). Section 3.10
  gives their rules.
- A live call (kind 21059). It does not go through the mail.

### 3.4 Held mail

- The device MUST keep held mail in the bucket `held` of the mail
  database, apart from the store. The inbox reads each seal in the store,
  so a held seal must not be there. The bucket keeps the seal, which is
  ciphertext, its author, the ID of its rumor, and the expiration of its
  wrap. The word "requests" names the bucket of received call requests.
- The bucket keeps at most 200 seals (estimated). Past that, the device
  drops the oldest. A held seal also goes when its wrap expires: 7 days by
  default.
- Held mail makes no notification, and the inbox does not show it.
- When a key joins the contacts, on any device, the device MUST move the
  held mail of that key to the store, and acknowledge each message. Each
  sync does this, so a contact that another device added releases the
  mail after the list syncs. If the wrap has not expired, the sender's
  outbox then shows the message as delivered.
- If the device gets another wrap of a held seal, and that wrap has enough
  work, the device MUST move the seal to the store and acknowledge it.

Why held mail gets no answer:

- Each answer is a wrap that the recipient's signer must sign. An answer
  to each spam message gives a spammer a way to load the signer.
- An answer tells the stranger that the key is in use.

The sender is not left without a result. Its outbox shows the message as
expired after 7 days ([the delivery spec](../delivery/SPEC.md), section
4.2).

### 3.5 Proof of work

- Work follows NIP-13: a `nonce` tag on the gift wrap, and leading zero
  bits in its ID.
- The difficulty is 20 bits. It is a protocol constant, not a setting, so
  that each sender knows it. `core/private` holds it as `Work`.
- The recipient MUST count the work that the ID has, not the target in the
  `nonce` tag.
- A sender that makes work makes it on both forms of the wrap. A stick
  carries only the courier form, so a first message by hand needs the work
  there too. The two forms are two events with two IDs, so the work costs
  twice.
- A wrap MUST NOT carry both a `nonce` tag and a `stamp` tag.
- `MaxWrap` is 55,634 bytes (arithmetic: 55,149 + 485). It leaves room for
  one tag that admits the wrap, the larger of the two: a `nonce` tag of 38
  bytes, or a `stamp` tag of 485 bytes (section 3.14). This is the limit of
  section 3.4 of the delivery spec.

### 3.6 The sender

For each recipient of a rumor of kind 14 or of an app kind, a sender:

1. Not built: if it holds an unused stamp of the recipient for a key that
   is valid now, it MUST put the stamp on the relay form of the wrap, and
   make no work.
2. Else, it MUST make work when it is a stranger to the recipient: it holds
   no rumor from the recipient of a kind that adds a contact (kind 14, or
   the private kind of an app). Such a rumor shows that the recipient wrote
   to the sender, so the sender is a contact of the recipient (section
   3.1).
3. Not built: else, it MUST make work when every inbox relay of the
   recipient lists `arc-stamps-enforce`.
4. Else, it sends the wrap with neither.

More rules for the sender:

- The device records each key whose rumor of kind 14 or of an app kind it
  opens, held or not, in the bucket `wrote` of the mail database. Mail
  from before this spec has no record. On the first send to a key without
  a record, the device opens the seals of that key in its store, once, and
  records the result. On a device each seal is a request to the signer.
- An acknowledgement from the recipient does not make the sender a
  contact. A recipient acknowledges mail with work from a stranger, and
  the stranger stays a stranger.
- The sender makes the work before it gives the wrap to a transport. It
  keeps the wrap with its nonce in the outbox, so that each later sync
  sends the same wrap.
- Not built: the sender MUST NOT post its courier-form copy to a relay that
  lists `arc-stamps-enforce`.
- Not built: an acknowledgement (3274), a reply (3273) and a stamp reply
  (3278) pay with a stamp of the recipient, or with work.

### 3.7 Group messages

[The group messages spec](../groups/SPEC.md) seals one rumor with several
`p` tags to each member.

- The author of the seal decides the filter. The `p` tags MUST NOT make a
  key a contact, because any author can write any `p` tag.
- A member who sends to a group seals to each other member, so each member
  becomes a contact of that sender (section 3.1).
- A new member who writes to a group makes work for each member that has
  not written to it (section 3.6). A group has at most 10 participants,
  so that is at most 9 wraps.
- Not built: the relay form of the wrap to each member carries a stamp of
  that member, or work.
- Not built: the rumor of a group message is the same for each member. A
  reply stamp for a member is thus readable by each other member, who can
  spend it first on a relay of the author. The loss is one stamp.

### 3.8 Stamps

Not built.

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

Each stamp of Bob for one key period has the same challenge, so the
challenge does not tell two stamps apart.

Why these choices:

- The token is publicly verifiable. A relay verifies a stamp with Bob's
  public stamp key, holds no secret of Bob, and cannot make stamps.
- The signature is blind. Bob signs a blinded message, and never sees the
  stamp that Alice makes from it. Bob's records of issuance do not link a
  stamp on a relay to a contact.
- A relay verifies a stamp with `crypto/rsa.VerifyPSS` of the Go standard
  library. Issuance and unblinding use
  `github.com/cloudflare/circl/blindsign/blindrsa`.
- `github.com/cloudflare/pat-go` says that it is not for production.
- VOPRF, RFC 9578 token type 1, is rejected: only the holder of the issuer
  secret can verify a token, so each relay would hold Bob's secret, and
  could make stamps.
- Blind Schnorr signatures on secp256k1 are rejected: the ROS attack
  forges signatures when an issuer runs many sessions at once, and
  issuance over mail does.
- The IETF drafts for anonymous rate-limited credentials are rejected: the
  work was discontinued at IETF 126, and the name clashes with ARC.
- A secret stamp key that Bob shares with his contacts is rejected: a leak
  from one contact lets anyone make stamps.

### 3.9 The stamp key

Not built.

- Bob's stamp key MUST NOT be the citizen key. It is a new RSA key of 2048
  bits for each key period of 30 days.
- The machine that holds the citizen key MUST make each stamp key, as it
  makes the data roots ([the devices spec](../devices/SPEC.md), section
  3.5). Thus two devices never make two keys for one period.
- That machine MUST keep each private stamp key as self data: a draft with
  the `d` tag `arc-stamp-keys`, sealed under the newest data root. It
  syncs to Bob's devices through his storage citizens, so any device that
  is online can issue stamps. Without storage citizens, only the machine
  that holds the citizen key issues.
- The machine MUST make the key of the next period at least 7 days before
  the current period ends (estimated).
- When a device is revoked, the machine MUST make a new stamp key at once,
  start a new period, and drop the current key without a grace period. The
  revoked device gets no new data root, so it cannot open the new key.

Bob publishes his public stamp keys in a replaceable event of kind 10276:

| Field | Value |
| --- | --- |
| `kind` | 10276 |
| `pubkey` | Bob's citizen key |
| `tags` | one to three tags `["key", <base64 DER SubjectPublicKeyInfo>, <start as decimal Unix seconds>]` |
| `content` | empty |

- The tags name the key of the previous period while its grace period
  lasts, the key of the current period, and the key of the next period
  when it exists. The grace period is 2 days after the end of a period
  (estimated).
- The citizen key signs the event, through the signer. The signer MUST
  refuse to sign kind 10276 for any device, as it refuses kind 10275.
- The event is public, and has no NIP-70 tag. Kind 10276 MUST NOT be a
  sealed kind of an ARC relay.
- Bob MUST publish the event to each relay of his NIP-17 relay list (kind
  10050), and to his indexers.

Kind 10276 is free in [the kind registry](../KINDS.md) and in the kind
table of the NIPs README (checked on 2026-10-15). The build of stamps adds
its row.

### 3.10 Issuance

Not built.

Bob is the issuer. Alice is the client. Bob's contact list is the
attester. Issuance runs over private mail, in two private rumor kinds that
are free in the kind registry and in the NIPs README (checked on
2026-10-15). The build of stamps adds their rows.

**The stamp request, kind 3277.** Alice sends it to Bob.

- Tags: one `p` tag with Bob's key, and one reply stamp (section 3.13).
- Content: `{"key": "<64 hex token_key_id>", "requests": ["<base64 TokenRequest>", ...]}`.
- Each `TokenRequest` is the structure of RFC 9578, section 6.1: 259
  bytes, and 348 characters in base64 (arithmetic).
- A request MUST hold at most 64 entries: about 22,464 bytes (arithmetic:
  64 × 351), below `MaxMailContent`. A request that pays with work MUST
  hold at most 8 entries (estimated), because work grows with the size of
  the wrap.
- Alice keeps the nonce, the challenge digest and the blind inverse of
  each entry until the reply comes, or the request expires.

**The stamp reply, kind 3278.** One of Bob's devices sends it to Alice.

- Tags: one `p` tag with Alice's key, and one `e` tag with the ID of the
  request rumor.
- Content: `{"stamps": ["<base64 blind signature>", ...], "left": <n>}`,
  in the order of the requests. `left` is the number of stamps that Bob
  still issues to Alice in this key period.

Bob's device MUST:

- Answer a request only when its author is a contact at the time of the
  answer. It sends nothing to a stranger.
- Answer only for a key that the newest kind 10276 of Bob names, and for
  which the device holds the private key.
- Issue at most 300 stamps to one contact in one key period (estimated):
  10 wraps a day, 5 messages and 5 acknowledgements (arithmetic:
  300 / 30).
- Count the stamps by the ID of the request rumor, as self data. Two
  devices that answer the same request make the same signatures, because
  the RSA signature of a blinded message is deterministic. Between two
  syncs, the worst case is the limit times the number of devices.
- Not acknowledge a request with kind 3274.

Alice's device MUST:

- Ask Bob for stamps only when Bob published a kind 10276, and Alice holds
  a rumor of kind 14 or an app kind from Bob, or a stamp reply from Bob.
- Ask when she holds fewer than 16 unused stamps for Bob's current key
  (estimated), and ask for stamps of the next key when the event names one.
- Keep at most one open request to Bob for each key. A request expires
  with its wrap, after 7 days.
- Check each stamp after it unblinds it, and drop a stamp that fails.
- Keep each unused stamp by Bob's key and the key ID, and drop it when its
  period and grace period end.
- Ignore a stamp reply that matches no open request.

### 3.11 Redemption

Not built.

A stamp rides on the relay form of a gift wrap of kind 1059, as one tag
`["stamp", "<base64 of the 354 bytes>"]`, in the standard alphabet of RFC
4648, section 4. One wrap MUST carry at most one `stamp` tag. The same
wrap goes to each inbox relay of Bob, with the same stamp, and each relay
redeems it once. A stamp cannot be bound to its wrap: Alice picks the
nonce before Bob signs, and makes the wrap later.

An inbox relay checks a stamp in this order, the cheap checks first:

1. The wrap has exactly one `p` tag, and it names Bob.
2. The tag decodes to exactly 354 bytes, and `token_type` is `0x0002`.
3. The newest kind 10276 of Bob on this relay names a key with the
   `token_key_id` of the stamp, and the time of the relay is in its
   period or its grace period.
4. `challenge_digest` equals the SHA-256 of the challenge for Bob and the
   start of that key.
5. `crypto/rsa.VerifyPSS` with SHA-384 and a salt of 48 bytes accepts the
   `authenticator` over the SHA-384 of the first 98 bytes of the stamp.
6. The nonce is not in the spent set of that key, or it is there with the
   ID of this same wrap.

If each check passes, the relay MUST add the nonce and the wrap ID to the
spent set, and take the wrap. The same wrap again is a duplicate, not a
second use: the outbox sends the same wrap again on each sync. The relay
MUST keep the spent set of a key until the end of its period plus the
grace period, and then drop it.

### 3.12 The relay rule

Not built.

An ARC relay has a mode for gift wraps of kind 1059, set by
`arc relay serve --stamps off|advisory|enforce`:

| Mode | What the relay does |
| --- | --- |
| `off` | It takes wraps as before. This is the default. |
| `advisory` | It runs the rule below, takes each wrap, and counts the wraps that `enforce` would refuse. It lists the tag `arc-stamps` in its NIP-11 document. |
| `enforce` | It runs the rule below, and refuses. It lists the tags `arc-stamps` and `arc-stamps-enforce`. |

The rule, for a wrap of kind 1059:

1. A relay-form wrap whose `p` tag names a citizen with a kind 10276 on
   this relay: a valid unused stamp admits it; else 20 bits of work admit
   it; else, in `enforce`, the relay refuses it with
   `pow: this citizen takes mail with a stamp, or with 20 bits of work`.
2. A courier-form wrap: a stamp cannot apply. In `enforce`, the relay
   takes it only with 20 bits of work.
3. A relay-form wrap to a key with no kind 10276 on this relay: the relay
   takes it as before.

Authentication MUST NOT admit a wrap in cases 1 and 2. Today the relay
checks work only when no key authenticated on the connection
(`adapters/relay/limits/limits.go`), and the relay transport answers the
challenge of an inbox relay with a new random key
(`runtime/citizen/session.go`). Any sender can do the same and skip the
work. `--wrap-auth` and `--wrap-pow` keep their meaning only for kind 21059
and for case 3.

The relay transport tries again with authentication only after
`auth-required:`, so an older sender reports a `pow:` refusal, and does not
loop. The device still runs the filter in each mode.

Revocation of a contact: Bob removes the key. His devices issue it no more
stamps. Its stamps work until their key period and grace period end. The
device filter holds its mail at once.

### 3.13 Reply stamps

Not built.

Bob's acknowledgement to Alice goes to Alice's inbox relays. If they
enforce the rule, Bob needs a stamp of Alice, or work. A provider that
answers a call has the same need, and is not a contact of its callers.

A citizen holds its own private stamp key, so it can make a stamp of its
own key with no blinding. This is a reply stamp.

- A rumor of kind 3272 or 3277 MUST carry one reply stamp of its author,
  when the author published a kind 10276.
- A rumor of kind 14 or of an app kind MUST carry one reply stamp for each
  recipient from whom the author holds no stamp request in the current key
  period.
- The tag inside the rumor is
  `["stamp", <64 hex of the recipient>, <base64 stamp>]`, of 552 bytes
  (arithmetic: 10 + 66 + 1 + 474 + 1). If the reply stamps would make the
  rumor pass `MaxRumor`, the author leaves them out.
- The recipient MUST use a reply stamp only on a wrap to the author of the
  rumor, first for the answer to that rumor.

### 3.14 Sizes

| Item | Bytes | Tag |
| --- | --- | --- |
| The tag `,["nonce","18446744073709551615","20"]` on a wrap | 38 | arithmetic: the largest nonce has 20 digits |
| A stamp | 354 | arithmetic: 2 + 32 + 32 + 32 + 256 |
| A stamp in base64 | 472 | arithmetic: 354 / 3 × 4 |
| The tag `,["stamp","…"]` on a wrap | 485 | arithmetic: 13 + 472 |
| The largest relay-form wrap without a tag | 55,149 | measured, `TestTheLargestMailFitsTheWrapLimit` |
| `MaxWrap` | 55,634 | arithmetic: 55,149 + 485 |

The content of the wrap does not change, so it stays below 65,535 bytes,
which the Bolt event store holds.

## 4. Behavior

### 4.1 A message from a stranger

1. Carol, a stranger to Bob, sends a message with an arc older than this
   spec. Her wrap has no work.
2. Bob syncs. The mail layer opens the wrap, records that Carol wrote to
   Bob, and finds that Carol is not a contact and the ID has less than 20
   bits.
3. It keeps the seal in `held`, and sends no acknowledgement.
4. `arc message inbox` does not show it, and prints
   `1 message from a stranger waits: arc message held` on standard error.
5. `arc message held` lists it.
6. Bob runs `arc contacts add <carol>`. arc adds the key, moves the seal to
   the store, and acknowledges it. Carol's next sync marks it delivered.

### 4.2 A first message

1. Alice runs `arc message send <bob> hello`. Bob never wrote to Alice.
2. arc adds Bob to Alice's contact list.
3. The mail layer finds no record that Bob wrote, opens the seals of Bob
   in the store once, and finds none of kind 14. Alice is a stranger to
   Bob.
4. It seals the rumor, and makes 20 bits of work on the relay form and on
   the courier form.
5. Bob's device admits the wrap by its work, and acknowledges it.
6. Bob answers. Alice wrote to Bob, so Bob makes no work. Alice's device
   admits the answer, because step 2 put Bob on her list.

### 4.3 A contact on two devices

1. Bob adds Carol on his laptop. The laptop writes one draft for Carol.
2. `arc sync` gives the draft to Bob's storage citizens.
3. Bob's phone syncs, takes the draft, and its next mail sync moves the
   held mail of Carol to the store, and acknowledges it.

## 5. Failures

| Failure | What happens | What the user sees |
| --- | --- | --- |
| A stranger sends mail without work | The device holds it, and sends no acknowledgement. | The recipient sees the count on standard error, and the mail in `arc message held`. The sender's outbox shows the message as pending, then expired after 7 days. |
| The work of a wrap is less than its `nonce` tag claims | The device counts the work of the ID, and holds the mail. | As above. |
| More than 200 held seals | The device drops the oldest held seal. | The oldest held message is gone. |
| A held seal's wrap expires | The device drops it at the next sync. | It leaves `arc message held`. |
| Two devices of a citizen have different contact lists | One device can hold mail that the other shows. When the lists sync, the first device moves the mail to the inbox. | The message appears later on that device. |
| The contact list cannot be written, because no data root opens | The send fails before it seals anything. | The error of the data roots. |
| An older relay gets a wrap with work above 55,149 bytes | The relay refuses it with `invalid:`. | The sender sees the refusal for that relay. |
| Not built: an inbox relay in `enforce` gets a wrap with no stamp and no work | The relay refuses it with `pow:`. | The sender sees the refusal for that relay. |
| Not built: a stamp was spent on this relay by another wrap | The relay applies case 1 of section 3.12. | The same wrap still reaches the other inbox relays. |
| Not built: Bob does not answer a stamp request | The request expires after 7 days. | Alice sends with work when each inbox relay of Bob enforces. |
| Not built: Bob's limit for Alice is reached | The reply holds fewer stamps, and `left` is 0. | Alice sends with work until the next period. |

## 6. Security

### 6.1 What each party can do

| Party | Can | Cannot |
| --- | --- | --- |
| stranger | Send mail with work, and reach the inbox. Fill the held bucket with mail without work, and push out older held mail. | Reach the inbox without work. Learn whether its mail was held or lost. |
| contact | Send without work. Not built: give stamps to someone else. | Not built: get more stamps than the limit from one device. |
| relay | Drop mail, as before. Not built: learn that a wrap has a stamp or work. | See the author, so it cannot apply the filter. Not built: link two stamps to one contact, or make a stamp. |
| reader of a relay | Not built: copy a stamp from a wrap, and spend it once on each other inbox relay of the recipient. An ARC relay serves a gift wrap to any reader. | Not built: spend it twice on one relay. |
| revoked device | Not built: make stamps with the key that it held, until the signer makes a new key. | Not built: open the new key. |

A stranger can push real held mail out of the bucket with 200 cheap
messages. That mail then waits for the sender to try again. The work rule
keeps the inbox clean, and the bucket bounds the disk. Neither stops a
stranger from filling the bucket. The relay rule of section 3.12 would
move this cost to the stranger on each enforcing relay.

The device still unwraps each wrap, because only the seal names the
author: two NIP-44 decryptions in the signer for each wrap. The filter
removes the signature of the acknowledgement for each held message.

Blind stamps hide nothing from the recipient, who opens the seal. They
keep what a relay sees today: it cannot link two wraps to one sender.

### 6.2 Cost of the work

Measured on an Apple M3 Max with `nip13.DoWork` of `fiatjaf.com/nostr`
(the fork at `github.com/gezibash/nostrlib`), on 2026-10-15:

| Wrap | One attempt, one core | 16 bits, one core | 20 bits, one core | 20 bits, 16 cores |
| --- | --- | --- | --- | --- |
| short message, 1,659 bytes | 1.32 µs (measured) | 86 ms (arithmetic) | 1.38 s (arithmetic) | 0.65 s, mean of 5 runs (measured) |
| largest mail, 54,962 bytes | 38.6 µs (measured) | 2.5 s (arithmetic) | 40 s (arithmetic) | — |

The arithmetic is one attempt times 2^16 or 2^20, the expected number of
attempts.

- At 20 bits, one core makes about 2,600 short messages to strangers each
  hour (arithmetic: 3,600 s / 1.38 s). The work raises the cost of bulk
  mail; it does not stop a sender who pays.
- A phone is slower than this laptop. No run on a phone exists.
- 16 bits costs a sender 86 ms for each short message, which does not slow
  bulk mail (estimated). That is why the constant is 20.
- The largest mail to a stranger costs about 40 s of one core. A first
  message to a stranger is usually short (estimated).
- A sender pays the work for each message to a recipient that has not
  written to it, not only for the first one, and pays it twice: once for
  each form. A short message to a stranger thus costs about 2.8 s of one
  core (arithmetic: 2 × 1.38 s).

### 6.3 Cost of a stamp

Not built. Measured on an Apple M3 Max with Go 1.27.1 and `crypto/rsa`,
on 2026-10-15, three runs of `go test -bench . -benchtime 3s`:

| Operation | Who | Time | Tag |
| --- | --- | --- | --- |
| Make an RSA key of 2048 bits | Bob's signer, each 30 days | 43 to 57 ms | measured |
| One private-key operation (SignPSS, SHA-384, salt 48) | Bob's device, for each stamp | 0.95 to 1.07 ms | measured |
| VerifyPSS of one stamp | the relay, for each wrap | 32 to 36 µs | measured |

SignPSS stands in for the blind signature of RFC 9474: each is one RSA
private-key operation. The spent set of a relay holds 64 bytes for each
redeemed stamp: 960,000 bytes on each inbox relay in one period for a
citizen with 50 contacts that each use the full limit (arithmetic:
50 × 300 × 64).

## 7. Compatibility

- An older recipient opens all mail, as before. It ignores the `nonce`
  tag.
- An older sender makes no work. Its mail to a newer citizen that does not
  have it as a contact is held, and shows as expired in its outbox after 7
  days. Before, the same mail showed as delivered. This is the visible
  change for older senders.
- NIP-17 clients outside ARC make no work for gift wraps. Their first mail
  to an ARC citizen is held.
- An older device of a citizen shows each message, held or not, because
  it has no filter. It ignores the contact drafts.
- A device that updates puts each recipient of its outbox on the contact
  list at its first sync (section 3.1). A key that wrote to the citizen,
  and that the citizen never answered, is a stranger after the update.
- A newer device that updates has no record of who wrote to it. Its first
  send to each key opens the seals of that key in its store, once
  (section 3.6). On a device, each seal is a request to the signer.
- An older relay refuses a wrap above 55,149 bytes. Only the largest mail
  with work passes that size. A newer relay takes up to 55,634 bytes.
- The relay flags `--wrap-auth` and `--wrap-pow` do not change.
- `arc message held --json` prints the message line of
  [the client programs spec](../clients/SPEC.md), section 3.2.

This spec changes normative sections of
[the delivery spec](../delivery/SPEC.md): section 3.4 (`MaxWrap`), section
4.2 (held mail gets no acknowledgement), and section 6.2 (contacts and
work). It shipped as a breaking commit.

The stamps of sections 3.8 to 3.13 will change the delivery spec again
(section 3.7, kinds 3277, 3278 and 10276; section 6.2, the relay rule) and
[the devices spec](../devices/SPEC.md) (the signer makes, rotates and
signs the stamp key). They do not change `MaxWrap` again.

## 8. Proof

| Test | What it proves |
| --- | --- |
| `TestTheWrapLimitsAgreeWithTheMeasurement` | `MaxWrap` is the measured 55,149 bytes plus the 485 bytes of a stamp tag. |
| `TestTheLargestMailFitsTheWrapLimit` | The largest mail, sealed and wrapped with the real code, is 55,149 bytes, and fits `MaxWrap` with the largest `nonce` tag and with a `stamp` tag. |
| `TestAWrapWithWorkHasTheWorkInItsID` | A wrap made with `Work` has at least 20 bits in its ID, with a real search, and still verifies and opens. |
| `TestAMessageFromAStrangerWithoutWorkIsHeld` | A message of an older sender to a citizen with the filter is held, is not in the inbox, and gets no acknowledgement. |
| `TestAMessageFromAStrangerWithWorkReachesTheInbox` | A newer sender makes 20 bits of work, with a real search, for a recipient that never wrote to it. The message reaches the inbox and is acknowledged. |
| `TestAWrapThatClaimsWorkItDoesNotHaveIsHeld` | A wrap whose `nonce` tag claims 20 bits, and whose ID has fewer, is held. |
| `TestAcceptingAStrangerReleasesItsHeldMail` | A key added to the contacts releases its held mail to the inbox, and the sender's outbox shows it as delivered. |
| `TestASyncReleasesTheHeldMailOfANewContact` | A sync that reads no wrap releases the held mail of a key that joined the contacts. |
| `TestTheReplyOfARecipientNeedsNoWork` | A send adds the recipient to the contacts. The reply and the acknowledgement of the recipient carry no work, and reach the inbox. Only the first message of the sender carries work. |
| `TestACallFromAStrangerIsNotHeld` | A call request from a stranger reaches the provider without work. |
| `TestAGroupMessageFromAStrangerIsHeld` | A group message from a stranger, with a contact in its `p` tags and no work, is held. |
| `TestTheHeldMailHasALimit` | Past the cap, the newest held seal pushes out the oldest. |
| `TestHeldMailExpiresWithItsWrap` | A held seal goes when its wrap expires. |
| `TestAMessageFromAStrangerCrossesAStick` | A stranger's message crosses a stick with no relay: the courier form carries work, and reaches the inbox. |
| `TestAHeldSealMovesWhenAWrapWithWorkArrives` | A courier form without work is held, and the relay form of the same seal with work moves it to the inbox. |
| `TestAnOlderMessageFromTheRecipientCountsOnce` | A message that the recipient sent before the filter counts: the next send to it makes no work. |
| `TestAnOldOutboxSeedsTheContacts` | The mail of arc v0.31.0, in `core/mail/testdata/outbox-v0.31.0`, puts both recipients of its outbox on the contact list at the first sync, and a second sync does not seed again. |
| `TestContactsReleaseHeldMail` | Through the CLI: `arc message inbox` reports the held message on standard error, `arc message held --json` lists it, `arc contacts add` moves it to the inbox, and `arc contacts rm` removes the key. |
| `TestASendAddsAContact` | Through the CLI: `arc message send` puts the recipient on `arc contacts`, and the message reaches the recipient. |

`TestDelivery` of [the delivery spec](../delivery/SPEC.md) proves that a
stick that a courier carries names neither citizen, after a send that wrote
a contact draft.

Unverified: no test syncs the contact list between two devices through a
storage citizen, or adds one key on two devices at once. No test runs an
older arc. No run measures work on a phone. Stamps (sections 3.8 to 3.13)
have no test, because they are not built.
