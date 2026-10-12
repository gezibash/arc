# Storage citizens and the sync of self data

- Status: partial
- Layers: sdk, runtime
- Owns: sdk/storage
- Proof: go test -count=1 -run '^(TestStorageJournalSync|TestStorageImportsDrafts)$' ./internal/proof/
- Remaining: the steps that section 8.2 names: invites, the storage list of kind 10273, random data roots, set reconciliation, the deletion of blobs, and the removal of the old drafts from the relays.
- Unverified: No test runs the two ends of a pipe on two machines or behind real NATs; the tests use the loopback address. No test runs a signer that is offline when it revokes a device. No test runs the sync of `arc serve` on its 15-minute tick; the tests run `arc sync`. No test measures a sync of 1,000 pages.

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
  in `STORAGE_OWNERS`. It MUST refuse a set with an `at` that is less than the
  `at` of the set it holds, with `stale_grants`. A set with an equal `at`
  replaces the set, so the signer can send it again.
- The reply MUST be `{"v":1,"devices":<n>}`, the count of device keys.
- The newest set replaces the older set. A device key that the newest set
  does not hold is revoked. A revocation is a set without the device.

**The session.** A device opens a session with one call:

- The manifest command `open` of the installed app makes the body:
  `{"v":1,"session":{"hold_ms":<n>,"stun":"<url>","sdp":<offer>}}`. An older
  client also sent `account`, a pseudonym; the storage app ignores it.
- The storage app MUST find the account from the author: an account key in
  `STORAGE_OWNERS` uses its own account, and a device key uses the account
  whose newest grant set holds it. It MUST refuse each other author with
  `not_owner`, before it opens a pipe.
- The reply MUST be `{"v":1,"sdp":<answer>}`, or an error: `invalid_request`,
  `not_owner`, `busy` or `storage_failed`.
- The two ends then open one pipe, as [the direct pipe spec](../direct/SPEC.md)
  defines.

### 3.3 The requests on the pipe

The device sends one request as one text message of JSON. The storage app
answers each request with one text message of JSON before the device sends
the next request.

| Request | Reply |
| --- | --- |
| `{"op":"put","id":"<64 hex>","size":<n>}` | `{"status":"send"}`, then the device sends the bytes, and the app answers `{"status":"stored","seq":<n>}`. Or `{"status":"held"}` or `{"status":"tombstoned"}`, and no bytes go. |
| `{"op":"list","after":<cursor>,"limit":<n>}` | `{"entries":[{"seq":<n>,"id":"…","size":<n>}, {"seq":<n>,"id":"…","deleted":true}],"next":<cursor>}` |
| `{"op":"get","id":"<64 hex>"}` | `{"size":<n>}`, then the bytes of the blob. |
| `{"op":"delete","id":"<64 hex>"}` | `{"status":"deleted","seq":<n>}` or `{"status":"absent"}`. |
| `{"op":"usage"}` | `{"used":<n>,"quota":<n>,"blobs":<n>}` |

- The bytes of a blob MUST go in binary messages of at most 16 KiB.
- A refused request gets `{"error":"<code>","message":"<text>"}`. The codes
  are `invalid_request`, `bad_size`, `over_quota`, `not_found` and
  `storage_failed`.
- A list MUST give at most 1,000 entries. A device lists again from `next`
  until a list gives no entry.

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
- For generation 0, the keys MUST be
  `blob key = HKDF-SHA256(keyed root, info = "arc-storage-blob-v1")` and
  `id key = HKDF-SHA256(keyed root, info = "arc-storage-id-v1")`, with no
  salt, 32 bytes each.

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

Steps 4 and 5 run in this order. A device that was offline thus gets a
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

1. The citizen runs `arc keys account <key of the storage citizen>` on the
   signer. It prints the account key.
2. The operator of the storage citizen puts the account key in
   `STORAGE_OWNERS`.
3. The signer installs the storage citizen, as each device does.
4. `arc keys devices add` and `arc keys devices revoke` publish the device
   list, and then send the grant set to each storage citizen that the signer
   installed. Each line `grants: sent to the storage citizen <name>` names a
   storage citizen that took the set.
5. `arc sync` on the signer sends the grant set again (section 4.1). A
   revocation that did not reach a storage citizen thus reaches it on the
   next sync of the signer.

The signer knows a storage citizen only if it installed it. A signer that did
not install a storage citizen sends it no grant set, and that storage citizen
refuses each device of the citizen. Invites and the storage list of kind
10273 replace this in a later step.

## 5. Failures

| Cause | What the device does | What the user sees |
| --- | --- | --- |
| No storage citizen is installed | The data stays on the device. | `self data stays on this machine: no storage citizen; install one with arc install <key> storage` |
| The storage citizen refuses the caller: the account key is not in `STORAGE_OWNERS`, or the device key is in no grant set | No pipe opens. | `the storage citizen refused this machine: its key is neither an account key in STORAGE_OWNERS nor a device key with a grant; the signer sends grants with arc keys devices add and arc sync` |
| A grant set does not reach a storage citizen | `arc keys devices add` or `revoke` fails after it publishes the device list. The storage citizen keeps the older set until the next sync of the signer. | `the device list changed, but <name>: the grants did not reach the storage citizen: <reason>; run arc sync on the signer to try again` |
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
- In generation 0, each holder of the citizen key derives the blob key. A
  revoked device keeps the keyed root, so it can open each blob. Random data
  roots fix this in a later step.
- With no device list, the AEAD is the only check of who made a blob: only a
  holder of the keyed root makes one that opens. With a list, each device
  checks the device key of each change.
- A hostile storage citizen can withhold or delete blobs. A device keeps its
  full copy. It cannot change or make a blob that opens.
- The storage citizen pays for the disk. The quota of each account limits
  it. The TURN quota of the citizen pays for a pipe that goes through the
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
  0 refuses a blob of a later generation, and says to update arc.
- `sdk/storage` holds the messages that `apps/storage/wire` held before. The
  bytes on the pipe did not change.
- Before the account key, the citizen key signed each call, the operator
  listed citizen keys in `STORAGE_OWNERS`, and an account had a pseudonym as
  its name. That changed in one breaking step:
  - An older storage citizen refuses a session of a newer client with
    `invalid_request`, because the body has no pseudonym. It does not know
    the grant set either. The operator updates the storage citizen.
  - A newer storage citizen refuses an older client with `not_owner`,
    because the citizen key is not an account key. The user updates the
    device.
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
- The first sync imports the drafts that an older client put on the relay,
  and puts them on the storage citizen. A second import changes nothing.
  The drafts stay on the relay.

The proofs of the delivery spec and of the app interface spec carry pages
through a storage citizen and through a directory.

`go test ./runtime/selfsync/ -run TestAccountKey` checks the account key
against HKDF-SHA256 that the Python standard library computed. `go test
./apps/storage/...` checks that a newer grant set revokes a device key, and
that an older set is refused.

### 8.2 Not built

- Not built. Invites and `arc storage add`. The operator names the owners in
  `STORAGE_OWNERS`.
- Not built. The sealed storage list of kind 10273.
- Not built. Random data roots and generations after 0 (base step 2).
- Not built. Set reconciliation over blob IDs. The cursor of section 4.1
  does the work.
- Not built. The deletion of the blobs of old versions. Each version of a
  draft and each checkpoint stays on the storage citizen.
- Not built. The NIP-09 deletion of the imported drafts on the relays, and a
  relay that refuses self data.
