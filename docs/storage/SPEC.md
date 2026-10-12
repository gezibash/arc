# Storage citizens and the sync of self data

- Status: partial
- Layers: sdk, runtime
- Owns: sdk/storage
- Proof: go test -count=1 -run '^(TestStorageJournalSync|TestStorageImportsDrafts|TestStorageSealsWithDataRoots|TestStorageList|TestPruneGone|TestStorageNetworkSync|TestStoragePieces|TestSilence|TestInviteAdmits|TestAskRule|TestOpenRuleQuotaEachAccount|TestNewKeys|TestPieceKeys|TestCode|TestPieces|TestPieceIDsDiffer|TestParityPrefix|TestNewStorageCitizenAddsPiece|TestPutManyWindow|TestPutManyOlderApp|TestRequestsInFlight|TestHelloAndWindow|TestPieceTags|TestCrashMidWrite|TestImportOldState|TestIndexBytesGrowLinearly|TestAudit|TestAuditReplay|TestAuditProofVector|TestDrain|TestStorageRepair|TestStorageRepairTURN|TestRepairGone|TestRepairTURN|TestRepairAudit|TestRepairRule2|TestRepairDrain|TestRepairExtraPiece|TestRepairMirror|TestStorageFormat1|TestRepairFormat1|TestRepairFormat1Interrupted|TestRepairFormat1Drain|TestRepairFormat1Bound|TestMovable|TestMoves|TestKept|TestAStoreRefusesAMovedDeletion|TestARelayRefusesNewDrafts|TestARelayServesSealedDataOnlyToItsAuthor|TestRelayAddPublishesNoPrivateRelayList)$' ./internal/proof/ ./apps/storage/cmd/arc-storage/ ./runtime/selfsync/ ./sdk/storage/ ./apps/storage/server/ ./core/store/ ./adapters/relay/sealed/ ./cmd/arc/
- Remaining: the steps that section 8.2 names: the usage line of `arc sync`, the removal of the bytes of a closed account, set reconciliation, the deletion of blobs, a command that removes the old drafts from a relay, and the close call at the end of a drain.
- Unverified: No test runs the two ends of a pipe on two machines or behind real NATs; the tests use the loopback address. No test runs a signer that is offline when it revokes a device. No test runs the sync of `arc serve` on its 15-minute tick; the tests run `arc sync`, and the repair of `arc serve` at its start. No test runs a relay that is not an ARC relay with the storage list. No test asks a real bunker to sign a storage list through the normal CLI, because the device refuses first; the bunker policy has a unit test. No test measures a sync of 1,000 pages through `arc sync`; the measurement of requests in flight runs the app and the client on an emulated link, not on two machines. No test stops the power of a machine: the crash test stops the program, and the page cache of the machine survives. No test runs an invite token older than 7 days, a request older than 30 days, or an account after its `expires`; unit logic decides them from the clock. No test gives a blob of a later generation to a revoked device through `arc sync`: the signer and the storage citizen refuse the device first. The proof reads the data roots that the revoked device keeps on its disk, and tries each one on each blob file. The proof of a parallel sync delays each ARC call to a storage app with a wrapper program; no test delays the pipe itself on a real link. No test waits 7 days: the proof moves `silent_since` back on the disk of the device, and a unit test gives the clock. No test audits a blob of 64 MiB, or 256 blobs in one request; the tests audit blobs of 4 KiB. The test of a replayed answer runs the app and the client in one test program, not through `arc-storage`, because the CLI makes a new nonce on each run and cannot send an old answer. The proof of TURN runs the TURN server of a local relay on the loopback address, with no NAT. No test waits 24 hours for the next audit: the proof audits at the first repair, and a unit test gives the clock. No test runs two devices that repair at the same time through the CLI; a unit test makes the extra piece by hand. No test stops `arc` in the middle of a move of blobs of format 1 to pieces: the proof stops a storage citizen, and a unit test ends a pipe before its puts and before its deletes. No test moves more than 1,024 blobs of format 1 through the CLI; a unit test checks the limit. The proof of the pieces runs one older storage app, built from commit d53e91b. `TestStorageFormat1` runs `arc` of commit d53e91b against storage citizens with pieces: its sync fails with `the storage citizen announced a blob of`. `TestStoragePieces` and `TestStorageFormat1` need commit d53e91b in the checkout, so a shallow clone fails them. No test codes a blob larger than 4 KiB through the CLI; unit tests code blobs of 4 KiB and 64 KiB. No test runs two devices that code one record at the same time. The proof of the removal of the old drafts runs `arc relay serve` of commit d53e91b as the older relay; it sends the drafts of the older client to that relay with the relay adapter, not with an older `arc`. No test runs a v0.25.0 device against a relay that serves a moved deletion: the relay and the store refuse it, and unit tests prove each refusal. No test removes the drafts on 2027-01-10. No test waits 30 days for the removal of a gone entry: the proof moves `since` back in a storage list that the test signs with the citizen key, and a unit test gives the clock. No test ends a drain with a signer that signs through a bunker; such a signer does not hold the citizen key, so it does not end a drain.

This spec defines how a device of a citizen keeps its self data in sync with
a storage citizen. The design is in
[the storage proposal](../proposals/storage.md). This spec holds the parts
that are built.

## 1. Purpose

Self data is the data that a citizen keeps for itself: the pages of the
journal, the files of the files app, and the other `sealed` kinds of
[the app interface](../interface/SPEC.md), section 3.2. The relay is for
coordination only. Self data therefore stays in the local store of each
device. A storage citizen holds encrypted copies, so that another device of
the citizen gets them.

This spec defines:

- the account key of a citizen for one storage citizen, and the grant set
  that it signs for the device keys,
- the join call that asks a storage citizen for an account, the rules of
  admission, and the record of each account: its quota, plan and end,
- the ARC call that opens a pipe to the storage app, and the requests on the
  pipe (`sdk/storage`),
- the blob that holds one record, and the pieces of a blob,
- the sync of a device with its storage citizens,
- the one-time import of the drafts that the relays hold, and their removal
  from the relays.

The storage app documents its service, its settings and its storage on disk
in [its README](../../apps/storage/README.md).

Without this spec, the journal sends each draft, checkpoint and part to the
relays. The relay operator then sees the count, the sizes and the times of
the self data of each citizen. On 2026-10-07, 1,000 journal pages held 16 MB
on a relay.

## 2. Terms

- **Self data**: the events of kinds 31234 (draft), 1234 (checkpoint) and
  3275 (part) that a citizen signed, and the kind 5 deletions of these
  events.
- **Storage citizen**: a citizen that serves the storage app. A device uses
  a storage citizen that it installed: from the storage list, or with
  `arc install <key> storage`.
- **Storage list**: the sealed event of kind 10273 that names the storage
  citizens of a citizen (section 3.8). Only the signer changes it.
- **Record**: one event of self data.
- **Blob**: the encrypted bytes of one record (section 3.4).
- **Piece**: one shard of the Reed-Solomon code of a blob, sealed alone
  (section 3.10). One storage citizen holds one piece of a blob.
- **k** and **t**: k pieces of one seal rebuild a blob. A blob has t
  pieces, on t storage citizens.
- **Seal**: 8 random bytes that name one coding of one blob. Pieces of two
  seals never mix.
- **Tag**: the sealed header of a piece: its blob ID, seal, k, index and
  blob size. The storage citizen keeps it, and cannot open it.
- **Format 1**: a whole blob on a storage citizen, with no tag. A device
  puts it only on a storage app without the feature `pieces`.
- **Device key**: the random key of one machine. [The device spec](../devices/SPEC.md)
  defines it.
- **Account key**: the key of a citizen for one storage citizen. The signer
  derives it from the citizen key (section 3.1). The storage citizen knows the
  account by this key, and never sees the citizen key.
- **Grant set**: the device keys that can use one account. The account key
  signs it. A device key that the newest grant set does not hold has no
  grant: it is revoked, or it was never added.
- **Signer**: a machine that holds the citizen key. [The device spec](../devices/SPEC.md)
  defines it.
- **Keyed root**: the root key of the keyed values of a citizen. The app
  interface spec, section 3.7.1, defines it.
- **Cursor**: the last sequence number that a device listed from one storage
  citizen.
- **Account record**: the status, quota, plan, end and state of one
  account, which the storage citizen keeps (section 3.7).
- **Invite token**: 128 random bits that admit one account. The operator
  makes it, and gives it to a citizen in an invite link.
- **Admitted account**: an account key in `STORAGE_OWNERS`, or an account
  key whose record has the status `admitted`.

## 3. Rules

### 3.1 The account key

- The account key MUST be
  `HKDF-SHA256(citizen secret key, salt = none, info = "arc-storage-account-v1" || key of the storage citizen, 32 bytes)`.
  The key of the storage citizen goes into the info as its 32 bytes.
- If the 32 bytes are 0, or are not less than the order of secp256k1, the
  derivation MUST fail. The chance is about 2^-128, so no retry is defined.
- Only a signer derives the account key. A device that signs through a
  remote signer MUST NOT get it. NIP-46 has no method for it.

Reason: two storage citizens see two account keys that they cannot join, and
neither sees the citizen key (proposal, decision 4).

### 3.2 The ARC calls

Each call is a live call to the capability `storage`, method `RAW`, path
`/`. The author of the call is never the citizen key:

- A signer MUST author each call with the account key for that storage
  citizen.
- A device that signs through a remote signer MUST author each call with
  its device key.

`core/call` seals each reply to the author of the request. A device thus
opens the reply with its own device key, and the remote signer decrypts
nothing for it.

**The grant set.** The signer sends it to a storage citizen:

- The body MUST be `{"v":1,"grants":["<device key in hex>",...],"at":<n>}`.
  `grants` holds each device key of the device list, at most 64. `at` is the
  time of the device list event, in Unix seconds.
- The account key MUST author the call. The call is the signed record: the
  ARC call event carries the signature. No other event kind holds a grant.
- The storage app MUST refuse the set with `not_owner` if the author is not
  an admitted account, or if its account is closed. It MUST refuse a set with an `at` that is less than the
  `at` of the set it holds, with `stale_grants`. A set with an equal `at`
  replaces the set, so the signer can send it again.
- The reply MUST be `{"v":1,"devices":<n>}`, the count of device keys.
- The newest set replaces the older set. A device key that the newest set
  does not hold is revoked. A revocation is a set without the device.

**The session.** A device opens a session with one call:

- The manifest command `open` of the installed app makes the body:
  `{"v":1,"session":{"hold_ms":<n>,"stun":"<url>","sdp":<offer>}}`. An older
  client also sent `account`, a pseudonym; the storage app ignores it.
- The storage app MUST find the account from the author: an admitted
  account key uses its own account, and a device key uses the admitted
  account whose newest grant set holds it. It MUST refuse each other author,
  and each author of a closed account, with `not_owner`, before it opens a
  pipe. It MUST read the account records on each call, so that a change of
  the operator counts at once.
- The reply MUST be `{"v":1,"sdp":<answer>}`, or an error: `invalid_request`,
  `not_owner`, `busy` or `storage_failed`.
- The two ends then open one pipe, as [the direct pipe spec](../direct/SPEC.md)
  defines.

**The join.** The signer asks a storage citizen for an account:

- The body MUST be
  `{"v":1,"join":{"invite":"<token>","want":<n>,"note":"<text>","citizen":"<hex>","sig":"<hex>"}}`.
  Each field of `join` is optional.
  - `invite` is the token of an invite link: 32 lower-case hex digits.
  - `want` is the quota that the citizen asks for, in bytes.
  - `note` is a text for the operator, at most 140 bytes.
  - `citizen` and `sig` show the citizen key, by choice. Both are set, or
    neither. `sig` MUST be the BIP-340 signature of the citizen key over
    `SHA-256("arc-storage-join-v1" || 0x00 || account key || key of the storage citizen)`,
    with each key as its 32 bytes. The default sends neither.
- The account key MUST author the call.
- The storage app MUST answer `invalid_request` to a body that breaks these
  rules, or to a signature that does not verify.
- The reply MUST be one of these bodies:

  ```json
  {"v":1,"account":{"status":"admitted","quota":1073741824,"used":0,"plan":"free","state":"open","tombstone_days":90,"read_days_after_close":30}}
  {"v":1,"account":{"status":"pending","message":"the operator decides by hand; ask again later"}}
  {"v":1,"error":"refused","message":"this storage citizen admits only with an invite"}
  ```

  An admitted reply also holds `expires`, in Unix seconds, if the account
  has an end. A refusal is a body, not an error of the call, so it carries
  a message for a person. The error of the call is kept for a request that
  the app cannot read.
- A join of an admitted account MUST NOT change the account. The reply
  gives its current record. Thus the signer can send the join again.

### 3.3 The requests on the pipe

The device sends each request as one text message of JSON. The storage app
answers with text messages of JSON, in the order of the requests.

| Request | Reply |
| --- | --- |
| `{"op":"hello"}` | `{"v":2,"features":["batch","pieces","audit","drain"],"window":32}` |
| `{"op":"put","id":"<64 hex>","size":<n>}` | `{"status":"send"}`, then the device sends the bytes, and the app answers `{"status":"stored","seq":<n>}`. Or `{"status":"held"}` or `{"status":"tombstoned"}`, and no bytes go. |
| `{"op":"put","id":"<64 hex>","size":<n>,"tag":"<base64>"}` | The put of a piece, with the same replies. |
| `{"op":"list","after":<cursor>,"limit":<n>}` | `{"entries":[{"seq":<n>,"id":"…","size":<n>}, {"seq":<n>,"id":"…","size":<n>,"tag":"<base64>"}, {"seq":<n>,"id":"…","deleted":true}],"next":<cursor>}` |
| `{"op":"get","id":"<64 hex>"}` | `{"size":<n>}`, then the bytes of the blob. |
| `{"op":"get","ids":["<64 hex>",…]}` | `{"items":[{"id":"…","size":<n>},{"id":"…","error":"not_found"}]}`, then the bytes of each found blob, in the order of `ids`. |
| `{"op":"delete","id":"<64 hex>"}` | `{"status":"deleted","seq":<n>}` or `{"status":"absent"}`. |
| `{"op":"delete","ids":["<64 hex>",…]}` | `{"items":[{"id":"…","status":"deleted","seq":<n>},{"id":"…","status":"absent"}]}` |
| `{"op":"usage"}` | `{"used":<n>,"quota":<n>,"blobs":<n>,"state":"<state>","plan":"<plan>","expires":<n>}` |
| `{"op":"audit","id":"<64 hex>","nonce":"<64 hex>"}` | `{"size":<n>,"proof":"<64 hex>"}`, or the error `not_found`. Section 3.9. |
| `{"op":"audit","ids":["<64 hex>",…],"nonce":"<64 hex>"}` | `{"items":[{"id":"…","size":<n>,"proof":"<64 hex>"},{"id":"…","error":"not_found"}]}` |
| `{"op":"drain"}` | The usage reply, with the state `draining`. |
| `{"op":"drain","off":true}` | The usage reply, with the state `open`, or `closing` if the account closes. |

**Requests in flight.**

- Before a device keeps requests in flight or sends `ids`, it MUST send
  `hello`, one time in each session. If the reply holds the feature
  `batch`, the device can keep requests in flight. If the reply is the
  error `invalid_request`, or holds no `batch`, the device MUST send one
  request at a time, with no `n`, and wait for its last reply.
- With `batch`, the device MUST number each request that it keeps in
  flight, and each request with `ids`, with `n`: from 1, one higher for
  each numbered request of the session. Another request can have no `n`. The app MUST put the `n` of the
  request in each reply to it, also in a refusal.
- The device MUST keep at most `window` puts in flight. A put is in flight
  from its request to its last reply: `stored`, `held`, `tombstoned`, or an
  error. The window is 32. The app MUST end the pipe if a 33rd put comes.
- The device MUST send the bytes of each put after its `send` reply, in
  the order of the `send` replies. The app MUST give each run of bytes to
  the oldest put that got `send` and has not all its bytes. Thus the order
  of the bytes of each put is kept. A text request can come between two
  runs, and not inside one binary message.
- The app MUST write the entry of a put before it answers `stored`. It MAY
  write the entries of several puts in one transaction.
- The app MUST write the entries of the puts that wait before it answers a
  request that is not a put. Thus a `list` sees each put that came before
  it.
- A request with `ids` MUST name 1 to 256 IDs, and MUST NOT have `id`. The
  app answers `invalid_request` to another list. A delete with `ids` is one
  change of the index.

**Pieces.**

- A storage app with the feature `pieces` MUST keep the tag of a put, and
  MUST give it in each list entry of that ID, in standard base64. An entry
  with no tag is a blob of format 1.
- A tag MUST be 87 bytes. The app answers `invalid_request` to a tag of
  another length.
- A put with a tag MUST have the size of a piece: `ceil(B / k) + 45`, for a
  B that is a power of two from 4,096 bytes to 64 MiB, and a k from 1 to 8.
  A put with no tag MUST have the size of a blob. The app answers
  `bad_size` to another size.
- The app does not open a tag, and does not decode a piece.
- A device MUST send `hello` before it puts a piece. If the reply does not
  hold `pieces`, the device MUST NOT put a piece on that app: an older app
  ignores `tag`, and would keep a piece with no tag.

**The other rules.**

- The bytes of a blob or a piece MUST go in binary messages of at most 16
  KiB.
- A refused request gets `{"error":"<code>","message":"<text>"}`. The codes
  are `invalid_request`, `bad_size`, `over_quota`, `refused`, `not_found`
  and `storage_failed`.
- The `quota` of a usage reply MUST be the quota of the account. `state`,
  `plan` and `expires` are the account record (section 3.7). `expires` is
  absent if the account has no end.
- A put MUST be refused with `over_quota` if the bytes in use, the bytes
  of the puts of the session in flight and the blob pass the quota of the
  account. If the operator lowers the quota below the bytes in use, each
  put is refused, and each read and delete works.
- A put on a closing account MUST be refused with `refused`. Each read,
  list and delete works until the reads end.
- A list MUST give at most 1,000 entries. A device lists again from `next`
  until a list gives no entry.

Reason: a put takes two round trips. One request at a time, 1,000 puts at
44 ms took 117.1 to 121.3 s on an emulated link. With 32 puts in flight, they took
7.9 to 8.5 s (measured, section 8.1).

### 3.4 The blob

```text
blob     = id || ts || gen || nonce || XChaCha20-Poly1305(blob key, nonce, pad(envelope), aad = id || ts || gen)
id       = HMAC-SHA256(id key, record ID)                    32 bytes
ts       = 0                                                  8 bytes, big endian
gen      = generation of the blob key                         4 bytes, big endian
nonce    = random                                            24 bytes
pad(e)   = length of e (8 bytes, big endian) || e || zeros
```

- The size of the whole blob MUST be the next power of two that holds it,
  and at least 4,096 bytes. The largest blob is 64 MiB.
- The envelope MUST be this JSON object:
  `{"v":1,"device":"<hex>","sig":"<hex>","record":<event>}`.
- `sig` MUST be the BIP-340 signature of the key in `device` over
  `SHA-256("arc-storage-change-v1" || 0x00 || id as 64 hex characters || record ID as 32 bytes)`.
  Thus the key of the device signs each change inside the encrypted bytes.
- If a machine holds the citizen key, it MUST sign with the citizen key. It
  is the signer, and is on no device list. A machine that signs through a
  remote signer MUST sign with its device key.
- The record MUST be signed by the citizen key, as the local store requires
  today.
- For generation g, the keys MUST be
  `blob key = HKDF-SHA256(data root of g, info = "arc-storage-blob-v1")` and
  `id key = HKDF-SHA256(keyed root, info = "arc-storage-id-v1")`, with no
  salt, 32 bytes each. The data roots are in section 3.5 of
  [the devices spec](../devices/SPEC.md). The data root of generation 0 is
  the keyed root. Thus a blob of generation 0 has the keys of a blob that an
  older device sealed, and a blob keeps its ID in each generation.
- A device MUST seal each new blob with the newest data root that it holds,
  and MUST write the generation of that root in `gen`. With no roots event,
  the newest data root is generation 0.
- A device MUST open a blob with the data root of the generation in `gen`.
  If it holds no data root of that generation, it refuses the blob.

### 3.5 What a device takes

A device MUST refuse a piece, and use another piece of the seal, if any of
these is true:

- its tag does not open with the piece key of the generation in the tag,
- the HMAC of the blob ID and the index in the tag is not the ID that the
  storage citizen listed,
- the tag names a k or a blob size other than the other pieces of its seal,
- the piece does not open with its tag as the associated data, or names
  another generation than its tag.

A device MUST refuse a blob, and give nothing to its store, if any of these
is true:

- the blob does not open, or its `id` is not the ID that the device asked
  for,
- the HMAC of the record ID is not the `id`,
- the record has a bad ID or a bad signature, or another citizen signed it,
- the record is not self data,
- the device signature does not verify,
- the citizen has a device list, and the key in `device` is neither the
  citizen key nor on the list.

A blob that a device refused because its device key is not on the list gets
a new check on each sync. A device skips each other refused blob after the
first refusal.

### 3.6 Where self data goes

- A device MUST NOT send self data to a relay. `arc` keeps it in the local
  store.
- `arc sync --dir` MUST still carry self data, because the citizen carries
  the directory.
- The draft of the keyed root stays on the relays. A machine that signs
  through NIP-46 reads it there.

### 3.7 Admission and the account record

The storage app keeps one record for each account:

| Value | Rule |
| --- | --- |
| `status` | `admitted`, `pending` or `refused`. |
| `quota` | Bytes. The admission sets it. The operator changes it. |
| `plan` | A name that the operator chooses, such as `free` or `friend`. ARC gives it no meaning. |
| `expires` | Unix seconds, or 0 for no end. |
| `state` | `open`, `draining`, `closing` or `closed`. The app derives it, as below. |

- An account key in `STORAGE_OWNERS` with no record is admitted, with the
  quota `STORAGE_QUOTA` and the plan `owner`.
- An account is `closing` from the earlier of the time that the operator
  closed it and `expires`. It is `closed` 30 days later. A closed account
  takes no session.
- An account is `draining` from a `drain` request until a `drain` request
  with `off`, unless it is `closing` or `closed`. Each session of the
  account can send it: the account key or a device key with a grant. The
  app keeps the time of the drain in `state.json`, also for an account of
  `STORAGE_OWNERS` with no record. A second `drain` request does not change
  the time.
- A draining account MUST refuse each put with `refused`. It MUST keep each
  get, list, delete, usage and audit, with no end. Its usage reply gives
  what remains: `blobs` and `used`. A list from cursor 0 names each blob
  that remains, and each deletion.

Reason: a citizen that drops a storage citizen moves its data away first
(proposal, section 5.2). A drain stops new data there, and keeps the reads
for the move. A device can delete each blob, so a drain gives a device no
new power. The operator closes an account with `account close`, which ends
the reads after 30 days.

The operator chooses one rule with `STORAGE_ADMIT`. A join of an account
that is not admitted gets this answer:

1. If the join has a token, and the token is known and less than 7 days
   old, the app MUST spend the token, and then admit the account at the
   quota and the plan of the token. Each other token MUST get
   `refused`. A token admits under each rule. If the program stops after
   the spend and before the admission, the token is lost; no token admits
   two accounts.
2. Under `invite`, the default, a join with no token MUST get `refused`.
3. Under `ask`, a join with no token MUST get `pending`, and the app keeps
   the request: `want`, `note`, the citizen key if the join shows it, and
   the time. A second join of a pending account does not change the
   request. A request ends after 30 days; the next join then makes a new
   one. An account that the operator refused MUST get `refused`.
4. Under `open`, the app MUST admit the account at `STORAGE_OPEN_QUOTA`,
   with the plan `free`, unless the sum of the quotas of the accounts that
   are not closed, and this quota, passes `STORAGE_CAPACITY`. Then it MUST
   answer `refused`.

The app keeps each token as the SHA-256 of the token, so a reader of the
disk cannot use it.

**The invite link** is `storage+arc://<key of the storage citizen>?invite=<token>`.
The token is 16 random bytes, as 32 lower-case hex digits. The query holds
only `invite`.

### 3.8 The storage list

The storage list is one replaceable event of kind 10273:

| Field | Value |
| --- | --- |
| `kind` | 10273 |
| `pubkey` | the citizen key |
| `tags` | exactly `[["-"]]`, the NIP-70 tag |
| `content` | the list as JSON, encrypted with NIP-44 from the citizen key to the citizen key |

The JSON inside is:

```json
{"v":1,"storage":[
  {"key":"<64 hex>","name":"charlie","relays":["wss://relay.republic.sh"],"state":"active","since":1791800000}
]}
```

| Field | Rule |
| --- | --- |
| `key` | The key of the storage citizen. |
| `name` | The installed name of its storage app on each device. 1 to 64 bytes, and no control characters. |
| `relays` | The relays of the citizen when the signer added the entry. At most 8. |
| `state` | `active`, `draining` or `gone`. |
| `since` | Unix seconds of the last change of `state`. |

- Only the citizen key MUST sign a storage list.
- A list MUST hold at most 16 entries. Two entries MUST NOT have the same
  key or the same name.
- `v` is 1. A reader MUST refuse another version, and say to update arc.
- A new list MUST have a `created_at` later than the list that it replaces.
- A reader MUST ignore a JSON field that it does not know.
- A reader MUST refuse a list with another author, a bad signature, content
  that does not open, or a limit that is broken. Of the lists that open, the
  reader takes the newest, as NIP-01 orders replaceable events.
- A device syncs with each storage citizen that is `active` or `draining`.
  It removes the install of a storage citizen that is `gone`, and does not
  sync with it.
- A device reads from a `draining` storage citizen, and MUST NOT put a blob
  on it. Each sync sends it a `drain` request (section 4.1), and repair
  moves the data off (section 4.6).
- The signer sets `gone` when a draining storage citizen holds no piece
  and no blob, and takes no put (section 4.6). The user sets `gone` by
  hand for a storage citizen that does not answer.
- The signer MUST remove an entry that is `gone` for 30 days or more: 30
  days after its `since`. It does this at each sync, and at each change of
  the list. A device that did not sync in those 30 days then finds a local
  install that the list does not name (rule 3 of section 4.3).
- A relay MUST serve kind 10273 only to its author after NIP-42
  authentication, as it serves kind 10274. `adapters/relay/sealed` enforces
  this for an ARC relay. The relay sees that the citizen has a storage list,
  its size and its time. It does not see the storage citizens.
- The signer MUST refuse to sign kind 10273 for any device, whatever its
  `--allow-kind` flags say. A manifest MUST NOT name kind 10273.
  `runtime/iface` reserves it.

### 3.9 Audits

A device checks that a storage citizen still holds a blob, and does not
get the blob.

- The device MUST make a new nonce of 32 random bytes for each audit, and
  MUST NOT send a nonce two times.
- The app MUST answer, for each ID, the proof
  `SHA-256("arc-storage-audit-v1" || 0x00 || nonce || id || bytes)`. `id`
  goes in as its 32 bytes. `bytes` are the bytes of the blob that the app
  holds, as a get gives them.
- The app MUST read the bytes for each audit. It MUST NOT keep a proof.
- The app MUST answer `not_found` for an ID with no entry, and for an
  entry whose file is gone. A request with `ids` names 1 to 256 IDs.
- An audit passes if the app gives the proof that the device computed. An
  answer of `not_found`, another proof, or no answer is a failure.
- The device computes the expected proof from the bytes of the blob.
  Thus it holds the bytes, or it computed the proof for a nonce at a time
  when it held them. `arc-storage audit` pads the local file again. A
  device of `arc` keeps one proof for a secret nonce of a piece after it
  put the piece or got it (section 4.6). A device with no kept proof gets
  the piece and opens it, as section 6.9 of
  [the storage network proposal](../proposals/storage-network.md) says.
- The audit names a blob by its ID. A piece of Reed-Solomon has its own ID
  on the storage citizen, so the same request audits a piece. The proof
  covers the bytes of the piece, not its tag.

Reason: the nonce comes before the bytes. Thus the app cannot hash the
bytes before it knows the nonce, and it cannot pass without each byte.
One changed byte gives another proof. A proof for an older nonce does not
pass for a new nonce, so a replayed answer fails. A device sends 32 bytes
and gets 32 bytes for each blob, in place of the whole blob of up to
64 MiB. The proposal gets and opens a piece. That also proves the bytes,
but it moves each blob whole. A hash over the nonce and the bytes is the
simplest check that needs no new state on the storage citizen.

### 3.10 The piece

A device codes each blob into pieces, and puts one piece on each of t
storage citizens. For a blob B of size |B|:

```text
k, t     = ceil(S / 2), min(S, 2k)
seal     = random                                                8 bytes
shards   = RS(k, 2k).Split+Encode(B)                             2k shards of ceil(|B| / k) bytes
header   = blob id (32) || seal (8) || k (1) || index (1) || log2 |B| (1)
tag      = gen (4) || nonce1 (24) || XChaCha20-Poly1305(piece key, nonce1, header, aad = gen)
piece    = 0x02 || gen (4) || nonce2 (24) || XChaCha20-Poly1305(piece key, nonce2, shards[index], aad = tag)
piece id = HMAC-SHA256(piece id key of the storage citizen, blob id (32) || seal (8) || index (1))
rank     = HMAC-SHA256(placement key, blob id (32) || key of the storage citizen (32))
```

- S MUST be the count of storage citizens that the storage list names
  `active`, less each one whose app the device knows has no `pieces`. A
  storage citizen that did not answer still counts. With no storage list,
  each install counts.
- The code MUST be the Reed-Solomon code of `github.com/klauspost/reedsolomon`
  v1.14.2, `reedsolomon.New(k, k)`, with its default matrix. Shards 0 to
  k-1 hold B, with zeros at the end. The device puts shards 0 to t-1. A
  blob thus has at most 2k pieces, and a new index needs no new coding of
  the other pieces.
- With k = 1, each piece holds the whole blob. Erasure coding starts at
  three storage citizens.
- A blob keeps its k and its seal for its life. Each piece of one blob
  MUST have the same seal, k and blob size.
- A tag is 87 bytes. A piece is `ceil(|B| / k) + 45` bytes.
- For generation g, the keys MUST be, with no salt and 32 bytes each:
  - `piece key = HKDF-SHA256(data root of g, info = "arc-storage-piece-v1")`,
  - `piece id key = HKDF-SHA256(keyed root, info = "arc-storage-piece-id-v1" || key of the storage citizen)`,
    with the key as its 32 bytes,
  - `placement key = HKDF-SHA256(keyed root, info = "arc-storage-place-v1")`.
- The device MUST seal each new piece with the newest data root that it
  holds, as it seals a blob.
- The blob ID, the seal and the index go into the piece ID as 41 bytes.
  Each storage citizen has its own piece id key, so two storage citizens
  show no common piece ID. This replaces decision 8 of the storage
  proposal for pieces: a device never copies a piece between storage
  citizens under one ID.
- A new seal of a blob thus has new piece IDs. Its puts never meet a piece
  or a tombstone of an older seal of the blob on the same storage citizen
  (section 4.6).
- **Placement.** The device orders the storage citizens of a blob by
  `rank`, highest first. Index i of a new blob goes to the i-th storage
  citizen in that order that is active, has `pieces`, and has an open pipe.
  A missing index goes to the highest such storage citizen that holds no
  piece of the blob.
- **The piece map.** Each device builds the map of the pieces from the
  listings of each storage citizen: it opens each tag. It keeps the
  listings, with the tags, in its state file. No device writes a shared
  map.

Reason: a storage citizen holds coded ciphertext of half a blob, not a
whole copy. Each piece opens alone, so a device names a bad piece with no
trial decode. The tag and the piece of a new generation stay closed to a
revoked device.

## 4. Behavior

### 4.1 One sync

`arc sync`, the start of `arc serve`, and each 15 minutes of `arc serve` run
these steps. A write never starts a sync, because a direct path takes
seconds to open.

0. Follow the storage list (section 4.3). The signer removes each entry
   that is `gone` for 30 days or more, publishes the list, and prints
   `storage list: removed <name>, gone since <date>`.
1. On a signer with a device list, send the grant set to each installed
   storage citizen (section 3.2). A failure is a note, and the sync goes on.

Then the device syncs with each installed storage citizen that is not
`gone`, all at the same time, each on its own pipe. A storage citizen that
fails does not stop the others.

2. Open a pipe to each storage citizen (section 3.2). If no pipe opens,
   keep the silence record (below). The device still uses the last listing
   of that storage citizen. On each pipe to a `draining` storage citizen,
   send `drain` before the list of step 3. The app then refuses each put
   of the account, also from an older device. If the hello names no
   `drain`, print
   `<name> runs a storage app from before drain requests: it still takes a put from an older device; update it`,
   and go on. On the signer, for each pipe to an `active` storage
   citizen, send `usage`. If the state is `draining`, send `drain` with
   `off`, and print
   `<name>: the storage list names it active, so its drain ended`.
   Only the signer ends a drain: another device can hold an older list.
3. Send `hello` on each pipe, and keep if the app has `pieces`. List from
   the cursor of each storage citizen, and keep each ID and its tag.
   In a sync that repairs, audit each storage citizen (section 4.6).
4. Build the piece map (section 3.10). For each blob that the store does not
   hold:
   - If a storage citizen with an open pipe lists a blob of format 1, get
     it from one of them.
   - Else take the seal with the most pieces on open pipes, and at least k.
     Get k of its pieces, from the storage citizens that rank highest, all
     at the same time. Check each piece (section 3.5). If a piece fails,
     get another piece of the seal. Rebuild the blob from k shards. A
     piece that does not open, or that the app answers `not_found`, is
     lost (section 4.6).
   - Open the blob, check it (section 3.5), and give the record to the
     store. The store applies a deletion, and refuses a record that a
     deletion names.
   - With fewer than k pieces on open pipes, the blob waits for the next
     sync, and the sync fails.
5. Read the store again. For each record:
   - Put a blob of format 1 on each active storage citizen without
     `pieces` that does not list the blob ID.
   - If a storage citizen lists a blob of format 1 with the ID, put no
     piece. If the storage citizen has `pieces` or drains, repair codes
     the blob into pieces (section 4.6).
   - If no storage citizen lists a piece of the blob, seal the blob, code
     it with a new seal, and put t pieces (section 3.10). If fewer than k
     active storage citizens with `pieces` have an open pipe, put nothing:
     the record waits, and the sync fails. With k or more but fewer than t,
     put what can go. A later sync adds the rest.
   - If a storage citizen lists a piece of the blob, also a lost piece,
     leave the blob to repair (section 4.6).
   Puts go to the storage citizens at the same time. A `draining` storage
   citizen gets no new blob.
6. In a sync that repairs, repair each blob (section 4.6). Else count the
   blobs that need repair.
7. Keep the cursor, the IDs, the tags, the feature `pieces`, the lost
   pieces, the kept proofs and the record of the audits in
   `<home>/selfsync/<key of the storage citizen>.json`, and the blob IDs
   that the device got in `<home>/selfsync/seen.json`.

After the sync, the device prints one line for each storage citizen, in
the order of the installs, and one line for the whole sync. `received` is
the count of pieces and blobs that the device got from that storage
citizen, `sent` the count that it stored, and `already held` the count of
puts that it answered `held`:

```text
charlie: self data received 0, sent 3, already held 0 in 1.36s
dana: self data received 2, sent 0, already held 1 in 950ms; draining, so nothing new goes to it
self data: took 2 records, already held 0, coded 3 new records into pieces, 2 of 3 rebuild each
```

A storage citizen that fails gets `<name>: self data: <reason>` on
standard error, and `arc sync` ends with `some transports failed`. The
other lines still print.

The silence record is two fields of the same file. `answered` is the Unix
time of the last pipe that opened. `silent_since` is the Unix time of the
first failed open after it, and is absent while the storage citizen
answers. If no pipe opens and `silent_since` is 7 days old or more, the
device prints
`<name>: no answer for <days> days; if it is gone, run arc keys storage rm <name> --gone on the signer`.
It prints this at each sync. Only the user marks a silent storage citizen `gone`.
The count starts at the first failed open, not at the last answer. Thus a
device that was off itself for 7 days does not warn at its first sync.

Steps 4, 5 and 6 keep requests in flight on a storage app with `batch`
(section 3.3): step 4 rebuilds 256 blobs at a time, step 5 seals 64
records at a time and keeps 32 puts in flight on each pipe, and step 6
repairs 256 blobs at a time. Steps 4, 5 and 6 run in this order. A device
that was offline thus gets a deletion before it could put the old records
back.

### 4.2 The import

The first `arc sync` after an update gets every event of self data of the
citizen from each relay, and keeps it in the local store. It then marks the
import as done in `<home>/selfsync/imported`. Step 5 of the next sync puts
the imported records on the storage citizen. `arc sync --import` runs the
import again. A second import takes nothing new, because the store keeps each
event once.

If a relay does not answer, the import is not marked, and the next sync runs
it again.

The import never keeps a deletion with the tag `["arc", "moved"]`. The
local store refuses one too (section 4.2.1).

#### 4.2.1 The removal of the old drafts from the relays

After the sync with the storage citizens, the device deletes the old drafts
from its relays. These rules apply:

- The device does this only after the import is marked done.
- The device asks each relay for its NIP-11 document. It sends a deletion
  only to a relay whose `tags` hold `arc-drafts-retired`. An ARC relay of
  this version has the tag. Section 7 says why another relay gets no
  deletion.
- The device lists the events of kinds 31234, 1234, 3275 and 10013 of the
  citizen on each such relay. It does not delete the draft with the d tag
  `arc-keyed-root`: a device that signs through a bunker reads its keyed
  root from it.
- An event is movable when two conditions are true. The local store holds
  the event, a newer version of its d tag, or a deletion that names it. The
  storage citizens hold that record of the store.
- The storage citizens hold a record when their last listings show k pieces
  of one seal of its blob, or a whole blob of format 1. A put that a storage
  citizen answers `held` counts at the next listing.
- A private relay list, kind 10013, is always movable. It holds no self
  data.
- The device sends one NIP-09 deletion, kind 5, for each 100 movable events
  or fewer. The deletion names each event in an `e` tag, names their kinds
  in `k` tags, and has the tag `["arc", "moved"]`. The signer signs it as
  the citizen.
- The deletion goes to the relay only. A store never keeps a deletion with
  the tag `["arc", "moved"]`: there the deletion deletes the drafts that it
  names, and the sync then takes them from each device.
- An event that is not movable stays. The next sync tries again.
- When each relay answered, and each relay with the tag holds no event that
  the device can delete, the device writes `<home>/selfsync/moved`. Later
  syncs then do not ask the relays. `arc sync --import` removes the marker.
- A bunker that runs with `--allow-kind` signs the deletion only if it
  allows kind 5. If it does not, the drafts stay, and each sync says why.

`arc sync` prints `<relay>: deleted <n> old drafts and private relay lists
from the relay`, and `<relay>: <n> drafts stay until the storage citizens
hold them` for the events that are not movable.

### 4.3 How a device finds its storage citizen

Each install of the app `storage` is a storage citizen of the citizen. At
the start of each sync, a device reads the newest storage list from its
store and the relays of the citizen (section 3.8). A machine that signs
through a remote signer opens the list there, with `--decrypt self`.

1. For each entry that is `active` or `draining` and that no install of the
   app `storage` names by its key, the device installs the storage app of
   the entry under `name`, and prints `installed <name> from the storage list`.
   If another app has the name, the device prints a note, and skips the
   entry.
2. For each entry that is `gone`, the device removes the install of the
   storage app of that key, and prints
   `removed <name>: the storage list says it is gone`.
3. A local install of `storage` that the list does not name still syncs.
   The device prints
   `<name> is not in the storage list; run arc keys storage add on the signer`.
4. If no storage list exists, the device changes no install and prints
   nothing. It syncs with its local installs.
5. If the list does not come, and the store holds none, the device prints a
   note, and syncs with its local installs.

The signer is a device too, so its own sync follows the list.

A machine with only the seed derives the citizen key with
`arc keys recover`. With the relay of the citizen and the app `journal`
installed, its first `arc sync` reads the storage list, installs each
storage citizen, derives each account key, and syncs.

The signer changes the list:

| Command | What it does |
| --- | --- |
| `arc keys storage add <link or key> [--as <name>]` | After `admitted`, puts the entry as `active`, and publishes the list. Then it sends the grant set (section 4.4). An entry with the same key is replaced, so a storage citizen that the citizen left can come back. |
| `arc keys storage ls` | Prints each entry: name, state, key, `since` and relays. |
| `arc keys storage rm <name or key>` | Sets `draining`, and publishes the list. The sync of the signer sets `gone` when the storage citizen is empty (section 4.6). |
| `arc keys storage rm <name or key> --gone` | Sets `gone`, and publishes the list. |

A device that runs `add` or `rm` gets `only the signer, which holds the
citizen key, changes the device list and the storage list`. `since` changes only with `state`.
`add` and `rm` also remove each entry that is `gone` for 30 days or more.

### 4.4 Accounts and grants

1. The operator makes an invite link with `arc-storage invite`, and gives
   it to the citizen. Under the rule `ask` or `open`, the key of the storage
   citizen is enough.
2. The citizen runs `arc keys storage add <link or key>` on the signer. The
   signer sends the join (section 3.2).
   - On `admitted`, it prints the quota and the plan, installs the storage
     app of the storage citizen if it is not installed, puts the storage
     citizen in the storage list as `active`, and sends the grant set if a
     device list exists.
   - On `pending`, it prints the message and installs nothing. The citizen
     runs the command again after the operator decides.
   - On `refused`, it fails with the message.
3. Each other device installs the storage citizen from the storage list at
   its next sync (section 4.3).
4. An operator can still admit an account by hand: the citizen runs
   `arc keys account <key of the storage citizen>` on the signer, and the
   operator puts the account key in `STORAGE_OWNERS`, or runs
   `arc-storage accept <account key>`.
5. `arc keys devices add` and `arc keys devices revoke` publish the device
   list, and then send the grant set to each storage citizen that the signer
   installed, and to each storage citizen that the storage list names as
   `active` or `draining`. Each line `grants: sent to the storage citizen <name>` names a
   storage citizen that took the set.
6. `arc sync` on the signer sends the grant set again (section 4.1). A
   revocation that did not reach a storage citizen thus reaches it on the
   next sync of the signer.

The signer knows a storage citizen if it installed it, or if the storage
list names it. A storage citizen that neither names gets no grant set, and
refuses each device of the citizen.

### 4.5 The commands of the operator

The operator runs these commands on the machine of the storage citizen.
They change the files in `STORAGE_DIR` under the locks of the app, so they
run while the app runs. Each change counts at the next call.

| Command | What it does |
| --- | --- |
| `arc-storage invite [-quota 1GiB] [-plan free]` | Makes a token, and prints the invite link. `arc whoami` gives the key, or `-key`. |
| `arc-storage requests` | Prints each pending request: the account key, `want`, the time, the citizen key or `-`, and the note. |
| `arc-storage accept <account> [-quota 1GiB] [-plan free]` | Admits the account. It need not have asked. |
| `arc-storage refuse <account>` | Refuses the request. The next join gets `refused`. |
| `arc-storage account set <account> [-quota] [-plan] [-expires]` | Changes the record of an admitted account. |
| `arc-storage account close <account>` | Makes the account `closing`. |

### 4.6 Audits and repair

Only a sync that repairs audits, rebuilds, moves or deletes a piece. Each
sync of `arc serve` repairs: at its start and on each tick. `arc sync`
repairs only with `--repair`. Without it, `arc sync` counts the blobs that
need repair, and prints
`self data: <n> blobs need repair; arc serve repairs them on its tick, or run arc sync --repair`.

Reason: decision 4 of
[the storage network proposal](../proposals/storage-network.md). A phone
repairs only on request. Two devices that repair at the same time cost at
most one extra piece, which the next repair deletes.

**TURN.** Repair and audits use only direct pipes. A pipe through TURN
gives no piece to repair, gets no repair put and no audit. If the repair of
a blob needs a pipe through TURN, the device prints
`self data: the repair of <n> blobs waits for a direct path: the pipe to <names> goes through TURN; arc sync --repair --turn allows TURN`.
`--turn` lets repair and audits use TURN. `--turn` with no `--repair` is an
error. Reason: decision 7 of the proposal. A rebuild of 1 GiB needs more
than the TURN quota of one day.

The sync opens each pipe with the path of the citizen (`arc path`): `turn`
uses only the TURN server of the relay, and `direct` uses no TURN. `auto`
and `session` try a direct path first.

**Decisions.** Zim accepted these on 2026-10-12, with the build of this
section:

1. `arc path` applies to the sync of self data: `turn` opens each pipe to a
   storage citizen through TURN only, and `direct` uses no TURN.
2. A plain `arc sync` does not add a piece for a late or new storage
   citizen. Only a sync that repairs adds it: `arc serve`, or
   `arc sync --repair`. Reason: the piece costs k pieces down, as a
   rebuild does (decision 4 of the proposal).
3. Audits wait for a direct path, as repair does. `--turn` allows both.

Zim accepted these on 2026-10-12, with the move of blobs of format 1:

4. Only a sync that repairs moves blobs of format 1 to pieces: `arc serve`,
   or `arc sync --repair`. A plain `arc sync` counts them.
5. One sync moves at most 1,024 blobs of format 1.
6. The device deletes the whole copies of a blob only when k indices of
   its seal are on active storage citizens that listed in this sync, or
   come with the puts of the plan, and only after each put of the plan
   answered `stored` or `held`.

**Audits.** In a sync that repairs, the device audits each storage citizen
with a pipe that repair can use, once in 24 hours:

1. Pick 32 random pieces that the storage citizen lists with a tag, and
   that are not lost.
2. For a piece with a kept proof, send `audit` with the nonce of that proof,
   one piece in each request (section 3.9). The piece passes if the app
   gives the kept proof. Then forget the kept proof.
3. Get each other piece, and open it with its tag. If it opens, it passes,
   and the device keeps a new proof for it.
4. A piece that the app answers `not_found`, or whose proof differs, or
   that does not open, is lost.

A kept proof is a new random nonce of 32 bytes, and the proof of the bytes
of the piece for that nonce. The device makes it while it holds the bytes:
after a put that the app stored, and after a get in which the piece opened.
The nonce stays secret on the device until the audit sends it. Before the
device sends the nonce, it forgets the kept proof, and saves its state
file. Thus no nonce goes out two times, also after a crash. The first audit
of a piece moves 32 bytes each way, and the next audit gets the piece. A
kept proof adds 88 bytes of base64 for each piece to the state file, beside
the 116 bytes of the tag. The device also marks a piece as lost if a get of step 4 of
section 4.1 gives `not_found` or a piece that does not open.

After an audit, the device prints `<name>: audit checked <n> pieces, <m>
failed`. It counts the lost pieces of each storage citizen for each day of
UTC. With 3 or more on one day, each sync of that day prints
`<name> is suspect: <n> pieces failed the audits of today; repair replaces them; if it is gone, run arc keys storage rm <name> --gone on the signer`.
The device does not drop the storage citizen. Only the user marks a suspect storage citizen gone
(decision 10 of the proposal).

**The plan of one blob.** Repair looks at each blob that a storage citizen
lists a piece of, also a lost piece, and at each blob of format 1 (below).
It skips a blob that the same sync coded. S is as in section 3.10. A writable storage
citizen is active, has `pieces`, and has a pipe that repair can use. A lost
piece counts for no seal. The device keeps the seal of step 4 of section
4.1. Then:

1. If no seal has k pieces (rule 2 of section 6.7 of the proposal), or if
   S is less than the k of the seal, seal again (below).
2. Else let want be min(S, 2k). An index is held if an active storage
   citizen lists a piece of it. A free storage citizen is writable and
   lists no piece of the blob. The device takes free storage citizens in
   the rank of the blob, highest first.
3. **Drain.** For each index that only a draining storage citizen holds:
   get the piece, check that it opens, and put the same bytes and the same
   tag on the next free storage citizen, under its own piece ID. Stop when
   want indices are held.
4. **Rebuild.** For each other index from 0 that no active storage citizen
   holds: make the piece, and put it on the next free storage citizen.
   Stop when want indices are held. The device gets k pieces of the seal,
   and rebuilds the blob as step 4 of section 4.1 says. With k = 1 and the
   record in the store, it seals the record again, and gets nothing.
5. A free storage citizen does not get an index whose piece ID it listed
   before: its tombstone refuses the put.
6. If fewer than want indices are held, no storage citizen is free, and a
   lost piece is on a writable storage citizen that holds no other piece
   of the seal, seal again. Reason: the same index of the same seal on the
   same storage citizen has the same piece ID. A put of it gets `held`, and
   a put after a delete gets `tombstoned`.
7. Delete each lost piece. When want indices are held, also delete each
   piece on a draining storage citizen, the second piece of an index on
   the storage citizen of the lower rank, and each piece of another seal.

**Seal again.** The device needs the record in its store, or k pieces of
the seal. It needs k writable storage citizens for the k of S. It codes the
blob with a new seal and the code of S, and puts t pieces as for a new
record. The new seal has new piece IDs (section 3.10), so no put gets
`held` or `tombstoned`.

**Blobs of format 1.** A whole copy of format 1 moves to pieces, except
on an active storage citizen whose app has no `pieces`: that storage
citizen keeps whole copies. Thus the move also empties a draining storage
citizen. For a blob with such copies:

1. If a seal of the blob has k pieces, the plan of the seal applies. If k
   indices of the seal are on active storage citizens that listed in this
   sync, or come with the puts of the plan, delete each whole copy.
2. Else seal again (below), and delete each whole copy after the puts.
   The device seals the record of its store again, and gets nothing. If
   the store does not hold the record, the device gets a whole copy, and
   codes it only if its size is a blob size and it opens under its ID
   with the data root of its generation. If a copy does not open, the
   device asks the next storage citizen that holds a copy.
3. The device deletes only the copies on storage citizens with a pipe
   that repair can use. A copy on another storage citizen waits for the
   next repair.

A blob whose only whole copies are on active storage citizens with no
`pieces` stays whole, and gets no pieces.

One sync moves at most 1,024 blobs of format 1, after each other blob of
the plan. Each sync, also one that does not repair, prints the count of
blobs that still have a whole copy to move:
`self data: <n> blobs of format 1 remain; repair codes them into pieces`.

Reason: a move puts up to 2 times the bytes of a blob (section 6.4 of the
proposal), so one tick of `arc serve` does a bounded part of a large home.
The whole copies serve until the pieces hold. A move that stops loses
nothing: before its puts hold, the whole copies stay; after them, the next
repair finds a seal with k pieces and deletes the copies. The move needs k
writable storage citizens, not two with `pieces` as the proposal said: with
one active storage citizen, the drain of another one still moves.

**Order and deletes.** The blobs with the fewest spare pieces go first: the
pieces that the blob can still lose before it cannot be read. A blob that
needs a new seal goes first. The device deletes the pieces of a blob only
after each put of its plan answered `stored` or `held`. If a put fails, the
old pieces still serve, and the next repair tries again.

**Draining and gone.** A draining storage citizen gets no new blob, and
repair moves its pieces. It is empty when the device listed it to the end
in this sync, and it lists no piece and no blob. The drain request of step
2 of section 4.1 comes before the list. Then:

1. If the drain request answered `draining`, `closing` or `closed`, the
   account takes no put. On the signer, the device sends `usage`. If the
   usage counts 0 blobs, the signer sets `gone` and publishes the list. It
   prints `<name> holds no piece now and takes no put, so the storage list marks it gone`
   and `storage list: <name> is gone; each device removes its install at its next sync`.
   If the usage counts blobs, the entry stays `draining`, and the sync
   says so.
2. On another device, the sync prints
   `<name> holds no piece now; the signer marks it gone at its next sync`.
3. If the app has no `drain`, the sync prints
   `<name> holds no piece now; run arc keys storage rm <name> --gone on the signer`.
   An older device can still put a blob there, so the signer does not set
   `gone` by itself.

The drain ends only at a sync of the signer: `arc sync` or `arc serve` on
the machine that holds the citizen key. A device does not tell the signer
that a storage citizen is empty; the signer lists it itself. Reason: a
device cannot sign the storage list, and a new message would need a new
event kind. A storage citizen that does not answer, or that fails its
audits, stays as it is: only the user marks it gone (decision 10 of the
proposal).

A storage citizen that the storage list names `gone` is not in the sync.
Its indices are then not held, and step 4 rebuilds them on a free storage
citizen.

After a sync that repairs, the device prints one line:

```text
self data: repair rebuilt 9 pieces, moved 0, sealed 0 records again, coded 0 blobs of format 1 into pieces, deleted 0; 37674 bytes down, 18837 bytes up
```

`coded` counts the blobs whose whole copies the repair deleted. The bytes
are those of the pieces and whole copies that repair got and put. A move costs
one piece down and one piece up. A rebuild costs k pieces down and one
piece up. These are the costs of section 6.10 of the proposal. Measured in
`TestStorageRepair`, with 9 blobs of 4 KiB, k = 2 and pieces of 2,093
bytes: the drain moved 18,837 bytes down and 18,837 bytes up; the rebuild of
the pieces of a gone storage citizen moved 37,674 bytes down and 18,837
bytes up.

## 5. Failures

| Cause | What the device does | What the user sees |
| --- | --- | --- |
| No storage citizen is installed | The data stays on the device. | `self data stays on this machine: no storage citizen; install one with arc install <key> storage` |
| The storage citizen refuses the caller: the account key is not admitted, or the device key is in no grant set | No pipe opens. | `the storage citizen refused this machine: its key is neither an account key in STORAGE_OWNERS nor a device key with a grant; the signer sends grants with arc keys devices add and arc sync` |
| A join is refused | `arc keys storage add` installs nothing. | `<name> refused the account: <message>` |
| A join is pending | `arc keys storage add` installs nothing. | `<name>: pending: the operator decides by hand; ask again later` |
| The storage citizen runs a storage app from before admission | It answers `invalid_request` to the join. | `<name> runs an older storage app: it cannot admit accounts; use STORAGE_OWNERS` |
| A put on a closing account | The sync stops, as for `over_quota`. | `the account is closing: it takes no put, and its reads end at <day>` |
| A put on a draining account | The put is refused, as for a closing account. | `the account is draining: it takes no put, and keeps each read, list and delete` |
| A draining storage citizen runs a storage app from before drain requests | The sync sends no drain request, and goes on. The signer does not set `gone` by itself. | `<name> runs a storage app from before drain requests: it still takes a put from an older device; update it` |
| The drain request fails on an open pipe | The sync with that storage citizen fails. The next sync sends it again. | `<name>: self data: the drain request failed: <reason>` |
| The storage list does not publish when the signer sets `gone` | The entry stays `draining`. The next sync of the signer tries again. | `self data: the storage list did not change, so <names> stays draining: <reason>` |
| An audit fails | `arc-storage audit` prints `fail` or `lost` for the blob, and ends with an error. | `<n> of <m> blobs failed the audit` |
| The storage citizen runs a storage app from before audits | It answers `invalid_request` to the audit and the drain, and its hello names no `audit` and no `drain`. The client does not send them. | `the storage citizen runs an older storage app; it must update` |
| A grant set does not reach a storage citizen | `arc keys devices add` or `revoke` fails after it publishes the device list. The storage citizen keeps the older set until the next sync of the signer. | `the device list changed, but <name>: the grants did not reach the storage citizen: <reason>; run arc sync on the signer to try again` |
| A device signs through a bunker, and the citizen has no device list | No grant set exists, so the storage citizen refuses the device key. | The refusal above. Add the device on the signer with `arc keys devices add`. |
| The storage list names a storage citizen whose app is not found | The device skips that entry. The other entries sync. | `<name>: the storage list names it, but its storage app was not found: <reason>` |
| A local install has the name of a list entry | The device skips that entry. | `<name>: the storage list names it, but this machine uses the name for <app> of <key>; remove that install to sync with it` |
| A local storage install is not in the storage list | It still syncs. | `<name> is not in the storage list; run arc keys storage add on the signer` |
| The storage list does not come, and the store holds none | The device syncs with its local installs. | `self data: the storage list did not come, so this machine syncs with its own installs: <reason>` |
| A device asks the signer to sign kind 10273 | The device refuses first. A device that skips that check gets the refusal of the signer. | `only the signer, which holds the citizen key, changes the device list and the storage list`, or `the bunker never signs a storage list; run arc keys storage on the signer` |
| A grant set is older than the set of the account | The storage citizen keeps its set. | `stale_grants` |
| No path opens | The sync of that storage citizen fails. The other storage citizens still sync at the same time. | The error of the direct pipe, and `some transports failed`. |
| No pipe opens to a storage citizen for 7 days | Each sync of the device warns. The device does not mark it `gone`. | `<name>: no answer for <days> days; if it is gone, run arc keys storage rm <name> --gone on the signer` |
| A blob does not pass section 3.5 | The device skips it. | `refused <id>: <reason>` |
| A piece does not pass section 3.5 | The device gets another piece of the seal. | `refused <name>: piece <id> of blob <blob>: the piece does not open; the device asks another storage citizen` |
| Fewer than k pieces of a blob answer | The blob waits for the next sync. The sync fails. | `self data: <n> blobs cannot be read now: fewer of their pieces answered than rebuild them; start the storage citizens that are stopped, and sync again`, and `unreadable <blob>: 1 of the 2 pieces that rebuild it answered` for each |
| Fewer than k storage citizens answer for a new record | The record waits. The sync fails. | `self data: <n> records wait: a new blob needs <k> storage citizens that answer; the next sync puts them` |
| No seal of a blob has k pieces | The device does nothing. | `self data: <n> blobs have fewer than k pieces of one seal on the storage citizens; a later step repairs them` |
| A storage citizen runs a storage app without `pieces` | It gets whole blobs of format 1, and does not count in S. | `<name> runs an older storage app: it holds full copies; update it for erasure coding` |
| A put passes the quota | The sync stops. The cursor and the IDs so far are kept. | The `over_quota` message, with the bytes in use and the quota. |
| A relay does not answer during the import | The import is not marked. | `the import of the drafts on the relays is not complete` |
| A relay has no tag `arc-drafts-retired` | The device sends it no deletion. The drafts stay on it. | `<relay>: an older or another relay; the old drafts stay on it, because it would serve their deletion to older devices` |
| A relay does not give its NIP-11 document, or does not list the drafts | The device deletes nothing there, and does not write the marker. | `<relay>: the relay did not say if it hides deletions, so no draft was deleted: <reason>`, or `<relay>: the drafts on the relay were not listed, so none were deleted: <reason>` |
| The signer does not sign a deletion | The drafts stay. The next sync tries again. | `<relay>: the deletion of <n> drafts was not signed: <reason>` |
| A relay refuses a deletion | The drafts stay. The next sync tries again. | `<relay>: the relay did not take the deletion of <n> drafts: <reason>` |
| An older client sends a draft to an ARC relay | The relay refuses it. | `blocked: this relay keeps no drafts; arc keeps them on the device and syncs them with a storage citizen; update arc` |

## 6. Security

- The storage citizen sees the account key, each device key of the grant
  set, the count of the pieces, their sizes, the times of the puts and
  the sessions, and the IP address of each machine. The size of a piece is
  the padded size of its blob divided by k, plus 45 bytes, so it shows k
  roughly. A storage citizen does not see the blob IDs, the tags, which
  pieces form one blob, or the other storage citizens.
- Each storage citizen has its own piece id key, so two storage citizens
  that work together show no common piece ID. They can still join pieces
  by the time of the puts, their sizes and the IP address.
- k storage citizens that work together hold coded ciphertext of the
  blob. They still need the data root to open it.
- A storage citizen that changes a piece, moves a tag to another ID, or
  gives a piece of another blob, gets refused by section 3.5. The device
  rebuilds the blob from other pieces. It does not see the
  citizen key, the device list, the names of the devices, the kinds, the `d`
  tags, the content, or which blob is a page.
- A revoked device that skips the remote signer, and calls the storage
  citizen with its device key, gets `not_owner` and no pipe, once the newest
  grant set reached the storage citizen. Until then it can still read and
  put blobs. It cannot open a blob of a newer data root.
- A device never needs the remote signer to decrypt a reply of the storage
  citizen, so the remote signer keeps `--decrypt self`.
- A stolen account key opens the account and sets its grants. It does not
  give the citizen key, nor the account of another storage citizen.
- A revoked device gets no data root of the generation that the signer
  makes at the revocation. It cannot open a blob that another device seals
  after the revocation, also if a storage citizen gives the blob to it.
- A revoked device keeps each data root that it held, and the keyed root. It
  can still open each blob of generation 0 and of each generation that it
  held. No device seals these blobs again.
- In generation 0, each holder of the citizen key derives the blob key. A
  device that knows the keyed root opens each blob of generation 0.
- With no device list, the AEAD is the only check of who made a blob: only a
  holder of the keyed root makes one that opens. With a list, each device
  checks the device key of each change.
- The storage list is sealed to the citizen key. A relay serves it only to
  the citizen. The relay sees its size and its time, not the storage
  citizens. A storage citizen does not see the list.
- A device cannot change the storage list: it holds no citizen key, and the
  signer refuses to sign kind 10273 for a device. A device that lost its
  place on the device list cannot add a storage citizen.
- A hostile storage citizen can withhold or delete blobs and pieces. A
  device keeps its full copy. It cannot change or make a blob or a piece
  that opens. With t - k storage citizens that withhold, each blob still
  rebuilds.
- An audit proves that the storage citizen read each byte of the blob after
  the nonce came. A storage citizen that keeps the blob only on another
  machine, and fetches it for each audit, still passes. The proposal
  accepts this for a friend or the own machine of the user (section 6.9).
- The audit shows the storage citizen which blobs a device checks, and
  when. It learns no content.
- An invite token carries the consent of the operator. Whoever holds the
  link can spend it once, in its 7 days. The operator gives it by hand. The
  storage citizen keeps only the SHA-256 of each token.
- A join shows the citizen key only by choice. Then the storage citizen
  can join the account key to the citizen key. It still cannot derive the
  account key of another storage citizen.
- The storage citizen pays for the disk. The quota of each account limits
  it. Under `open`, `STORAGE_CAPACITY` limits the sum of the quotas. The TURN quota of the citizen pays for a pipe that goes through the
  relay.

## 7. Compatibility

- An ARC relay of this version refuses new events of kinds 31234, 1234,
  3275 and 10013. The one exception is the draft with the d tag
  `arc-keyed-root`. The reason is `blocked: this relay keeps no drafts; arc
  keeps them on the device and syncs them with a storage citizen; update
  arc`. The relay still serves the old events of these kinds to their
  author. This is a breaking change for an older client:
  - An older client still writes drafts to its relays. The relay refuses
    each one. The draft stays in the local store of the older client, and
    its `arc sync` reports it as not sent. The older client reads only from
    its relays, so it does not see the pages of a newer client.
  - An older client publishes kind 10013 with `arc relay add`. The relay
    refuses it, and the older client prints `the relay list did not reach`.
  - A newer client still reads drafts from the relays when it queries self
    data, so it sees the old pages of an older client.
  - The user updates each device.
- A newer client deletes the drafts on its ARC relays after a storage
  citizen holds them (section 4.2.1). An older client of the same citizen
  that has not imported its drafts then does not find them on the relay.
  It finds them on the storage citizen after the user updates it.
- The proposal keeps old drafts on the relays for reads for 90 days after
  the breaking release (proposal, decision 6). The breaking release is
  v0.25.0, of 2026-10-12. Thus the relay serves the old drafts to their
  author until 2027-01-10, unless the author deletes them first. The
  proposal refuses writes from the breaking release, and keeps only reads
  (proposal, section 3.3, step 2). This step therefore refuses writes now.
- After 2027-01-10 the operator removes each event of kinds 31234, 1234,
  3275 and 10013, except the drafts with the d tag `arc-keyed-root`.
  `arc relay serve` has no command for this. That is not built (section
  8.2).
- The relay applies a deletion with the tag `["arc", "moved"]`, and then
  serves it to nobody, also not to its author. A v0.25.0 client keeps each
  deletion of its drafts that its import gets, and its store applies it.
  That client would then delete its own drafts, and its sync would send the
  deletion to the storage citizens and so to each device. For the same
  reason a newer store refuses such a deletion, also one that comes from a
  storage citizen or a stick.
- A newer client sends no deletion to a relay without the tag
  `arc-drafts-retired`: an older ARC relay, or a relay that is not an ARC
  relay. Such a relay would serve the deletion to a v0.25.0 device. The old
  drafts stay on that relay. A relay that is not an ARC relay serves them to
  anyone, as before. After the operator updates an older ARC relay, the
  user runs `arc sync --import` on one device, and that device deletes the
  drafts.
- Update the relays before the devices. A v0.25.0 device whose store took a
  moved deletion from an older relay refuses the drafts that it names, also
  after an update. No newer client sends a deletion to an older relay, so
  this does not happen through this version.
- An ARC relay of this version serves a deletion whose `k` tag names a
  sealed kind only to its author, because its `e` tags count the drafts.
  An older ARC relay serves such a deletion to anyone. A query of kind 5
  alone does not authenticate, so it gets no deletion of drafts from an ARC
  relay of this version. The import asks with the draft kinds, and gets
  them.
- The private relay list, kind 10013, is retired. A newer client does not
  publish it. A newer client still lists kind 10013 on its relays, to delete
  the old ones.
- The blob format names its generation. A device that knows only generation
  0 refuses a blob of a later generation, and says to update arc. Each
  device of a citizen with a device list seals new blobs with generation 1
  or later. Thus an older device does not get the changes of a newer
  device. A newer device still opens each blob that an older device sealed,
  because those blobs are of generation 0. The user updates each device.
- The storage list adds kind 10273. It changes no earlier message:
  - An older device does not read kind 10273. It syncs with its local
    installs, as before. The user installs each storage citizen on it with
    `arc install <key> storage`, or updates it.
  - An older signer publishes no storage list. A newer device then finds no
    list, changes no install, and syncs with its local installs. Its
    `arc keys storage add` installs and grants, as before.
  - A newer signer that adds a storage citizen publishes a list with that
    one entry. A storage citizen that the signer installed before the list
    is not in it. Each sync of a newer device names it, and the user runs
    `arc keys storage add` for it once.
  - An older ARC relay, or a relay that is not an ARC relay, serves kind
    10273 to anyone who asks. The content stays sealed to the citizen key.
    Such a relay shows that the citizen has a list, its size and its time.
  - An older bunker signs kind 10273 for a device that asks. A newer device
    does not ask. The user updates the signer.
- The sync of all storage citizens at the same time changes no message:
  - An older device syncs its storage citizens one after another. A device
    with the storage list but before this step still puts blobs on a
    `draining` storage citizen. Both still read and write full copies, so
    a newer device reads what they put.
  - An older device ignores `answered` and `silent_since` in its state
    file, and drops them when it writes the file. A newer device then
    starts the count again at its next failed open.
- The pieces (section 3.10) change the bytes on each storage citizen. This
  is a breaking change:
  - An older device lists the piece IDs, and asks for each one. Its client
    refuses a get whose size is not a power of two, so its sync with that
    storage citizen fails each time with
    `the storage citizen announced a blob of 2093 bytes`. It then puts
    nothing there, and sees no record that a newer device wrote. The user
    updates each device.
  - An older device puts blobs of format 1 on a storage citizen that it
    syncs with. A newer device reads them. Its repair codes them into
    pieces, and deletes them (section 4.6).
  - A newer device sends `hello` first. An older storage app answers with
    no `pieces`, or with `invalid_request`. The device then puts only
    blobs of format 1 on it, does not count it in S, and prints
    `<name> runs an older storage app: it holds full copies; update it for erasure coding`.
  - A newer storage app gives `tag` in list entries, and `pieces` in
    `hello`. An older device ignores both.
  - The state file of each storage citizen has `"v":2`, the tags and the
    feature. A device lists each storage citizen again from 0 if the file
    has no `v`: an older device kept no tags. An older device that writes
    the file drops these fields, and the next sync of a newer device lists
    from 0 again. The blob IDs that the device got move to `seen.json`.
  - The Go API changes: `Syncer.SyncAll` replaces `Syncer.Sync`, and the
    interface `Blobs` gains `Pieces`. `storage.Blob`, `storage.Entry` and
    `storage.Request` gain `Tag`. `sdk/storage` adds `Conn.Pieces`,
    `FeaturePieces`, `TagSize`, `PieceOverhead`, `MaxK`, `PieceSize` and
    `GoodPieceSize`.
  - The module requires `github.com/klauspost/reedsolomon` v1.14.2, which
    raises `github.com/klauspost/cpuid/v2` from v2.0.9 to v2.3.0. A newer
    version of the library can change the bytes of the parity shards.
    `TestParityPrefix` keeps the property of v1.14.2, and section 3.10 names
    the version.
- Audits and repair (section 4.6) put the seal in the piece ID (section
  3.10). This is a breaking change of the pieces:
  - The step of the pieces before it (commit 171daa4) named a piece with
    no seal. No release holds that step: v0.25.0 is older. A device of this
    version refuses each such piece with `the tag names another piece`, and
    codes its record again with a new seal. The older pieces stay on the
    storage citizen, and no device reads or deletes them. A device of
    commit 171daa4 refuses each piece of this version in the same way. The
    user updates each device.
  - An older device does not audit, repair, move or delete a piece. It still
    reads each blob whose seal has k pieces. It still puts nothing on a
    `draining` storage citizen. A newer device repairs what the older one
    leaves.
  - No message on the pipe changes. A storage app of commit 604b1af or
    later answers `audit`. With an older storage app, the device gets and
    opens each piece that it audits.
  - The state file of each storage citizen gains `lost`, `proofs`,
    `audited`, `audit_day` and `failed_today`. An older device drops them
    when it writes the file. A newer device then audits at its next repair,
    gets each piece that it audits, and finds a lost piece again in its
    audit or its read.
  - `arc sync` gains `--repair` and `--turn`. An older `arc` refuses both
    flags. `arc sync` of an older device ignores `arc path` for self data.
  - The Go API changes: `PieceID` takes the seal. The interface `Blobs`
    gains `DeleteMany` and `Audit`. `Syncer` gains `Repair`, `TURN` and
    `Now`. `Report.Mixed` goes, and `Report.Repair` comes. `SideReport`
    gains the fields of the audits and `Empty`. `storage.Client` gains
    `OnlyRelay`. `citizen.SelfSync` gains `Path`, `Repair` and
    `RepairTURN`.
- The move of blobs of format 1 to pieces (section 4.6) changes no
  message:
  - Each repair of a newer device codes the blobs of format 1 of each
    storage citizen with `pieces` into pieces, and deletes them. The blob
    IDs of format 1, which each storage citizen shared, go.
  - An older device lists the deletions. It does not put a record whose
    blob ID the storage citizen lists, also as deleted, so it puts no blob
    again. A put of a deleted blob ID gets `tombstoned` for 90 days.
  - An older device still cannot read a piece. When each blob it lacks is
    in pieces only, each of its syncs with that storage citizen fails with
    `the storage citizen announced a blob of <n> bytes`, and puts nothing.
    The user updates each device.
  - A storage citizen whose app has no `pieces` keeps its whole copies.
  - The repair line of `arc sync` gains `coded <n> blobs of format 1 into
    pieces`. `RepairReport` gains `Coded` and `Old`.
- `sdk/storage` holds the messages that `apps/storage/wire` held before. The
  bytes on the pipe did not change.
- Requests in flight add `hello`, `n` and `ids`. They change no earlier
  message:
  - An older device sends no `hello` and no `n`. It sends one request at a
    time. A newer storage app answers each request in order, with no `n`,
    as before.
  - A newer device sends `hello` to an older storage app. The older app
    answers `invalid_request`, because the request has no blob ID, and
    keeps the pipe open. The device then sends one request at a time, with
    no `n`, and `get` and `delete` with one `id` each.
  - The Go interface `Blobs` of `runtime/selfsync` has `GetMany` and
    `PutMany` in place of `Get` and `Put`. `*storage.Conn` has both.
- A newer storage app keeps the index of each account in `index.db`. At its
  start, it imports the entries of each older `state.json` once, and keeps
  the record and the grant set in `state.json`. The sequence numbers do not
  change, so the cursor of each device stays good. An older storage app
  cannot read `index.db`, and sees each account as empty after the import.
  The operator does not go back to an older storage app.
- Admission adds the join call and fields of the usage reply. It changes no
  earlier message:
  - An older storage citizen answers the join with `invalid_request`,
    because the body has no session. `arc keys storage add` then says that
    the storage citizen cannot admit accounts. Its operator uses
    `STORAGE_OWNERS`, as before. Its usage reply has no `state`, `plan` and
    `expires`, and its `quota` is the quota of the app.
  - An older device or signer sends no join. It uses an account that
    `STORAGE_OWNERS` or the operator admitted. It ignores the new fields of
    the usage reply. It shows the `refused` error of a put on a closing
    account by its message.
  - A newer storage citizen keeps each account of `STORAGE_OWNERS`, with
    the quota `STORAGE_QUOTA`, as before. Its default rule is `invite`, so
    it refuses a join with no token until the operator makes an invite.
  - The Go function `Conn.Usage` of `sdk/storage` returns a `Usage` value
    in place of three values. An app that calls it changes its call.
- Audits and drain add `audit`, `drain`, `nonce`, `off` and `proof`, and
  two features in the hello reply. They change no earlier message:
  - An older storage citizen answers `audit` and `drain` with
    `invalid_request`, because the request has no blob ID, or an unknown
    operation. Its hello names neither feature, so a newer client does not
    send them, and says that the storage app must update.
  - An older device sends no audit and no drain. A newer storage citizen
    answers each earlier request as before. On a draining account, an older
    device gets `refused` for a put, and shows the message, as for a
    closing account. Its reads still work.
  - The state `draining` is new in the usage reply and the join reply. An
    older device shows it as text, and does not act on it.
- The end of a drain adds no message, no field and no event kind. A device
  now sends `drain` to a draining storage citizen, and the signer sets
  `gone` and removes old `gone` entries:
  - An older device sends no drain request. After a newer device sent one,
    the storage citizen refuses each put of the older device with
    `refused`. Its reads still work.
  - An older storage citizen has no `drain` in its hello. The device sends
    nothing, and says that the app must update. The signer does not set
    `gone` by itself, and the user runs `rm --gone`, as before.
  - An older signer does not set `gone`, and does not remove old `gone`
    entries. The newer devices still send drain requests, and print the
    line of rule 2 of "Draining and gone". The user runs `rm --gone`.
  - An older device reads a list from which the signer removed a `gone`
    entry. It keeps a local install of that storage citizen, and syncs with
    it as an install that the list does not name. The account takes no
    put, so its sync fails for that storage citizen. The user removes the
    install with `arc apps remove <name>`.
  - A storage list of a newer signer is still version 1. An older reader
    reads it.
- Before the account key, the citizen key signed each call, the operator
  listed citizen keys in `STORAGE_OWNERS`, and an account had a pseudonym as
  its name. That changed in one breaking step:
  - An older storage citizen refuses a session of a newer client with
    `invalid_request`, because the body has no pseudonym. It does not know
    the grant set either. The operator updates the storage citizen.
  - A newer storage citizen refuses an older client with `not_owner`,
    because the citizen key is not an account key. The user updates the
    device.
  - A device behind a bunker with no device list worked before with
    `--decrypt all`. Now no grant set names it, so it gets `not_owner`. The
    user adds it on the signer with `arc keys devices add`.
  - An older signer sends no grant set. A newer storage citizen then refuses
    each device behind it with `not_owner`. The user updates the signer.
  - The operator replaces each citizen key in `STORAGE_OWNERS` with the
    account key of `arc keys account`.
  - The account of a pseudonym stays on disk, and no machine uses it again.
    Each device holds a full copy, so the first sync puts each blob again in
    the new account. The operator removes the old directory.

## 8. Proof

### 8.1 Tests

`go test -count=1 -run '^(TestStorageJournalSync|TestStorageImportsDrafts)$' ./internal/proof/`
builds `arc` and `arc-storage`, and runs them with a local relay and two
storage citizens. It proves:

- A page that one home writes is not on the relay. A second home of the same
  citizen reads it after `arc sync` on both. A change crosses back, and the
  second sync gets only the change.
- Each blob has a size that is a power of two from 4 KiB, and holds no text
  of the page.
- A device that signs through a bunker with the default `--decrypt self`
  reads the page after a sync. The device key signs its storage calls.
- A device key with no grant, and a revoked device key, get `not_owner` from
  the storage citizen in a call that does not go through the bunker. A
  granted device key passes the grant check.
- A device with a grant on one storage citizen is refused by a second storage
  citizen until the signer installs it and sends its grant set.
- With a device list, the changes of a machine that holds the citizen key
  still cross. A change of a revoked device is refused. After the device
  joins the list again, the next sync takes it.
- Each storage citizen names its one account by the account key, and no file
  of a storage citizen holds the citizen key, in hex or as its bytes.
- Before a device list, the signer seals blobs with generation 0. A device
  on the list seals with generation 1. After the signer revokes the phone, a
  tablet seals with generation 2. With each data root on the disk of the
  revoked phone, no blob of generation 2 opens, and each blob of generations
  0 and 1 opens. The tablet reads each page. A device that the signer adds
  after the revocation, and a new machine from the seed, open each blob of
  each generation.
- An older relay, `arc relay serve` of commit d53e91b, takes the drafts and
  the private relay list of an older client. After the update the relay
  refuses a new draft, and says why. It still serves the old drafts and the
  private relay list to their author.
- The first sync imports the drafts, and puts them on the storage citizen.
  Then it deletes the drafts and the private relay list from the ARC relay.
  The draft of the keyed root stays. The relay serves no moved deletion,
  also not to the author.
- A relay that is not an ARC relay gets no deletion, keeps its drafts, and
  the sync says so.
- A second import takes nothing, and keeps no moved deletion. A second new
  machine of the citizen imports from the relay, and reads each page
  through the storage citizen.

`go test -count=1 -run '^(TestMovable|TestMoves|TestKept)$' ./runtime/selfsync/`
proves that an old draft is movable only when the storage citizens hold the
record that replaces it on the device, that one deletion names at most 100
events and has the tag, and that a record is kept only with k pieces.
`go test -count=1 -run '^TestAStoreRefusesAMovedDeletion$' ./core/store/`
proves that a store refuses a moved deletion. `go test -count=1 -run
'^(TestARelayRefusesNewDrafts|TestARelayServesSealedDataOnlyToItsAuthor)$'
./adapters/relay/sealed/` proves the refusal, the exception of the keyed
root, the hidden moved deletion, the NIP-11 tag, and that the old drafts
still go to their author. `go test -count=1 -run
'^TestRelayAddPublishesNoPrivateRelayList$' ./cmd/arc/` proves that
`arc relay add` publishes no kind 10013 to a relay that takes any kind.

`go test -count=1 -run '^TestStorageList$' ./internal/proof/` builds `arc`
and `arc-storage`, and runs them with a local relay and three storage
citizens. It proves:

- `arc keys storage add` on the signer puts two storage citizens in the
  storage list as `active`, and `arc keys storage ls` prints both.
- A storage citizen that the signer installs by hand, and that the list
  does not name, still syncs, and the sync names it.
- A phone that signs through the bunker installs both storage citizens from
  the list at its first sync, syncs with both, and reads the page of the
  signer.
- The phone cannot run `arc keys storage rm`.
- A machine with only the seed, the relay and the journal installs both
  storage citizens from the list, and reads every page. It does not install
  the storage citizen that the list does not name.
- `rm` sets `draining`, and the phone still syncs with it. The sync of the
  phone sends a drain request. The machine from the seed did not sync
  after the `rm`. Its put on the draining storage citizen, through the
  normal ARC call of `arc dana open`, gets
  `the account is draining`. `rm --gone` sets `gone`, and the next sync of
  the phone removes the install.
- The test signs a storage list with the `since` of the gone entry moved
  back by 30 days less 1 minute. The sync of the signer keeps the entry.
  With `since` moved back by 30 days, the sync of the signer removes it,
  and `arc keys storage ls` does not name it.
- The relay gives the list to the citizen, and refuses another citizen with
  `restricted`.

Each check failed with a temporary break of its behavior. With the check of
the device removed, the real bunker refused the request with
`the bunker never signs a storage list`. `go test ./cmd/arc -run
TestTheBunkerPolicy` checks that the bunker refuses kind 10273 also with
`--allow-kind 10273`.
`go test -count=1 -run '^TestPruneGone$' ./sdk/storage/` gives the clock:
an entry gone since 2026-09-01 12:00 UTC stays at 29 days, 23 hours, 59
minutes and 59 seconds, and goes at 2026-10-01 12:00 UTC. An old `active`
or `draining` entry stays. The check of the drain request failed when the
device sent `usage` in place of `drain`: the put was stored. The check of
the removal failed when the signer removed an entry only after 30 days and
1 hour. The unit test failed when the removal came only after 30 days.

`go test -count=1 -run '^TestStorageNetworkSync$' ./internal/proof/`
builds `arc`, `arc-storage` and `slowcall`, and runs them with a local
relay and three storage citizens in the storage list. `slowcall` holds each
ARC call to the storage app of charlie for 400 ms, of dana for 800 ms and of
erin for 1,200 ms. It proves:

- One `arc sync` puts each page on all three storage citizens. The whole
  sync takes less than 80% of the sum of the times of the three. A run took
  1.46 s, with 2.9 s for the sum and 1.36 s for the slowest (measured).
- With charlie stopped, `arc sync` names the failure of charlie, and still
  puts the new page on dana and erin. A machine with only the seed then
  reads every page.
- The first failed sync does not warn. With `silent_since` moved 8 days
  back, the next sync prints `charlie: no answer for 8 days; ...`.
- After `rm dana`, a sync puts no blob on dana: its blob files do not
  change, and erin gets the new page. With charlie and erin stopped, a new
  machine from the seed reads every page that dana holds.
- After `rm dana --gone`, the sync does not sync with dana.

Each check failed with a temporary break: a sync one after another, a
failure that cancels the other storage citizens, a warning after 70 days, a
put on a draining storage citizen, and a sync that keeps a gone storage
citizen. `go test -count=1 -run '^TestSilence$' ./runtime/selfsync/` checks
the count of 7 days with a given clock.

`go test -count=1 -run '^TestStoragePieces$' ./internal/proof/` builds
`arc` and `arc-storage`, and the storage app of commit d53e91b from the
history of the repository. It runs them with a local relay, three storage
citizens of this version and one of the older version, all in the storage
list. It proves:

- One `arc sync` codes each record as 2 of 3 pieces, and names the older
  storage citizen: `frank runs an older storage app: it holds full copies;
  update it for erasure coding`.
- Each of the three storage citizens holds one piece of each of the 9
  blobs. Each piece is 2,093 bytes: half of a blob of 4 KiB, plus 45 bytes.
  No piece holds text of a page. The older one holds whole blobs.
- No piece ID is on two storage citizens. Each listing gives a tag of 87
  bytes for each piece, and no tag is on two storage citizens.
- With one byte changed in each piece of dana, and charlie stopped, a
  machine from the seed reads no page. Its sync fails, and names the
  pieces that do not open and the blobs that cannot be read.
- With charlie back, the same machine reads each page from the pieces of
  charlie and erin. It does not ask dana again: its first sync marked each
  piece of dana as lost. It says that the blobs need repair. A new machine
  asks for the piece of dana first for some blobs, refuses it, and rebuilds
  from the pieces of charlie and erin.
- With the directory of dana deleted, a new machine from the seed reads
  each page from charlie and erin.
- With erin stopped too, a new machine reads no page. The sync fails with
  `blobs cannot be read now` and `1 of the 2 pieces that rebuild it
  answered`.
- With the older storage citizen back, the same machine reads each page
  from its blobs of format 1.

`go test -count=1 -run '^(TestStorageRepair|TestStorageRepairTURN)$' ./internal/proof/`
builds `arc` and `arc-storage`, and runs them with a local relay and five
storage citizens through the normal CLI. It proves:

- A plain `arc sync` does not audit. `arc sync --repair` audits each
  storage citizen. With one byte changed in 2 of the 3 pieces of one blob
  and in 1 piece of another, the audit names 1 failure on charlie and 2 on
  dana. Repair seals both records again: no seal of the first has k pieces
  (rule 2), and no storage citizen is free for the second. With erin
  stopped, a new machine from the seed reads each page from charlie and
  dana.
- After `arc keys storage rm erin`, a plain `arc sync` moves nothing, and
  says that blobs need repair. `arc sync --repair` moves each piece of erin
  to frank with the same bytes, and deletes it on erin. erin then lists no
  piece. The same sync of the signer prints
  `erin holds no piece now and takes no put, so the storage list marks it gone`,
  and `arc keys storage ls` names erin `gone`. Nobody runs `rm --gone`.
  With erin stopped, a new machine from the seed reads each page.
  The check failed when the signer did not set `gone`.
- With charlie stopped, its directory deleted, gina added and charlie marked
  gone, `arc serve` of the signer rebuilds each piece of charlie on gina at
  its start. With dana stopped too, a new machine reads each page from
  frank and gina.
- With a relay that serves TURN on the loopback address, and `arc path
  turn` on the signer, each pipe goes through TURN. `arc sync --repair`
  then rebuilds nothing, and says that the repair waits for a direct path.
  `arc sync --repair --turn` rebuilds each missing piece through TURN.
  `arc sync --turn` with no `--repair` fails.

`go test -count=1 -run '^(TestRepairGone|TestRepairTURN|TestRepairAudit|TestRepairRule2|TestRepairDrain|TestRepairExtraPiece|TestRepairMirror)$' ./runtime/selfsync/`
runs the sync against storage apps in memory, with a given clock. It
proves:

- A storage citizen that is gone: a sync without repair puts nothing and
  counts the blobs; a repair rebuilds one piece of each blob on the free
  storage citizen, with 2 pieces down and 1 up for each; a second repair
  does nothing.
- A repair through TURN does nothing, and counts the blobs that wait. With
  TURN allowed, it rebuilds them.
- The first audit checks each piece with its kept proof and gets no piece.
  No audit runs again within 24 hours. A changed piece fails the next
  audit, and repair seals its record again and deletes the 3 old pieces.
  Three failures in one day make the storage citizen suspect. The next day
  starts the count again.
- Rule 2: with 2 of 3 pieces of a blob changed, repair seals the record
  again with no download, and each storage citizen gets a new piece and
  deletes the old one. No put gets `held`.
- A drain moves each piece of the draining storage citizen to the free one
  with the same bytes, and deletes it on the draining one, which is then
  empty.
- One index on two storage citizens: repair deletes the piece on the
  storage citizen of the lower rank.
- A mirror (k = 1) repairs from the record in the store, with no bytes
  down.

`go test -count=1 -run '^(TestRepairFormat1|TestRepairFormat1Interrupted|TestRepairFormat1Drain|TestRepairFormat1Bound)$' ./runtime/selfsync/`
proves the move of blobs of format 1:

- A sync with no repair counts the blobs of format 1, and changes nothing.
  A repair codes each one into pieces from the record, with no bytes
  down, and deletes each whole copy. A second repair does nothing.
- A pipe that ends before its puts deletes no whole copy. A pipe that ends
  before its deletes leaves its whole copies beside the pieces, and the
  next repair deletes them. A new machine reads each record at each point.
- The whole copies of a draining storage citizen move to pieces on the
  active ones, and it is empty. A whole copy of a record that the store
  does not hold is coded from the copy that opens, not from a copy with a
  changed byte.
- One sync moves 1,024 blobs of format 1, and the next one the rest.

`go test -count=1 -run '^TestStorageFormat1$' ./internal/proof/` runs
`arc` of commit d53e91b, which puts whole blobs of format 1 on three
storage citizens. The home then runs this `arc`, and erin drains. A plain
sync changes nothing, and counts the blobs. With dana stopped,
`arc sync --repair` puts a piece on charlie, and deletes the whole copies
of charlie and erin; erin is empty, and a new machine reads each page from
charlie. With dana back, the next repair puts a piece on dana, and deletes
its whole copies. Then charlie and dana hold pieces only, with tags, and a
new machine reads each page from each one alone. The older `arc` then
fails its sync with `the storage citizen announced a blob of`.

Each check of the move failed with a temporary break: a plan that skips
blobs of format 1, a delete of the whole copies after a failed put, no
delete of the copies beside a seal with k pieces, whole copies kept on a
draining storage citizen, no limit for each sync, and a whole copy coded
with no check.

Each check failed with a temporary break of its behavior: an audit that
takes any proof, no rule 2, no move in a drain, no rebuild of a missing
index, repair through TURN, `arc serve` that does not repair, a suspect
count of 30, no delete of an extra piece, a delete on the higher rank, an
audit each hour, no reseal of a lost piece with no free storage citizen, a
mirror that downloads, and a piece ID with no seal. The last failed
`TestPieceKeys` and `TestRepairRule2`: the new seal got `held`.

`go test -count=1 -run '^(TestPieceKeys|TestCode|TestPieces|TestPieceIDsDiffer|TestParityPrefix|TestNewStorageCitizenAddsPiece)$' ./runtime/selfsync/`
proves:

- The piece key, the piece id key, the placement key and a piece ID are
  the values that Python's `hmac` and `hashlib` computed.
- k and t follow the table of decision 6 of the storage network proposal.
- For k from 1 to 4 and blobs of 4 KiB and 64 KiB, each set of k
  consecutive pieces rebuilds the blob. A piece with a changed byte, a
  piece with the tag of another piece, and a tag with a changed byte do
  not open. A tag of generation 2 does not open with the piece key of
  generation 0.
- Three storage citizens show no common piece ID for one blob.
- The shards of a (k, n) code are the first n shards of a (k, m) code of
  v1.14.2.
- A storage citizen that did not answer gets its piece of each blob at a
  later sync that repairs, and a fourth storage citizen gets a fourth piece. No other
  piece changes. A pipe through TURN gets no such piece. A second device
  rebuilds each record from the two pieces that came late.

`go test -count=1 -run '^TestPieceTags$' ./apps/storage/server/` proves
that the app keeps the tag of a piece, also after a restart, takes the
size of a piece only with a tag, and refuses a tag of 40 bytes.

Each check failed with a temporary break of its behavior: a read that
needs all t pieces, a sync that does not report unreadable blobs, k = 1, a
read that skips the entries with no tag, a read with no second round, a
piece AEAD that is not checked, one piece id key for each storage citizen,
the Cauchy matrix of the library, k = floor(S / 2), no piece for a late
storage citizen, a late piece of a new seal, a missing piece through TURN,
an app that drops the tag, an app that takes the size of a piece with no
tag, and a client that refuses the size of a piece. One piece id key for
each storage citizen did not fail the proof through the CLI: the index
alone makes the piece IDs of one blob differ. `TestPieceIDsDiffer` and
`TestPieceKeys` failed.

`go test -run '^$' -bench BenchmarkPieces -benchtime 300ms -cpu 1,16 ./runtime/selfsync/`
measures the pieces of one blob on a device: encode is the code and the
sealing of t pieces, decode is the opening of k pieces with the first data
pieces lost, and the rebuild. Apple M3 Max, go1.27.1, v1.14.2, 2026-10-12,
one core, in blob bytes:

| Code | Work | 4 KiB | 64 KiB | 1 MiB | 64 MiB |
| --- | --- | --- | --- | --- | --- |
| 2 of 3 | encode | 342 MB/s | 492 MB/s | 609 MB/s | 696 MB/s |
| 2 of 3 | decode | 602 MB/s | 683 MB/s | 958 MB/s | 987 MB/s |
| 2 of 4 | encode | 288 MB/s | 377 MB/s | 475 MB/s | 520 MB/s |
| 2 of 4 | decode | 540 MB/s | 585 MB/s | 857 MB/s | 1,000 MB/s |
| 3 of 5 | encode | 277 MB/s | 456 MB/s | 589 MB/s | 621 MB/s |
| 3 of 5 | decode | 513 MB/s | 554 MB/s | 826 MB/s | 1,026 MB/s |
| 3 of 6 | encode | 239 MB/s | 409 MB/s | 479 MB/s | 520 MB/s |
| 3 of 6 | decode | 463 MB/s | 507 MB/s | 736 MB/s | 977 MB/s |

The AEAD sets these values: the code alone ran at 1.9 to 52 GB/s in the
proposal. The slowest value, 230 MB/s with 16 cores, is about 19 times
the 12 MB/s of a direct pipe of 97 Mbit/s (computed). 16 cores give the
same values: one blob runs on one core. The first `arc sync` of
`TestStoragePieces`, 9 records on three storage citizens and an older one,
took 407 to 437 ms in three runs.

The proofs of the delivery spec and of the app interface spec carry pages
through a storage citizen and through a directory.

`go test -count=1 -run '^(TestInviteAdmits|TestAskRule|TestOpenRuleQuotaEachAccount)$' ./apps/storage/cmd/arc-storage/`
builds `arc` and `arc-storage`, and runs them with a local relay and a
storage citizen for each rule, through `arc serve`. Each joining citizen
runs `arc keys storage add`, and is not in `STORAGE_OWNERS`. It proves:

- Under `invite`, a join with no token is refused, and a join with a wrong
  token is refused. A join with a token from `arc-storage invite` is
  admitted at the quota and the plan of the token. A second join of the
  account gets its state. A second use of the token is refused.
- Under `ask`, the join is pending, and the account opens no session.
  `arc-storage requests` shows the account key, `want`, the citizen key and
  the note. After `arc-storage accept`, the next join is admitted at the
  quota of the operator. After `arc-storage refuse`, the next join is
  refused.
- Under `open`, two joins are admitted at `STORAGE_OPEN_QUOTA`, and a third
  is refused, because the sum passes `STORAGE_CAPACITY`.
- A put over the quota of one account fails with the bytes in use and the
  quota, while a put of another account works. `usage` reports the quota,
  the plan and the state of each account. After `arc-storage account set`,
  the put works. After `arc-storage account close`, a put is refused, and a
  list works.

Each check failed with a temporary break of its behavior.

`go test -count=1 -run '^(TestPutManyWindow|TestPutManyOlderApp)$' ./sdk/storage/`
runs the client against a fake app on the loopback address. It proves:

- The client keeps at most 32 puts in flight, and each reply goes to its
  own request, although the app answers out of order.
- The bytes of each put reach the app in the order of the `send` replies.
- After `invalid_request` to `hello`, the client sends one put at a time,
  with no `n`.

`go test -count=1 -run '^(TestRequestsInFlight|TestHelloAndWindow|TestCrashMidWrite|TestImportOldState|TestIndexBytesGrowLinearly)$' ./apps/storage/server/`
runs the storage app and the client of `sdk/storage` on the loopback
address, and the index on disk. It proves:

- 101 puts of 4 KiB and 4 of 16 KiB in flight are stored with their own
  bytes, in the order of the requests. A second put of one ID in the same
  window gets `held`. A get and a delete with `ids` answer each ID, and
  `not_found` or `absent` for an unknown ID.
- `hello` gives the version, `batch` and the window. A 33rd put in flight
  ends the pipe.
- A program that stops after the bytes of a put and before its entry
  leaves an index whose entries each have a file of their size, and whose
  bytes in use and count agree. The app does not serve that put, and the
  next put of the ID gets the next sequence number.
- An older `state.json` with a record, a grant set, two blobs and a
  tombstone is imported once, with its sequence numbers. A command of the
  operator before the import keeps the entries. A second start does not
  import again.
- 10,000 puts in one account write about 31 KB of index for each put. The
  last 1,000 puts write at most 2 times the bytes of the first 1,000.

Each check failed with a temporary break of its behavior.

`STORAGE_LINK_PUTS=1000 go test -count=1 -v -run TestEmulatedLinkPuts ./apps/storage/server/`
measures puts on an emulated link of 92.5 Mbit/s and 44 ms. It is a
measurement, not a check. On an Apple Silicon Mac, 2026-10-12, 1,000 puts
of 4 KiB took 117.1 to 121.3 s in three runs one at a time with `state.json`, and 7.9 to 8.5 s in
four runs with 32 in flight and `index.db`.

`TestAudit` in `apps/storage/cmd/arc-storage` puts three files through the
real relay, `arc serve` and `arc-storage`, and audits them: each passes.
The test then changes one byte of one blob file, and removes another blob
file. The next audit gives `pass fail lost`, and fails. `TestDrain` drains
the account with `arc-storage drain`: a put from the second home is
refused, and usage, list, get and audit still work. `drain -off` takes
puts again. `TestAuditReplay` in `apps/storage/server` audits one blob two
times with two nonces, through the real app and client: each proof is the
proof of the bytes for its own nonce, and the first proof does not pass for
the second nonce. `TestAuditProofVector` in `sdk/storage` checks the proof
against SHA-256 that the Python standard library computed. Each test failed
with a temporary break: an app that hashes only the first KiB, a client
that ignores another proof, an app that hashes a lost file as empty, an app
that ignores the nonce, and an app that takes a put on a draining account.

`go test ./sdk/storage/ -run TestVerifyJoin` checks that the signature of
the citizen key in a join verifies only over its account key and its
storage key. `go test ./runtime/selfsync/ -run 'TestAccountKey|TestNewKeys'` checks the
account key and the keys of section 3.4 against HKDF-SHA256 that the Python
standard library computed. `go test
./apps/storage/...` checks that a newer grant set revokes a device key, and
that an older set is refused.

### 8.2 Not built

- Not built. `arc sync` does not print the usage of each storage citizen.
  `arc-storage usage` prints it.
- Not built. The removal of the bytes of a closed account. The operator
  removes its directory.
- Not built. An `offer` in a refused or pending reply, and a `pay` field
  in the join, for a paid service. `plan` and `expires` keep the place.
- Not built. Set reconciliation over blob IDs. The cursor of section 4.1
  does the work.
- Not built. The deletion of the blobs of old versions. Each version of a
  draft and each checkpoint stays on the storage citizen.
- Not built. A command of `arc relay serve` that removes the old drafts
  from the store of the relay on 2027-01-10. The operator must remove them
  from the store by other means.
- Not built. A repair for a v0.25.0 device whose store took a moved
  deletion from a relay that served it.
- Not built. The close call at the end of a drain (section 5.2 of the
  storage network proposal). When the signer sets `gone`, the account stays
  `draining` on the storage citizen, with no blob. A close call that the
  account key authors would close the account. Then a later
  `arc keys storage add` of the same storage citizen fails until the
  operator opens the account again, and no command does that today.
