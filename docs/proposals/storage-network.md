Status: proposed

# The storage network: admission, quota and Reed-Solomon

This proposal answers issue #212. It follows
[the storage proposal](storage.md) (#172) and the per-device grants of #209.
It closes four gaps as one design:

1. A citizen cannot ask a storage citizen for an account.
2. The quota is one number for the whole storage citizen.
3. A device syncs with storage citizens that it installed by hand. The seed
   alone does not find them.
4. A storage citizen holds each blob whole. Nothing codes the data across
   several storage citizens.

Nothing in this document is built. No production code changes with it.

Each claim about ARC names a file and a line at commit `d1a6662`. Each number
carries one of these labels:

- **measured**: a run on a machine, with the command in this document or in
  the cited file;
- **computed**: arithmetic from measured or cited values;
- **estimated**: a value from the code or from a stated assumption, with no
  run;
- **cited**: a value from an outside source, with its link.

## 0. Read this first

These facts limit the design, or must change before it.

1. **The blobs are not sealed with the data roots.** `sealSelf` refuses each
   generation other than 0 (`runtime/citizen/selfsync.go:49-51`). The blob
   key comes from the keyed root (`runtime/citizen/selfsync.go:53-57`). The
   only caller of `DataRoot` outside its own file is
   `cmd/arc/devices.go:259`. The devices spec says the same: "No app seals its
   data with the data roots yet" (`docs/devices/SPEC.md:7`). A revoked device
   therefore opens each blob, and it would open each piece of this design.
   Build step 0 (section 9) fixes this first.
2. **The storage app does not scale to many small objects.** Each change
   rewrites the whole `state.json` of the account
   (`apps/storage/server/store.go:73`, `:171-193`). A put takes two round
   trips, and the device sends one request at a time
   (`docs/storage/SPEC.md:74-80`). The device syncs its storage citizens one
   after another (`runtime/citizen/selfsync.go:123`). Erasure coding
   multiplies the count of objects by n. Section 6.12 gives the cost.
3. **A file part makes a blob of 64 KiB.** A full part of 32 KiB of text
   becomes a blob of 65,536 bytes (measured, section 6.11). A file then
   costs 2.67 times its bytes before any coding (computed: 24,576 bytes of
   file make 32,768 bytes of base64 text, which make one blob of 65,536
   bytes). This is a cost of decision 11 of the storage proposal
   (`docs/proposals/storage.md:608-610`). Open question 9 asks about it.
4. **Erasure coding does not hide size, time or count.** Each storage citizen
   still sees how many pieces an account holds, their sizes and the times of
   the puts. Two storage citizens that work together can still join one
   account by time, size and IP address (section 7).
5. **Each device keeps a full copy.** The local store holds every record
   (`docs/storage/SPEC.md:19-22`). Reed-Solomon therefore does not make
   normal sync faster or more available. It does three things: a storage
   citizen holds a piece that is useless alone; the citizen keeps its data
   after it loses every device; and the citizen pays less disk than with a
   full copy on each storage citizen.

## 1. The recommendation in short

- **Admission.** The signer sends one ARC call, `join`, authored by the
  account key of #209. The storage citizen answers `admitted`, `pending` or
  `refused`. It decides with one of three rules: an invite token (the
  default), the approval of the operator, or open admission with a fixed
  quota.
- **Quota.** Each account keeps its own quota, a plan name and an end time.
  The `join` reply and each `usage` reply report them. The put check uses the
  quota of the account. The plan name and the end time are the hook for a
  paid service. Nothing pays now.
- **Several storage citizens.** The signer keeps the list of storage
  citizens in kind 10273, sealed to the citizen key. Each device reads the
  list and installs each storage citizen in it. A device with only the seed
  finds the list on the relays.
- **Reed-Solomon.** Encrypt each record into a blob as today. Then code the
  blob with `klauspost/reedsolomon` into pieces, any k of which rebuild it.
  The unit is one blob. The rule for k is k = ⌈S/2⌉, where S is the count of
  active storage citizens. A blob gets min(S, 2k) pieces, one on each storage
  citizen. Each piece carries a sealed tag that names its blob, its index
  and its code. The storage citizen gives the tag back in each listing. Each
  device builds the piece map from the listings. No device writes a shared
  map.
- **Repair and audits.** Each device that runs `arc serve` repairs. It
  samples pieces, gets them and checks their seal. It moves the pieces off a
  storage citizen that leaves. It rebuilds the pieces of a storage citizen
  that lost them.

## 2. Facts this design starts from

| Fact | Source |
| --- | --- |
| A blob is `id || ts || gen || nonce || AEAD(pad(envelope))`. Its size is a power of two from 4 KiB to 64 MiB. | `docs/storage/SPEC.md:95-105` |
| The nonce of a blob is random. Two seals of one record give two different blobs with one ID. | `runtime/selfsync/blob.go:134-142` |
| The blob ID is `HMAC(id key, record ID)`. Each storage citizen of one citizen uses the same id key. | `docs/storage/SPEC.md:97`, `docs/proposals/storage.md:297-300`, decision 8 at `:599-601` |
| The storage app accepts only a power of two from 4 KiB to 64 MiB. | `sdk/storage/wire.go:20-22`, `:156-158` |
| The quota is one value for the whole app. `STORAGE_QUOTA` sets it. | `apps/storage/server/store.go:29`, `apps/storage/server/server.go:69-75` |
| The app admits a caller only if `STORAGE_OWNERS` lists its key. | `apps/storage/server/server.go:76`, `:151` |
| An unknown operation gets `invalid_request`. | `apps/storage/server/server.go:340` |
| A device finds a storage citizen only through a local install of the app `storage`. | `runtime/citizen/selfsync.go:97-101`, `docs/storage/SPEC.md:180-185` |
| A device syncs each storage citizen in turn. | `runtime/citizen/selfsync.go:123-135` |
| The keyed root never changes, and the seed derives it. | `docs/proposals/storage.md:148-149`, `docs/devices/SPEC.md:275-277` |
| A draft part carries at most 32 KiB of text. Each larger draft is split. So each blob of self data is 4 KiB to 64 KiB today (estimated from the code). 64 MiB is a bound that no record reaches. | `core/draft/draft.go:40`, `runtime/iface/data.go:229` |
| A storage citizen keeps reads for 30 days after it closes an account. | `docs/proposals/storage.md:234-237` |
| A storage citizen keeps a tombstone for 90 days. | `apps/storage/server/store.go:18` |
| A direct pipe ran at a median of 97.0 Mbit/s (measured). Through TURN: 9.1 Mbit/s (measured) and 10.5 Mbit/s from a home Mac (measured). | `docs/proposals/direct-path.md:97`, `:284`, `docs/proposals/storage.md:321-324` |
| The TURN quota is 1 GiB a day on relay.republic.sh. | `docs/proposals/storage.md:313-315` |
| A link of 92.5 Mbit/s had a round trip of 44 ms (measured). | `docs/direct/SPEC.md:92` |
| `allow` in the Arcfile matches the key of the caller. | `apps/storage/README.md:79-81` |
| `go.etcd.io/bbolt` is a dependency already. `klauspost/reedsolomon` is not. | `go.mod:20` |

The work of #209 is not merged. These facts come from the note of the #209
session and from the diff in its worktree, on 2026-10-12. They have no
stable line numbers.

- The account key is
  `HKDF-SHA256(citizen secret, info = "arc-storage-account-v1" || storage key)`.
  Only a machine with the citizen key derives it. A NIP-46 bunker cannot.
- The account public key names the account. It replaces the pseudonym.
- The signer pushes a grant set, `{"v":1,"grants":[...],"at":<unix>}`, in a
  live call that the account key authors. A newer set replaces an older one.
- `STORAGE_OWNERS` lists account keys.

## 3. Admission

### 3.1 Who asks

Only a machine with the citizen key derives the account key. That machine is
the signer. Thus the signer asks for each account. A device never asks. A
device gets the list of storage citizens from kind 10273 (section 5), and
its grant from the push of #209.

### 3.2 The call

The signer runs `arc storage add <link or key>`. It derives the account key
for that storage citizen, and sends one live call to the capability
`storage`. The account key authors the call. The body is:

```json
{"v":1,"join":{"invite":"<token>","want":1073741824,"note":"<text>"}}
```

- `invite` is optional. It is the token of an invite link.
- `want` is the quota that the citizen asks for, in bytes.
- `note` is optional, at most 140 bytes. The operator sees it in the
  `ask` rule.

A second `join` with the same account key is not an error. The storage
citizen answers with the current state of the account. Thus the signer can
ask again at each sync until the operator decides.

### 3.3 The rules of the storage citizen

The operator chooses one rule with `STORAGE_ADMIT`:

| Rule | What the storage citizen does |
| --- | --- |
| `invite` (default) | It admits a `join` with a good token, at the quota of the token. It refuses a `join` with no token, or a spent or old token. `arc-storage invite --quota 1GiB` makes a link `storage+arc://<storage key>?invite=<token>`, as section 2.2 of the storage proposal says (`docs/proposals/storage.md:162-172`). A token is random, has 128 bits, is good for one use, and ends after 7 days. |
| `ask` | It keeps the request and answers `pending`. The operator runs `arc-storage requests`, then `arc-storage accept <account> --quota 1GiB` or `arc-storage refuse <account>`. The next `join` of the signer gets the decision. A request ends after 30 days. |
| `open` | It admits each `join`, at the quota `STORAGE_OPEN_QUOTA`. It refuses a `join` if the sum of all quotas would pass `STORAGE_CAPACITY`. |

The `ask` rule needs a reply in the window of a live call. The account key
has no relay list, so the storage citizen cannot send mail to it later.
Thus `pending` is a normal answer, and the signer asks again.

The operator cannot admit friends by their citizen keys. Under #209 the
caller is an account key that no one can join to a citizen key. Thus `allow`
in the Arcfile, which matches the caller key, cannot name friends. The
invite token carries the consent instead. The operator gives the link to the
friend by hand, as text or a QR code. Open question 3 asks if a citizen can
show its citizen key in `join`, by choice, for the `ask` rule.

### 3.4 What the signer learns

The reply is one of these:

```json
{"v":1,"account":{"status":"admitted","quota":1073741824,"used":0,"plan":"free","expires":0,"tombstone_days":90,"read_days_after_close":30}}
{"v":1,"account":{"status":"pending","message":"the operator decides by hand"}}
{"v":1,"error":"refused","message":"this storage citizen admits only with an invite"}
```

After `admitted`, the signer does these steps in this order:

1. It adds the storage citizen to kind 10273 (section 5).
2. It pushes the grant set of #209 to the storage citizen.
3. It prints the quota and the plan.

## 4. Quota for each account

### 4.1 The account record

The storage citizen keeps these values for each account, in the account
state:

| Value | Rule |
| --- | --- |
| `quota` | Bytes. Set at admission. The operator changes it with `arc-storage account set <account> --quota 2GiB`. |
| `used` | The sum of the sizes of the pieces and blobs of the account, as today (`apps/storage/server/store.go:42-43`). |
| `plan` | A name that the operator chooses, such as `free` or `friend`. ARC gives it no meaning. |
| `expires` | Unix time, or 0 for no end. At the end the account closes. |
| `state` | `open`, `closing` or `closed`. |

### 4.2 Report and enforce

- The `usage` reply gains `state`, `plan` and `expires`. Its `quota` is the
  quota of the account, not of the app.
- `arc sync` prints one line for each storage citizen, such as
  `charlie: 312 MiB of 1 GiB, 4,812 pieces`.
- The put check uses the quota of the account. Today it uses the quota of the
  app (`apps/storage/server/store.go:148`, `:177`). The error text stays.
- If the operator lowers the quota below `used`, the account takes no new
  put. It keeps each read and each delete.
- A device does not place a piece on a storage citizen that has no room. It
  places the piece on the next storage citizen (section 6.5). Tahoe-LAFS
  does the same with servers that have too little space
  ([servers of happiness](https://tahoe-lafs.readthedocs.io/en/stable/specifications/servers-of-happiness.html)).

### 4.3 Close an account

`arc-storage account close <account>` sets `closing`. The storage citizen
then refuses each put, and keeps reads for 30 days. Each `open` reply and
each `usage` reply says `closing` and the end of reads. A device that sees
`closing` starts the drain of section 6.8. After 30 days the state is
`closed`, and the storage citizen removes the bytes.

### 4.4 The hook for a paid service

This proposal builds no payment. It keeps three places for one:

- `plan` and `expires` in the account record;
- an optional `offer` in a `refused` or `pending` reply, as text, such as
  `"1 GiB for one month: see https://example.org"`;
- a later field `pay` in `join`, for a token of payment.

A paid storage citizen then extends `expires` when a payment arrives.
NIP-96 plans are prior art for a quota with a name
([NIP-96](https://github.com/nostr-protocol/nips/blob/master/96.md)). Sia
locks payment and collateral in a contract
([file contracts](https://sia.tech/learn/file-contracts)). ARC needs neither
for a friend or for the own machine of the user.

## 5. Several storage citizens and kind 10273

### 5.1 The storage list

Kind 10273 is the design of section 2.2 of the storage proposal
(`docs/proposals/storage.md:186-200`) and of its decision 5. This proposal
fixes its content. It is one replaceable event. The citizen key signs it.
It carries the NIP-70 tag. A relay serves it only to its author, as the
relay rules of kinds 10274 and 10275 say (`docs/devices/SPEC.md:95-99`).
Its content is this JSON, sealed with NIP-44 to the citizen key:

```json
{"v":1,"storage":[
  {"key":"<64 hex>","name":"charlie","relays":["wss://relay.republic.sh"],"state":"active","since":1791800000},
  {"key":"<64 hex>","name":"dana","relays":["wss://relay.republic.sh"],"state":"draining","since":1791900000}
]}
```

- `state` is `active`, `draining` or `gone`.
- The list holds at most 16 storage citizens.
- Only the signer signs kind 10273. The signer refuses to sign it for a
  device, as it refuses kinds 10274 and 10275 (`docs/devices/SPEC.md:113`,
  `:145-146`). A manifest must not name it.
- `docs/KINDS.md` has no row for 10273 today (`docs/KINDS.md:39` is 10275).
  Build step 4 adds the row.

### 5.2 Pick, add and drop

- **Pick.** The user picks storage citizens. ARC does not pick them. A
  storage citizen announces the app as any provider does. The Arc Portal or
  an invite link shows it.
- **Add.** `arc storage add <link>` on the signer: `join` (section 3), then a
  new 10273 with the entry `active`, then the grant push of #209. The next
  sync of each device places new pieces on it, and adds a piece to old blobs
  (section 6.4).
- **Drop.** `arc storage rm charlie` on the signer sets `draining` in 10273.
  Each device moves the pieces off Charlie (section 6.8). When Charlie holds
  no piece, the signer sets `gone`, sends a close call that the account key
  authors, and removes the entry after 30 days.
- **Gone without notice.** If Charlie does not answer for 7 days, a device
  says so on each sync. `arc storage rm charlie --gone` on the signer sets
  `gone`. Each device then rebuilds the pieces of Charlie from the other
  pieces (section 6.8).

### 5.3 A new device

1. The device makes its device key. The signer adds it to the device list,
   and makes the data roots (`docs/devices/SPEC.md:191-206`).
2. The signer pushes a new grant set to each storage citizen of 10273.
3. The device reads 10273, and installs the app `storage` of each entry that
   is `active` or `draining`, under its `name`.
4. The device syncs (section 6.7). It lists each storage citizen, gets k
   pieces of each blob, decodes, opens and stores the records.

A citizen with only the seed derives the citizen key. That machine is then
the signer. It reads 10273 from the relays of the citizen, derives each
account key, and syncs as in step 4.

A local install of `storage` that 10273 does not name still syncs. On each
sync the device prints
`charlie is not in the storage list; run arc storage add on the signer`.

## 6. Reed-Solomon

### 6.1 The order

Encrypt, then code. The device seals each record into a blob, as section 3.3
of the storage spec says. Then it codes the blob. Then it seals each piece.
Tahoe-LAFS ([architecture](https://tahoe-lafs.readthedocs.io/en/stable/architecture.html))
and Storj ([data structure](https://storj.dev/learn/concepts/data-structure))
use the same order. A storage citizen then holds coded ciphertext. A piece is
useless alone, and k pieces give only ciphertext.

### 6.2 The unit: one blob

The unit of coding is one blob. Storj codes segments of 64 MB
([definitions](https://storj.dev/learn/concepts/definitions)). Tahoe-LAFS
codes segments of 128 KiB of a file
([file encoding](https://tahoe-lafs.readthedocs.io/en/latest/specifications/file-encoding.html)).
Their objects are files. ARC records are small, and each one is deleted,
tombstoned, counted against the quota and synced alone. All blobs are 4 KiB
to 64 KiB today (section 2). A blob is thus already the right size for one
code.

The other choice is a pack: many blobs in one object of some MiB, then one
code for the pack, as restic packs blobs. A pack hides the count and the
sizes of the records better. It also needs a repack to free the space of
deleted records. It breaks the idempotent put of a blob, which is the reason
for decision 8. It changes sync from blob IDs to pack IDs. This proposal
rejects packs for now. Open question 5 says when to revisit.

The cost of one code for each blob is many small pieces and many requests.
Section 6.12 gives the numbers, and build step 3 removes the request cost.

### 6.3 The piece

For a blob B of size |B|, the device makes these values:

```text
seal     = random                                         8 bytes
shards   = RS(k, t).Split+Encode(B)                       t shards of ceil(|B| / k) bytes
header   = blob id (32) || seal (8) || k (1) || index (1) || log2 |B| (1)
tag      = gen (4) || nonce1 (24) || AEAD(piece key, nonce1, header, aad = gen)
piece    = 0x02 || gen (4) || nonce2 (24) || AEAD(piece key, nonce2, shard[index], aad = tag)
piece id = HMAC-SHA256(piece id key of the storage citizen, blob id || index)
piece key        = HKDF-SHA256(data root of gen, info = "arc-storage-piece-v1")
piece id key     = HKDF-SHA256(keyed root, info = "arc-storage-piece-id-v1" || storage key)
placement key    = HKDF-SHA256(keyed root, info = "arc-storage-place-v1")
```

- The tag is 87 bytes (estimated: 4 + 24 + 43 + 16). The device sends it with
  the put. The storage citizen keeps it, and gives it back in each listing.
  It cannot open it.
- A piece is ceil(|B| / k) + 45 bytes (estimated: 1 + 4 + 24 + 16). The
  storage citizen sees the size class of the blob divided by k.
- The AEAD of each piece lets a device check one piece alone. A plain
  Reed-Solomon code over ciphertext cannot name a bad piece without trial
  decodes. Tahoe-LAFS keeps a hash tree of blocks for the same reason
  ([file encoding](https://tahoe-lafs.readthedocs.io/en/latest/specifications/file-encoding.html)).
- `seal` names one coding of one blob. The nonce of a blob is random
  (`runtime/selfsync/blob.go:134-135`), so two seals of one record give two
  sets of pieces that must never mix. Section 6.7 gives the rule.
- The header names k and the index, not the count of pieces. Thus a device
  can add a piece later without a change to the other pieces (section 6.4).
- For k = 1 the piece holds the whole blob. Two pieces of k = 1 can come from
  two seals. Each one rebuilds the blob alone.
- The piece key comes from the data root of the blob generation. A revoked
  device thus cannot open a tag or a piece of a newer generation. The piece
  id key and the placement key come from the keyed root, which never
  changes. A revoked device can compute piece IDs. It cannot open pieces of a
  newer generation.

The storage app does not decode, and needs no Reed-Solomon code. It keeps
opaque pieces and opaque tags. The Reed-Solomon code lives in the client,
`runtime/selfsync`.

### 6.4 The choice of k and n

Let S be the count of `active` storage citizens in 10273. The device writes a
new blob with:

- k = ⌈S/2⌉;
- a target of t = min(S, 2k) pieces, one on each of t storage citizens.

| S | k | t | Expansion | Pieces that can be lost | Reads if each storage citizen answers with p = 0.80 / 0.90 / 0.95 / 0.99 |
| --- | --- | --- | --- | --- | --- |
| 1 | 1 | 1 | 1.00 | 0 | 0.800 / 0.900 / 0.950 / 0.990 |
| 2 | 1 | 2 | 2.00 | 1 | 0.960 / 0.990 / 0.998 / 1.000 |
| 3 | 2 | 3 | 1.50 | 1 | 0.896 / 0.972 / 0.993 / 1.000 |
| 4 | 2 | 4 | 2.00 | 2 | 0.973 / 0.996 / 1.000 / 1.000 |
| 5 | 3 | 5 | 1.67 | 2 | 0.942 / 0.991 / 0.999 / 1.000 |
| 6 | 3 | 6 | 2.00 | 3 | 0.983 / 0.999 / 1.000 / 1.000 |

The last column is computed: the chance that at least k of t storage
citizens answer, if each answers with chance p. It is availability at the
time of a sync, not durability. Each device keeps a full copy, so a storage
citizen that is offline for an hour costs nothing. The column that drives
the rule is "pieces that can be lost": a permanent loss of storage citizens
before repair. The rule keeps the expansion at 2 or less, and lets the
citizen lose ⌊t/2⌋ storage citizens. MinIO uses half data and half parity by
default for the same reason
([erasure README](https://github.com/minio/minio/blob/master/docs/erasure/README.md)).

The table also shows a cost. Two of three reads worse than one of two at
each p. The rule takes 2-of-3 because it stores 1.5 times the bytes, not 2
times, and loses one storage citizen in both cases. Open question 6 asks
about it.

With one storage citizen the code is k = 1: one whole copy, as today. With
two it is k = 1 with two copies: a mirror. Erasure coding starts at three.

**A new storage citizen adds a piece, with no new coding.** With the default
matrix of `klauspost/reedsolomon` v1.14.2, the shards of a (k, n) code are
the first n shards of a (k, m) code for m > n (measured, section 6.11). A
blob of k = 2 with three pieces thus gets a fourth piece from a decode of two
pieces and one encode. The other three pieces stay. The library does not
promise this property. The design therefore does not depend on it. It only
saves bytes. Build step 6 pins the library version, and keeps a test of the
property.

A blob keeps its k for its life. If S grows from 4 to 5, the rule says k = 3
for new blobs. An old blob of k = 2 keeps k = 2, and keeps t = min(5, 4) = 4
pieces. If S falls below the k of a blob, the device codes the blob again
with the k of the new S (section 6.8).

### 6.5 Placement

The device ranks the active storage citizens for each blob by
`HMAC(placement key, blob id || storage key)`, highest first. This is
rendezvous hashing, as the permuted list of Tahoe-LAFS
([architecture](https://tahoe-lafs.readthedocs.io/en/stable/architecture.html)).
Each blob gets its own order, so the pieces spread over the storage
citizens. The placement key is secret, so a storage citizen cannot predict
the order.

The rank does not fix the index of a piece. When a device must place index i,
it takes the highest storage citizen in the order that holds no piece of the
blob, has room, and answers. Thus a storage citizen that leaves moves no
other piece.

### 6.6 The piece map

The piece map says which storage citizen holds which piece of which blob.
No device writes it. Each device builds it from the listings:

- Each `list` entry gains the tag: `{"seq":7,"id":"…","size":2093,"tag":"<base64>"}`.
- The device opens each tag of an ID that it does not know. The tag gives
  the blob ID, the seal, k and the index.
- The device keeps the map as a local cache, beside the cursor of each
  storage citizen (`docs/storage/SPEC.md:163`). It can always rebuild the
  cache from the listings.

A separate map record that each device edits would need merge rules for two
devices that change it at the same time. A map that each device derives
needs none. The storage citizen holds the tag, but cannot read it. The tag
is sealed, and only the citizen opens it. This is how the piece map syncs
between devices: each device reads the same listings.

### 6.7 Write, read and sync

**Write.** For a record that no storage citizen holds, the device seals a
blob, codes it, and puts t pieces on t storage citizens, in the order of
section 6.5. It puts the pieces of one blob in one batch, with one seal.

**One seal for each blob.** A device must not mix pieces of two seals. The
rule:

1. If the listings show k or more pieces of one seal, the device keeps that
   seal. It adds missing pieces from a decode of that seal.
2. If the listings show pieces but fewer than k of each seal, and the device
   holds the record, it seals again, and puts t new pieces. Then it deletes
   the pieces of the old seals.
3. If the listings show no piece, the device writes as above.

Two devices that put one record at the same time can make two seals. The
piece ID does not depend on the seal, so the second put of an index on one
storage citizen gets `held`. The pieces of two seals can then mix across
storage citizens. Rule 2 then repairs the blob on the next sync. This case
is rare: a record starts on one device, and other devices get it from a
storage citizen.

**Read.** For a blob that the device lacks, it gets k pieces of one seal
from the storage citizens that answer first, as Storj downloads only the
pieces that it needs
([definitions](https://storj.dev/learn/concepts/definitions)). It opens
each piece with its tag as aad. It checks that
`HMAC(piece id key, blob id || index)` is the ID that it asked for. It
decodes, and opens the blob as section 3.4 of the storage spec says
(`docs/storage/SPEC.md:121-137`).

**Sync.** Section 4.1 of the storage spec stays, for each storage citizen
(`docs/storage/SPEC.md:150-166`). The changes:

- The device syncs all storage citizens at the same time, not one after
  another.
- Step 2 lists each storage citizen, and the device builds the piece map.
- Step 3 gets k pieces of each blob that the device lacks, from several
  storage citizens.
- Step 4 puts the pieces of each record that the piece map lacks.
- A delete goes to each storage citizen that holds a piece of the blob.
  Each storage citizen keeps its tombstone for each piece ID.

### 6.8 Repair

Each device that runs `arc serve` repairs on its 15-minute tick. A device
repairs on `arc sync` only with `--repair`. A phone thus repairs only on
request. Two devices that repair at the same time make the same index. The
piece ID is the same on one storage citizen, so the second put gets `held`.
On two storage citizens, the blob gets one extra piece. The next sync deletes
the piece on the lower storage citizen in the order. This costs bytes once,
and loses nothing.

| Event | What the device does | Bytes for each blob |
| --- | --- | --- |
| A storage citizen is `draining` | It gets the piece from the draining storage citizen, puts the same bytes on the new storage citizen under the piece ID of that storage citizen, and deletes the piece on the draining one. | Down: one piece. Up: one piece. |
| A storage citizen is `gone`, or lost a piece | It gets k pieces of the seal, decodes, encodes the missing index, and puts it on the next storage citizen of the order. | Down: one blob (k pieces). Up: one piece. |
| k = 1, and the device holds the record | It seals the record again, and puts the whole blob. | Down: none. Up: one blob. |
| S fell below the k of a blob | It seals the record again with the new k, puts the new pieces, and deletes the old ones. | Up: t/k of one blob. |

A device repairs the blobs with the fewest spare pieces first, as the repair
queue of Storj does
([file repair](https://storj.dev/learn/concepts/file-repair)).

A device must not repair through TURN by default. Repair waits for a direct
path. `arc storage repair --turn` allows TURN. Section 6.10 explains why.

### 6.9 Audits

A device checks that a storage citizen still holds its pieces. Each piece
is small, and the device holds every key. So the audit is simple: get a
random piece, and open it.

- On each tick of `arc serve`, the device picks s pieces of each storage
  citizen at random from the piece map, gets them, and opens them with their
  tags. A piece that is absent, or does not open, is a failure.
- A failure marks the piece as lost in the local cache. Repair rebuilds it.
- Three failures in one day mark the storage citizen as suspect. `arc sync`
  then says so. The device does not drop the storage citizen by itself. The
  user decides with `arc storage rm`.

With s pieces of one storage citizen that lost a fraction f of its pieces,
one round finds the loss with chance 1 − (1 − f)^s. With s = 32 a day
(computed):

| f | One day | 30 days (960 samples) |
| --- | --- | --- |
| 10% | 0.966 | 1.000 |
| 1% | 0.275 | 0.99994 |

The cost is 32 pieces a day for each storage citizen. With k = 2 or more, a
piece is at most 32 KiB, so the cost is at most about 1 MiB a day. With
k = 1, a piece is a whole blob of at most 64 KiB, so the cost is at most
about 2 MiB a day (estimated, from the largest blob of 64 KiB).

Storj audits a random stripe, and finds a bad share with the
Berlekamp-Welch algorithm, because the satellite holds no key
([audit service](https://github.com/storj/storj/wiki/audit-service)). Sia
asks for a Merkle proof of a random leaf at the end of a contract
([file contracts](https://sia.tech/learn/file-contracts)). Filecoin proves
storage with Proof-of-Spacetime
([proofs](https://docs.filecoin.io/core-concepts/filecoin-virtual-machine/proofs)).
ARC needs none of these. The device holds the keys, so the AEAD of a piece is
the proof. A storage citizen that keeps a piece only on another machine, and
fetches it for each audit, passes. A friend or the own machine of the user
has no reason to do that. A paid storage citizen can need a stronger proof
later.

### 6.10 The cost of repair on the pipe

For a citizen with 1 GiB of blobs, S = 5, k = 3 and t = 5, each storage
citizen holds about 341 MiB (computed: 1,024 MiB / 3).

| Event | Down | Up | Direct, 97.0 Mbit/s | TURN, 9.1 Mbit/s |
| --- | --- | --- | --- | --- |
| Rebuild a storage citizen that is gone | 1,024 MiB | 341 MiB | 118 s | 1,259 s |
| Drain a storage citizen | 341 MiB | 341 MiB | 59 s | 629 s |
| Mirror (S = 2, k = 1), the device holds the records | 0 | 1,024 MiB | 89 s | 944 s |

All times are computed, from the measured rates of section 2. The session
that asked for this proposal reported 58 Mbit/s for a slow real link and
6.5 Mbit/s for TURN. This proposal did not check those values. At 58 Mbit/s
the rebuild takes 197 s (computed).

A rebuild through TURN moves 1,365 MiB. That is more than the TURN quota of
1 GiB a day. Thus repair through TURN takes two days, and blocks other TURN
use. This is why repair waits for a direct path by default.

### 6.11 Measured speed of the code

`klauspost/reedsolomon` v1.14.2, Apple M3 Max, arm64 (NEON), go1.27.1,
`-benchtime 300ms`, on 2026-10-12. The loop of encode includes `Split`, and
the loop of decode includes `Join`, so the values are low. Each value is
throughput in blob bytes. The first value is one core (`-cpu 1`), like a
phone. The second is 16 cores. The decode loses data shards first, which is
the worst case.

| Code | Work | 4 KiB | 64 KiB | 1 MiB | 64 MiB |
| --- | --- | --- | --- | --- | --- |
| 2-of-3 | encode | 5.1 / 5.3 GB/s | 5.7 / 5.6 GB/s | 5.5 / 11.2 GB/s | 6.5 / 46.4 GB/s |
| 2-of-3 | decode, 1 lost | 4.0 / 3.8 GB/s | 4.6 / 4.2 GB/s | 5.3 / 9.8 GB/s | 5.8 / 22.8 GB/s |
| 2-of-4 | encode | 2.9 / 2.9 GB/s | 3.2 / 2.6 GB/s | 3.5 / 7.4 GB/s | 4.2 / 29.9 GB/s |
| 2-of-4 | decode, 2 lost | 2.6 / 2.1 GB/s | 2.8 / 2.0 GB/s | 3.0 / 5.9 GB/s | 3.7 / 17.1 GB/s |
| 3-of-5 | encode | 5.4 / 4.4 GB/s | 7.9 / 5.5 GB/s | 10.5 / 10.8 GB/s | 21.8 / 40.3 GB/s |
| 3-of-5 | decode, 2 lost | 2.8 / 2.7 GB/s | 3.8 / 3.3 GB/s | 4.9 / 8.2 GB/s | 5.3 / 21.7 GB/s |
| 3-of-6 | encode | 4.2 / 3.0 GB/s | 6.3 / 3.6 GB/s | 9.0 / 7.9 GB/s | 15.7 / 31.5 GB/s |
| 3-of-6 | decode, 3 lost | 2.2 / 1.9 GB/s | 2.9 / 2.1 GB/s | 3.6 / 7.0 GB/s | 4.2 / 18.3 GB/s |
| 4-of-6 | encode | 5.1 / 4.2 GB/s | 5.4 / 5.5 GB/s | 6.1 / 9.9 GB/s | 8.3 / 52.1 GB/s |
| 4-of-6 | decode, 2 lost | 3.8 / 3.0 GB/s | 5.7 / 4.7 GB/s | 6.4 / 9.1 GB/s | 7.1 / 24.7 GB/s |

The slowest value is 1.9 GB/s (measured). A direct pipe of 97.0 Mbit/s moves
about 12 MB/s (computed). The code is thus at least 150 times faster than the
pipe (computed). It is never the bottleneck. One blob of 4 KiB takes 0.5 to
2.2 µs to encode or decode (measured).

The same run checked the prefix property of section 6.4. For (k, n, m) =
(1, 1, 2), (2, 3, 4), (2, 3, 6), (3, 5, 6) and (3, 4, 8), the n shards of
the (k, n) code were equal to the first n shards of the (k, m) code
(measured).

A second check sealed one file part with the code of
`runtime/selfsync/blob.go` (measured):

| Text in the part | Event content | Blob |
| --- | --- | --- |
| 100 bytes | 260 bytes | 4,096 bytes |
| 2,000 bytes | 2,820 bytes | 4,096 bytes |
| 8,000 bytes | 11,012 bytes | 16,384 bytes |
| 32,768 bytes | 43,780 bytes | 65,536 bytes |

The checks ran as a Go module in a scratch directory, and as a test file in
`runtime/selfsync` that was removed after the run. Neither is in this PR.

### 6.12 The cost of many small objects

These costs exist today. Erasure coding makes them t times larger on the
count of objects. Each one is estimated.

- **Round trips.** A put takes two round trips: the request and `send`,
  then the bytes and `stored` (`docs/storage/SPEC.md:80`). At 44 ms, 1,000
  puts take 88 s, whatever their size. A blob with t = 5 pieces costs five
  puts, on five pipes. If the pipes run at the same time, a storage citizen
  still takes 1,000 puts for 1,000 blobs.
- **The account state.** Each change rewrites `state.json`, which holds one
  entry of about 130 bytes for each piece. With 10,000 pieces, each put
  writes about 1.3 MB. 10,000 puts then write about 13 GB.

Build step 3 sends several requests before the first reply, and stores the
index of an account so that a put does not rewrite the whole index. The
harness of `DIRECT_LINK_RTT_MS` measures the result with a real round trip
(`docs/direct/SPEC.md:335`).

## 7. What each party sees

| Party | Sees | Does not see |
| --- | --- | --- |
| One storage citizen | The account key. The device keys of the grant set. The count of pieces. The size of each piece, which is the size class of its blob divided by k, plus 45 bytes. The generation of each piece. The time of each put and each session. The IP address of the device, unless TURN carries the bytes. | The citizen key. Which pieces form one blob. The blob IDs. The tags. The content. The other storage citizens. |
| Two or more storage citizens that work together | All of the above for each. The piece IDs differ on each storage citizen, so they do not join by ID. They can join by time: a device puts the pieces of one batch to each storage citizen at the same time, with the same sizes. They can join by IP address. | The content, unless k of them work together and also get the data root. They hold ciphertext. |
| The relay | The sealed 10273 of the citizen, its size and its time. One call to each storage citizen at each sync, at about the same time. With STUN, the public addresses. With TURN, the volume. | The storage citizens in 10273. The account keys of other calls. The pieces. |
| A passive observer of the network of a device | S encrypted WebRTC flows at each sync, their sizes and times. | The content, and the storage citizens behind TURN. |

**Correlation risks.**

1. **The shared id key.** Today each storage citizen of one citizen sees the
   same blob IDs (`docs/proposals/storage.md:297-300`). This design gives
   each storage citizen its own piece id key, so two storage citizens see no
   common ID. Decision 8 kept one id key so that a device can copy a blob
   between storage citizens without opening it
   (`docs/proposals/storage.md:599-601`). With Reed-Solomon each storage
   citizen holds a different piece, so no device copies a blob. Open
   question 1 asks to revise decision 8.
2. **Sizes.** Each piece of one blob has the same size, on each storage
   citizen. Padding to a power of two keeps the classes coarse
   (decision 9). The size ratio tells a storage citizen roughly what k is.
3. **Timing.** A device puts pieces in batches on a tick, not after each
   write (decision 10). The batch is the same on each storage citizen.
   Nothing here hides that.
4. **IP address.** Section 0 of the storage proposal states this limit
   (`docs/proposals/storage.md:22-38`). It stays.
5. **Generation.** `gen` stays in clear, so each storage citizen sees when
   the citizen revoked a device (`docs/proposals/storage.md:284-286`).

## 8. Migration and compatibility

### 8.1 What exists

- Blobs of format 1 (`docs/storage/SPEC.md:93-119`), sealed under generation
  0, on one storage citizen or a few, each a full copy.
- After #209, accounts named by account keys. The note of #209 says that the
  first sync puts each blob again into the new account.

### 8.2 The change

- A listing entry with no tag is a blob of format 1. A new device reads it
  as a piece of k = 1, and checks it as today.
- A new device asks each storage citizen `{"op":"hello"}` first. A new app
  answers `{"v":2,"features":["pieces","batch"]}`. An older app answers
  `invalid_request` (`apps/storage/server/server.go:340`). The device then
  puts only blobs of format 1 on that storage citizen, as a full copy, and
  prints `charlie runs an older storage app: it holds full copies; update it
  for erasure coding`. The check comes first, because an older app ignores
  the field `tag` and would store a piece with no tag.
- On the first sync with two or more storage citizens that support pieces,
  a device codes each record that it holds into pieces, puts them, and then
  deletes the blob of format 1. This is one upload of t/k of the data
  (computed: at most 2 times the bytes).

### 8.3 An older device

- An older device lists piece IDs. A piece does not open as a blob of
  format 1. Section 3.4 of the storage spec then skips it after the first
  refusal (`docs/storage/SPEC.md:135-137`). The older device does not see
  data that a newer device wrote as pieces. The user updates it.
- An older device puts its own records as blobs of format 1. A newer device
  reads them, codes them, and deletes the blob. The tombstone of 90 days
  then answers each new put of that blob with `tombstoned`
  (`apps/storage/server/store.go:18`).
- An older device does not read kind 10273. It syncs with its local installs,
  as today.

### 8.4 An older storage citizen

- It answers `invalid_request` to `join`, `hello` and the batch requests.
  The signer then says
  `charlie runs an older storage app: it cannot admit accounts; use STORAGE_OWNERS`.
- It keeps the quota of the app. A device reads `quota` from `usage`, as
  today.

### 8.5 The spec

The storage spec is `partial` (`docs/storage/SPEC.md:3`). Its sections 3.1,
3.2, 3.3, 4.1, 4.3, 6 and 7 change. An older device cannot read data that a
newer device writes. Thus the commit of build step 6 is `feat!:` with a
`BREAKING CHANGE` footer. Kind 10273 needs a row in `docs/KINDS.md` and a
section in the storage spec. Each step runs `mise run specs`.

## 9. Build steps

Each step is one PR. Each step names what proves it, through the real CLI.

| # | Step | Proof |
| --- | --- | --- |
| 0 | Seal blobs with the data roots: replace the body of `sealSelf` (`runtime/citizen/selfsync.go:49-58`) with the key of `DataRoot`, as the #204 note planned. | A proof test: revoke a device, write a page, sync. The revoked device cannot open the new blob. |
| 1 | #209: per-device grants. In progress in another session. | Its own tests. |
| 2 | Admission and quota for each account: `join` with the rules `invite`, `ask` and `open`; the account record; `usage` with the quota of the account; `arc storage add`; `arc-storage invite`, `requests`, `accept`, `refuse`, `account set`, `account close`. | A proof test with a real relay and `arc-storage`: a `join` with no token is refused; a `join` with a token is admitted at its quota; a second token use is refused; under `ask`, `pending`, then `accept`, then `admitted`; a put over the quota of the account fails while another account has room. |
| 3 | Scale the storage app: several requests in flight on one pipe, and an index that a put does not rewrite whole. Open question 8 asks where the index lives. | The `DIRECT_LINK_RTT_MS` harness with 44 ms: 1,000 puts of 4 KiB, before and after. The PR states both times. |
| 4 | Kind 10273: the signer publishes it; each device reads it and installs each storage citizen; `arc storage ls` and `rm`; a row in `docs/KINDS.md`. | A home with only the seed finds two storage citizens and reads every page. |
| 5 | Several storage citizens with full copies (k = 1), synced at the same time. | Two storage citizens. Stop one. A new device reads every page from the other. |
| 6 | Reed-Solomon: the piece format, tags in `put` and `list`, `hello`, placement, decode on read, the piece id key for each storage citizen. Pin `klauspost/reedsolomon`, and keep a test of the prefix property. Breaking. | Three storage citizens, k = 2. Delete the directory of one. A new device reads every page. No piece file holds text of a page. Each piece file is about half its blob. |
| 7 | Repair, drain and audits. | Remove a storage citizen with `arc storage rm --gone`. `arc serve` rebuilds its pieces on a fourth one. Change one byte of a piece file. The next audit finds it, and repair replaces it. A drain empties a storage citizen, and the data stays readable. |
| 8 | Migration of blobs of format 1. | A home with blobs of format 1 on two new storage citizens ends with pieces only. An older `arc` still syncs, and its puts get `tombstoned` or are coded again. |

Steps 0, 2 and 3 do not depend on each other. Step 4 needs step 2. Steps 5
to 8 run in order.

## 10. Prior art

| System | What it shows for this design |
| --- | --- |
| [Storj](https://storj.dev/learn/concepts/definitions) | Encrypt, then code segments of 64 MB with Reed-Solomon 29-of-80. Download stops after 29 pieces. Repair starts at a threshold of 54 pieces ([file repair](https://storj.dev/learn/concepts/file-repair)). The satellite audits a random stripe with Berlekamp-Welch ([audit service](https://github.com/storj/storj/wiki/audit-service)). A central satellite keeps the piece map. ARC has no satellite, so each device derives the map. |
| [Tahoe-LAFS](https://tahoe-lafs.readthedocs.io/en/stable/architecture.html) | The closest system: friends run servers. Encrypt, then code 3-of-10 by default. On a small grid, N is about the count of servers. A permuted list spreads shares. Servers of happiness counts distinct servers, not shares ([servers of happiness](https://tahoe-lafs.readthedocs.io/en/stable/specifications/servers-of-happiness.html)). A hash tree checks each block ([file encoding](https://tahoe-lafs.readthedocs.io/en/latest/specifications/file-encoding.html)). |
| [Sia](https://devs.sia.storage/docs/core-concepts/erasure-coding) | 10-of-30 by default, with each shard on another host. Contracts lock payment and collateral, and the host proves a random leaf with a Merkle proof ([file contracts](https://sia.tech/learn/file-contracts)). The hook of section 4.4 leaves room for this. |
| [Filecoin](https://docs.filecoin.io/core-concepts/filecoin-virtual-machine/proofs) | Proof-of-Replication and Proof-of-Spacetime prove storage to a chain. ARC has no chain and no stranger to convince, so the AEAD of a piece is enough. |
| [Backblaze Vaults](https://www.backblaze.com/blog/vault-cloud-storage-architecture/) | 17 data shards and 3 parity shards on 20 pods. Reed-Solomon from their own Java code, which `klauspost/reedsolomon` ports. |
| [MinIO](https://github.com/minio/minio/blob/master/docs/distributed/DESIGN.md) | Uses `klauspost/reedsolomon`. Erasure sets of 2 to 16 drives. Half data and half parity by default. |
| [klauspost/reedsolomon](https://github.com/klauspost/reedsolomon) | Pure Go with assembly for amd64 and arm64. Section 6.11 measures it. Storj uses its own library, `infectious`; it is the other choice. This proposal takes `klauspost` for its speed, its users and its maintenance. |

## 11. Recommendation

Build the storage network on what exists: the account key of #209, the blob
of format 1, the direct pipe and the cursor sync. Add three things:

1. a `join` call with three rules of admission, and a quota for each account;
2. the sealed list of kind 10273, which only the signer writes;
3. Reed-Solomon on each blob, k = ⌈S/2⌉ and t = min(S, 2k), with a sealed tag
   for each piece in the listings, so that each device derives the piece map.

Do step 0 first. Without it, a revoked device opens every piece.

## 12. Open questions for Zim

Each question has a recommended answer.

1. **Each storage citizen gets its own piece id key. This revises decision
   8.** Recommended: yes. With Reed-Solomon, no device copies a blob between
   storage citizens, so the reason for decision 8 is gone. Two storage
   citizens then see no common ID.
2. **The default rule of admission.** Recommended: `invite`. It needs no
   operator at the time of the request, and the token carries the consent
   that `allow` can no longer carry.
3. **Can a citizen show its citizen key in `join`, by choice, so that a
   friend under the `ask` rule knows who asks?** Recommended: yes, as an
   optional signature of the citizen key over the account key and the
   storage key. The default sends none.
4. **Who repairs?** Recommended: each device that runs `arc serve`, on its
   tick. A phone repairs only on `arc sync --repair`. Two repairs at the same
   time cost one extra piece, which the next sync deletes.
5. **One code for each blob, or packs?** Recommended: one code for each blob
   now. Revisit packs if an account passes 100,000 pieces, or if hiding the
   count of records becomes a goal.
6. **The rule for k.** Recommended: k = ⌈S/2⌉ and t = min(S, 2k). This keeps
   the expansion at 2 or less, and loses ⌊t/2⌋ storage citizens. The other
   choice for S = 3 is a mirror of three copies: it reads better and loses
   two, at 3 times the bytes.
7. **May repair use TURN?** Recommended: no by default. A rebuild of 1 GiB
   needs more than the TURN quota of one day. `--turn` allows it.
8. **Where does the index of an account live in the storage app?**
   `go.etcd.io/bbolt` is a dependency of the repository (`go.mod:20`). The
   rules put Bolt implementations under `adapters/`, and an app imports only
   `sdk/`. Recommended: the storage app uses `bbolt` directly as a library
   for its own disk, as it uses `sdk/atomicfile` today. It is the private
   disk format of one app, not an ARC interface. The other choice is a log
   that the app appends to, in plain files.
9. **File parts make blobs of 64 KiB, 2.67 times the file bytes.**
   Recommended: keep decision 11 for this work. Open a separate issue to
   store a file as one large blob of its raw bytes, with no base64 inside
   NIP-44. Reed-Solomon then codes the large blob with no change.
10. **How long does a device wait before it calls a silent storage citizen
    gone?** Recommended: it warns after 7 days, and only the user marks it
    `gone`. A home machine that is off for a holiday must not cost a
    rebuild.
11. **How many pieces does an audit sample?** Recommended: 32 for each
    storage citizen each day. This costs at most about 1 to 2 MiB a day, and finds
    a loss of 1% of the pieces in 30 days with a chance of 0.99994.
