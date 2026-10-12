# Storage citizens and the sync of self data

- Status: partial
- Layers: sdk, runtime
- Owns: sdk/storage
- Proof: go test -count=1 -run '^(TestStorageJournalSync|TestStorageImportsDrafts|TestStorageSealsWithDataRoots|TestInviteAdmits|TestAskRule|TestOpenRuleQuotaEachAccount|TestNewKeys|TestPutManyWindow|TestPutManyOlderApp|TestRequestsInFlight|TestHelloAndWindow|TestCrashMidWrite|TestImportOldState|TestIndexBytesGrowLinearly)$' ./internal/proof/ ./apps/storage/cmd/arc-storage/ ./runtime/selfsync/ ./sdk/storage/ ./apps/storage/server/
- Remaining: the steps that section 8.2 names: the usage line of `arc sync`, the removal of the bytes of a closed account, the storage list of kind 10273, set reconciliation, the deletion of blobs, and the removal of the old drafts from the relays.
- Unverified: No test runs the two ends of a pipe on two machines or behind real NATs; the tests use the loopback address. No test runs a signer that is offline when it revokes a device. No test runs the sync of `arc serve` on its 15-minute tick; the tests run `arc sync`. No test measures a sync of 1,000 pages through `arc sync`; the measurement of requests in flight runs the app and the client on an emulated link, not on two machines. No test stops the power of a machine: the crash test stops the program, and the page cache of the machine survives. No test runs an invite token older than 7 days, a request older than 30 days, or an account after its `expires`; unit logic decides them from the clock. No test gives a blob of a later generation to a revoked device through `arc sync`: the signer and the storage citizen refuse the device first. The proof reads the data roots that the revoked device keeps on its disk, and tries each one on each blob file.

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
- the blob that holds one record,
- the sync of a device with a storage citizen,
- the one-time import of the drafts that the relays hold.

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
- **Storage citizen**: a citizen that serves the storage app. A citizen uses
  a storage citizen after `arc install <key> storage`.
- **Record**: one event of self data.
- **Blob**: the encrypted bytes of one record.
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
| `{"op":"hello"}` | `{"v":2,"features":["batch"],"window":32}` |
| `{"op":"put","id":"<64 hex>","size":<n>}` | `{"status":"send"}`, then the device sends the bytes, and the app answers `{"status":"stored","seq":<n>}`. Or `{"status":"held"}` or `{"status":"tombstoned"}`, and no bytes go. |
| `{"op":"list","after":<cursor>,"limit":<n>}` | `{"entries":[{"seq":<n>,"id":"…","size":<n>}, {"seq":<n>,"id":"…","deleted":true}],"next":<cursor>}` |
| `{"op":"get","id":"<64 hex>"}` | `{"size":<n>}`, then the bytes of the blob. |
| `{"op":"get","ids":["<64 hex>",…]}` | `{"items":[{"id":"…","size":<n>},{"id":"…","error":"not_found"}]}`, then the bytes of each found blob, in the order of `ids`. |
| `{"op":"delete","id":"<64 hex>"}` | `{"status":"deleted","seq":<n>}` or `{"status":"absent"}`. |
| `{"op":"delete","ids":["<64 hex>",…]}` | `{"items":[{"id":"…","status":"deleted","seq":<n>},{"id":"…","status":"absent"}]}` |
| `{"op":"usage"}` | `{"used":<n>,"quota":<n>,"blobs":<n>,"state":"<state>","plan":"<plan>","expires":<n>}` |

**Requests in flight.**

- Before a device keeps requests in flight or sends `ids`, it MUST send
  `hello`, one time in each session. If the reply holds the feature
  `batch`, the device can keep requests in flight. If the reply is the
  error `invalid_request`, or holds no `batch`, the device MUST send one
  request at a time, with no `n`, and wait for its last reply.
- With `batch`, the device MUST number each request with `n`, from 1, one
  higher for each request of the session. The app MUST put the `n` of the
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

**The other rules.**

- The bytes of a blob MUST go in binary messages of at most 16 KiB.
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
| `state` | `open`, `closing` or `closed`. The app derives it, as below. |

- An account key in `STORAGE_OWNERS` with no record is admitted, with the
  quota `STORAGE_QUOTA` and the plan `owner`.
- An account is `closing` from the earlier of the time that the operator
  closed it and `expires`. It is `closed` 30 days later. A closed account
  takes no session.

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

## 4. Behavior

### 4.1 One sync

`arc sync`, the start of `arc serve`, and each 15 minutes of `arc serve` run
these steps for each installed storage citizen. A write never starts a sync,
because a direct path takes seconds to open.

1. On a signer with a device list, send the grant set to each installed
   storage citizen (section 3.2). A failure is a note, and the sync goes on.
2. Open a pipe (section 3.2).
3. List from the cursor of this storage citizen, and keep the IDs.
4. Get each listed blob that the store does not hold. Open it, check it
   (section 3.5), and give the record to the store. The store applies a
   deletion, and refuses a record that a deletion names.
5. Read the store again. Seal each record whose blob ID the storage citizen
   did not list, and put it.
6. Keep the cursor and the IDs in `<home>/selfsync/<key of the storage citizen>.json`.

Steps 4 and 5 keep requests in flight on a storage app with `batch`
(section 3.3): step 4 gets 256 blobs with each request, and step 5 seals 64
records at a time and keeps 32 puts in flight. Steps 4 and 5 run in this
order. A device that was offline thus gets a
deletion before it could put the old records back.

### 4.2 The import

The first `arc sync` after an update gets every event of self data of the
citizen from each relay, and keeps it in the local store. It then marks the
import as done in `<home>/selfsync/imported`. Step 5 of the next sync puts
the imported records on the storage citizen. `arc sync --import` runs the
import again. A second import takes nothing new, because the store keeps each
event once.

If a relay does not answer, the import is not marked, and the next sync runs
it again.

### 4.3 How a device finds its storage citizen

Each install of the app `storage` is a storage citizen of the citizen. Each
device installs it once: `arc install <key> storage`. This is a local
setting of each device. The sealed storage list of kind 10273 replaces it in
a later step.

### 4.4 Accounts and grants

1. The operator makes an invite link with `arc-storage invite`, and gives
   it to the citizen. Under the rule `ask` or `open`, the key of the storage
   citizen is enough.
2. The citizen runs `arc keys storage add <link or key>` on the signer. The
   signer sends the join (section 3.2).
   - On `admitted`, it prints the quota and the plan, installs the storage
     app of the storage citizen if it is not installed, and sends the grant
     set if a device list exists.
   - On `pending`, it prints the message and installs nothing. The citizen
     runs the command again after the operator decides.
   - On `refused`, it fails with the message.
3. Each other device installs the storage citizen with
   `arc install <key> storage`, as before. The storage list of kind 10273
   replaces this in a later step.
4. An operator can still admit an account by hand: the citizen runs
   `arc keys account <key of the storage citizen>` on the signer, and the
   operator puts the account key in `STORAGE_OWNERS`, or runs
   `arc-storage accept <account key>`.
5. `arc keys devices add` and `arc keys devices revoke` publish the device
   list, and then send the grant set to each storage citizen that the signer
   installed. Each line `grants: sent to the storage citizen <name>` names a
   storage citizen that took the set.
6. `arc sync` on the signer sends the grant set again (section 4.1). A
   revocation that did not reach a storage citizen thus reaches it on the
   next sync of the signer.

The signer knows a storage citizen only if it installed it. A signer that did
not install a storage citizen sends it no grant set, and that storage citizen
refuses each device of the citizen. The storage list of kind 10273 replaces
this in a later step.

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

## 5. Failures

| Cause | What the device does | What the user sees |
| --- | --- | --- |
| No storage citizen is installed | The data stays on the device. | `self data stays on this machine: no storage citizen; install one with arc install <key> storage` |
| The storage citizen refuses the caller: the account key is not admitted, or the device key is in no grant set | No pipe opens. | `the storage citizen refused this machine: its key is neither an account key in STORAGE_OWNERS nor a device key with a grant; the signer sends grants with arc keys devices add and arc sync` |
| A join is refused | `arc keys storage add` installs nothing. | `<name> refused the account: <message>` |
| A join is pending | `arc keys storage add` installs nothing. | `<name>: pending: the operator decides by hand; ask again later` |
| The storage citizen runs a storage app from before admission | It answers `invalid_request` to the join. | `<name> runs an older storage app: it cannot admit accounts; use STORAGE_OWNERS` |
| A put on a closing account | The sync stops, as for `over_quota`. | `the account is closing: it takes no put, and its reads end at <day>` |
| A grant set does not reach a storage citizen | `arc keys devices add` or `revoke` fails after it publishes the device list. The storage citizen keeps the older set until the next sync of the signer. | `the device list changed, but <name>: the grants did not reach the storage citizen: <reason>; run arc sync on the signer to try again` |
| A device signs through a bunker, and the citizen has no device list | No grant set exists, so the storage citizen refuses the device key. | The refusal above. Add the device on the signer with `arc keys devices add`. |
| A grant set is older than the set of the account | The storage citizen keeps its set. | `stale_grants` |
| No path opens | The sync of that storage citizen fails. The other storage citizens still sync. | The error of the direct pipe, and `some transports failed`. |
| A blob does not pass section 3.5 | The device skips it. | `refused <id>: <reason>` |
| A put passes the quota | The sync stops. The cursor and the IDs so far are kept. | The `over_quota` message, with the bytes in use and the quota. |
| A relay does not answer during the import | The import is not marked. | `the import of the drafts on the relays is not complete` |

## 6. Security

- The storage citizen sees the account key, each device key of the grant
  set, the count of the blobs, their padded sizes, the times of the puts and
  the sessions, and the IP address of each machine. It does not see the
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
- A hostile storage citizen can withhold or delete blobs. A device keeps its
  full copy. It cannot change or make a blob that opens.
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

- An older client still writes drafts to its relays, and reads only from its
  relays. A newer client still reads drafts from the relays when it queries
  self data, so it sees the pages of an older client. An older client does
  not see the pages that a newer client writes. The user updates each device.
- The newer client does not delete the drafts on the relays in this step.
  Older clients read them. The proposal keeps them on the relays for 90 days
  (proposal, decision 6). A later step sends a NIP-09 deletion for each
  imported draft, once a storage citizen holds it. After 90 days, the relay
  operator removes each event of kinds 31234, 1234, 3275 and 10013, and the
  relay refuses these kinds.
- The blob format names its generation. A device that knows only generation
  0 refuses a blob of a later generation, and says to update arc. Each
  device of a citizen with a device list seals new blobs with generation 1
  or later. Thus an older device does not get the changes of a newer
  device. A newer device still opens each blob that an older device sealed,
  because those blobs are of generation 0. The user updates each device.
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
- The first sync imports the drafts that an older client put on the relay,
  and puts them on the storage citizen. A second import changes nothing.
  The drafts stay on the relay.

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

- 105 puts of 4 KiB and 4 of 16 KiB in flight are stored with their own
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
- Not built. The sealed storage list of kind 10273.
- Not built. Set reconciliation over blob IDs. The cursor of section 4.1
  does the work.
- Not built. The deletion of the blobs of old versions. Each version of a
  draft and each checkpoint stays on the storage citizen.
- Not built. The NIP-09 deletion of the imported drafts on the relays, and a
  relay that refuses self data.
