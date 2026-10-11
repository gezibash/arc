Status: proposed

# Storage citizens: self data off the relay

This proposal answers issue #172. It moves the data that a citizen keeps for
itself off the relay.

Storage is a role. A citizen serves the role with the storage app, as another
citizen serves the transfer app. A storage citizen holds encrypted bytes for
another citizen, with a quota and with consent. It cannot read them. Any
citizen can serve the role: the user's own Mac, a friend, or a paid operator.
ARC adds no new kind of party. It is still only citizens and the relay.

The journal and the other data apps become local apps. They keep their data
in the local store. Sync is a choice. If the user chooses it, the device
encrypts the data, and sends opaque bytes over the direct pipe (#190) to a
storage citizen. The next device fetches the bytes and decrypts them. The
relay only introduces the two sides.

Nothing in this document is built. No production code changes with it.

## 0. What a storage citizen does not fix

Read this first.

- A storage operator sees the count, the sizes and the times of the blobs, as
  the relay operator does today. Every system in section 2.10 leaks the same
  three things. A storage citizen changes who sees them, and it keeps the
  relay small. It does not hide them. Padding (section 2.4) hides exact sizes
  only.
- A storage citizen that the relay operator runs under a second key gains
  little privacy. The same operator then sees the relay traffic and the
  storage traffic, and can join them by IP address and time.
- A direct path needs both machines online at the same time. Two devices that
  sleep, such as a phone and a laptop, seldom meet. Sync between them needs a
  third machine that is always on. That machine is the storage citizen.
- If the user has no storage citizen, nothing syncs. The data stays on the
  device. A lost device then loses the data that it alone held.

## 1. Use cases

### 1.1 The data today

These facts come from the code and the specs at v0.23.1.

| Data | Kind inside | Carried as | Source |
| --- | --- | --- | --- |
| Journal page, notebook index | 30023 | NIP-37 draft 31234, checkpoint 1234 for each revision, parts 3275 past 32 KiB | `apps/journal/manifest.json`, interface spec 3.9 |
| Journal KPI | 30078 | Draft 31234 | `apps/journal/manifest.json` |
| File | 1063 | Draft 31234 and parts 3275. The content is base64, so one file holds about 6 MiB at most (257 × 32 KiB of text). | `apps/files/manifest.json`, interface spec 3.9.2 |
| Keyed root | 30078 | Draft 31234 with `d` = `arc-keyed-root` | interface spec 3.7.1 |
| Relay list for drafts | — | 10013, content sealed | `core/draft` `RelayList`, delivery spec 4.11 |

`arc sync` reconciles these kinds with every relay of the citizen, or with a
directory (`sealedFilter` in `cmd/arc/main.go`). Each draft, checkpoint and
part is signed by the citizen key and carries the NIP-70 tag. An ARC relay
serves these kinds only to their author after NIP-42 authentication
(`adapters/relay/sealed`). A standard Nostr relay can serve them to anyone,
because NIP-37 only recommends that rule. In both cases the relay operator
sees the author, the count, the times and the sizes.

On 2026-10-07, 1,000 journal pages held 16 MB on a relay after #160.

### 1.2 Two devices of one citizen

A citizen writes in the journal on a Mac, and reads it on a phone. Today both
devices sync through the relay. With a storage citizen, each device syncs with
the storage citizen. The devices never need to be online at the same time.

If the two devices are online at the same time, they can sync directly. A
device of the same citizen can serve the storage role for the other device.
Section 2.6 applies to it as it is.

### 1.3 A new device after a lost phone

This is the story of #173. A citizen loses the phone on a trip. By evening, a
new phone reads the journal and gets mail. The old phone is dead to everyone.

The new phone needs these things, in this order:

1. A device key. The phone makes it.
2. A place in the device list. The citizen key signs the new device key into
   the list, on the signer or from the 24-word seed (#173). The same step
   revokes the old phone, and makes a new generation of the data root.
3. The addresses of the storage citizens. Section 2.2 says where these come
   from.
4. A grant for each storage citizen. The signer gives each storage citizen a
   grant for the new device key, and a revocation of the old one (section
   2.3).
5. A sync with one storage citizen. The phone first gets the wraps of the
   data root for its device key, and opens them. Then it gets every blob of
   the account.

The storage citizen holds the wraps, but it cannot open them. The old phone
keeps what it cached. The storage citizen refuses it from the moment it gets
the revocation.

### 1.4 Backup

A backup is a second storage citizen. The device syncs with each storage
citizen in turn. Each one holds the same set of blobs, so each one is a full
copy. If one storage citizen withholds or loses blobs, the other one shows
the difference (section 2.9).

### 1.5 Who serves the storage role

| Storage citizen | Good | Bad |
| --- | --- | --- |
| A machine of the user, such as a Mac mini at home or a virtual machine they run | Full trust. No money. No quota of someone else. Works now. | Many people have none. A home connection can be slow or behind a hard NAT. |
| A friend | No money. Mutual: each holds the data of the other. | Friends turn machines off. Nobody gives a friend an SLA. |
| A paid operator | Always on. A clear contract and a quota. | The operator sees count, sizes, times and IP addresses. Somebody must build payment. |
| The relay operator, under a second key | One deploy. | Little privacy gain, see section 0. The relay and the storage fail together. |

**Decision (Zim, 2026-10-11).** Start with the user's own machine as the
storage citizen, with no billing. A paid storage citizen on a host apart from
the relay is the end state for a user with no machine that is always on. A
friend is a good second storage citizen, not a good only one.

## 2. The protocol shape

### 2.1 Sealing: what this document takes from #173

Sealing is the work of #173 (PR #192, `docs/proposals/base.md`). This
section states what that design gives the storage citizen. If #173 changes,
sections 2.3, 2.4 and 2.8 change.

- The citizen key is the identity. The signer is a machine that holds it.
  The 24-word seed of NIP-06 restores it on any machine. ARC has no seed
  today; #192 adds it. A phone that the user restores from the seed holds the
  citizen key, and is its own signer.
- Each device has its own device key. The device list names the device keys.
  The citizen key signs the list. The list is sealed to the citizen.
- The data root is a random key of 32 bytes with a generation number. The
  data of the citizen is encrypted with a key that HKDF derives from it.
- A wrap is the data root of one generation, sealed to one device key or to
  the citizen key. One wrap is a signed event of 565 bytes. The wrap to the
  citizen key lets the citizen key recover every generation.
- To revoke a device, the citizen makes a new generation and wraps it only
  for the devices that remain. Each new generation carries the previous data
  root, sealed under the new one, so the newest root opens the chain. A
  revoked device never gets a newer generation.
- Generation 0 is the drafts of today, sealed to the citizen key.
- The signer also wraps the keyed root of interface spec 3.7.1 to each
  device. That key never changes. Section 2.4 derives the blob IDs from it.

The storage citizen never needs a data root. It stores sealed blobs and
wraps, and it checks device keys against grants.

### 2.2 Find and pair

A storage citizen is a citizen that serves the `storage` app. It announces
the app as any provider does (delivery spec 4.7). Discovery does not make it
usable. Each side consents first.

1. The storage operator makes an invite: `arc-storage invite --quota 1GiB`.
   The invite is a link, `storage+arc://<key of the storage citizen>?invite=<token>`.
   The token is random, is good for one use, and names the quota.
2. The operator gives the link to the citizen, as text, a QR code or a
   message. If the storage citizen is the user's own machine, the user is the
   operator.
3. The citizen runs `arc storage add <link>`. The device sends one ARC call to
   the storage citizen. The call holds the token, the account key, and a
   grant for the device key. The signer signs the grant.
4. The storage citizen checks the token, opens the account with the quota,
   and replies. The token is now spent.

The device keeps the link and the account in its local configuration. A new
device learns the storage citizens in one of two ways:

- From the signer, when it adds the new device. The signer holds the link of
  each storage citizen, beside the device list.
- From the seed, then the sealed storage list on the relays, when the signer
  is lost. The seed holds only the citizen key, because a link can change and
  the seed cannot.

A new device gets its data roots the same way: from the signer, wrapped to
its device key, or from the seed, which opens the wraps to the citizen key.

**The storage list.** It is one replaceable event of kind 10273, an ARC kind.
The signer publishes it to the relays of the citizen. Its content is the list
of storage links, sealed to the citizen key with NIP-44. It carries the
NIP-70 tag. It stays on the relay for these reasons:

- It is coordination data. It says where to reach the data of a citizen, as
  the relay lists of NIP-65 and NIP-17 say where to reach the citizen.
- It is the only way back for a user who has the seed and nothing else. A
  seed cannot hold a link that changes.
- It is small and bounded: one event for each citizen, a few hundred bytes,
  replaced in place.

The relay sees that the citizen has a storage list, its size and its time. It
does not see the storage citizens. Kind 10013 goes. Kind 10273 does not
replace it: 10273 names storage citizens, not relays for drafts.

### 2.3 The account, consent and the quota

The storage citizen knows an account by an account key. The citizen does not
show its citizen key to the storage citizen. The signer derives one account
key for each storage citizen:
`HKDF(citizen secret key, info = "arc-storage-account-v1" || key of the storage citizen)`.
Two storage citizens see two account keys that they cannot join. Only the
signer holds the secret half, so a device cannot make an account key.

The account key signs these records, and the signer gives them to the
storage citizen:

| Record | Meaning |
| --- | --- |
| Grant | A device key can read and write in this account. |
| Revocation | A device key can no longer do anything in this account. |

A device opens each session with its device key and its grant. The storage
citizen refuses a device key with no grant, or with a revocation. It never
sees the device list, nor the citizen key.

The storage citizen keeps these values for each account:

| Value | Rule |
| --- | --- |
| Quota in bytes | The storage citizen refuses a put that passes it. The error names the bytes in use and the quota. |
| Bytes in use | The sum of the padded sizes of the blobs. |
| Last sync | The time of the last session. |
| Tombstones | The IDs of deleted blobs, for 90 days. See section 2.7. |

Consent works in both directions:

- The storage citizen consents with the invite. The operator can close an
  account. The storage citizen then tells the client on its next session,
  and refuses puts. It keeps reads for 30 days, so the citizen can move the
  data.
- The citizen consents with `arc storage add`. The citizen can remove a
  storage citizen with `arc storage rm`. The device then deletes every blob
  of the account, and forgets the storage citizen.

### 2.4 What the storage citizen stores

The storage citizen stores opaque blobs that it cannot read.

```text
blob     = id || ts || gen || AEAD(blob key, nonce, pad(record))
id       = HMAC-SHA256(id key, record ID)              (32 bytes)
ts       = 0                                           (8 bytes)
gen      = generation of the data root                 (4 bytes)
blob key = HKDF(data root of gen, info = "arc-storage-blob-v1")
id key   = HKDF(keyed root, info = "arc-storage-id-v1")
```

- A record is one item of the local store: a journal page, an index, a KPI,
  a file, a keyed value, or a deletion. The device encrypts it before it
  leaves the device. The next device decrypts it, and gives it to its store.
- In the first version, a record is the signed event that the store holds
  today, and its record ID is the event ID. `core/draft` and the store rules
  for deletion work as they are. The citizen key signs each record, so a
  revoked device, which holds old data roots, cannot forge a record. Open
  question 4 asks whether a local record still needs this form.
- The outer seal hides everything about the record: its signer, its kind,
  its keyed `d` tag and its generation.
- The ID is a keyed hash of the record ID. Two devices that put the same
  record make the same blob. A put is therefore idempotent, as in Tarsnap.
  The keyed root never changes, so a new generation does not change the ID of
  an old blob. A citizen who has only generation 0 has a keyed root too.
- Negentropy sorts items by a timestamp, then by ID. Both sides must use the
  same timestamp. The device sets `ts` to 0 for each blob, as Syncthing sets
  one fixed time on an untrusted device. Reconciliation then splits the set by
  ID only, and the blob shows no time of writing.
- `gen` is in the clear, so a device knows which data root opens the blob. The
  storage citizen learns when the citizen made a new generation, which is
  when the citizen revoked a device.
- `pad` pads the record up to the next power of two, with a minimum of 4 KiB.
  A journal page of 2 KB and a page of 3 KB look the same.
- The storage citizen stores the blob, the ID, the padded size and the time of
  the put. It stores nothing else.

The storage citizen also keeps the wraps of the data root, apart from the
blobs. A device gets the wraps for its device key after `hello`, before it
can open any blob. The storage citizen sees how many wraps the account has,
so it sees the count of devices and generations. It cannot open a wrap.

The id key is the same for all storage citizens of one citizen. Two storage
citizens that compare their IDs can see that they hold the same citizen. The
account keys of section 2.3 do not stop this. Open question 5 asks if this
matters.

### 2.5 The introduction and the direct pipe

The relay carries only the introduction. Every other byte goes over the
direct pipe of #190, `sdk/direct`.

1. The device makes a WebRTC offer, and sends it to the storage citizen in one
   ARC call through the relay.
2. The storage citizen replies with the answer.
3. The two ends open one pipe for the sync session, used in both directions.
   They try a direct path first, with the STUN server of the relay.
4. If no direct path opens, the bytes go through the TURN server of the relay,
   as the last resort (delivery spec 4.12). The device tells the user. The
   bytes count against the TURN quota of the citizen, 1 GiB a day on
   relay.republic.sh.

All of section 2.6 runs on the pipe. Nothing of it goes through the relay. A
set reconciliation message for a large set can pass the mail limit of 24 KiB
(#184), so it cannot go through the relay anyway.

Measured on a real link on 2026-10-11 (#181): a direct path ran at a median
of 97 Mbit/s. A first sync of 16 MB then takes about 2 s on the wire. Through
TURN from a home Mac, about 10.5 Mbit/s was measured, so the same sync takes
about 13 s.

Setting up a path takes seconds. A device therefore syncs in batches, never
for each write.

### 2.6 Sync

Sync uses the reconciliation that ARC already has. The client is a
`core/transport.Reconciler`, as the relay transport is. `arc sync` and the
store work with it as they are.

The blob IDs are not record IDs, but both have 32 bytes. The adapter returns
`need` as blob IDs, and resolves them with `get` in its own `Fetch`. It
returns `give` as record IDs, because it computed the blob ID of each local
record in step 1. `core/transport` and `core/node` do not change.

| Message | From | Meaning |
| --- | --- | --- |
| `hello` | device | The device key, a signature over the session, and the grant of the device key. |
| `wraps` | device | The wraps of the data root for this device key. |
| `reconcile` | both | Negentropy messages (NIP-77) over the blob IDs of the account. |
| `get` | device | The blobs for a list of IDs. |
| `put` | device | One blob. The reply is `ok`, `held`, `tombstoned`, or `over quota`. |
| `delete` | device | A list of IDs. |
| `usage` | device | The bytes in use and the quota. |

The cursor is the set itself. Negentropy finds the IDs that each side lacks,
in a few round trips, with no list of every blob. The storage citizen keeps
no sequence number, and needs no clock of the device. A device that was away
for a month sends the same messages as a device that was away for a minute.

The device follows these steps:

1. It computes the blob ID of each local record that `sealedFilter` matches.
2. It reconciles. Now it has the IDs that it lacks, and the IDs that the
   storage citizen lacks.
3. It gets the blobs that it lacks, opens them, and gives each record to its
   store. The store refuses a record that a stored deletion names.
4. It puts each blob that the storage citizen lacks, but only if its store
   still holds the record after step 3.

Steps 3 and 4 must run in this order. Section 2.7 explains why. `core/node`
already does this: `exchange` fetches the events that the store needs, then
reads each event to give from the store.

A device syncs with each storage citizen when the user runs `arc sync`, when
`arc serve` starts, and on a tick while `arc serve` runs. A device does not
sync after each write.

### 2.7 Deletion

A citizen deletes a journal page or a file as today: core writes a blank
draft and a NIP-09 deletion to the local store (interface spec 3.10). The
deletion is a record, so it goes to the storage citizen as a blob. Each other
device gets it on its next sync, and its store applies it.

The device that deletes also sends `delete` for the blobs of the old
versions, checkpoints and parts. The storage citizen removes their bytes at
once, and the quota goes down.

A device that was offline still holds the old records. Two rules stop it from
putting them back:

- Steps 3 and 4 of section 2.6 run in order. The offline device gets the
  deletion first. Its store drops the old records. The device then has
  nothing old to put.
- The storage citizen keeps the ID of each deleted blob for 90 days, and
  answers a put of that ID with `tombstoned`. This covers a device whose
  store failed to apply the deletion.

After 90 days a device that never synced can put an old blob back. The other
devices then refuse its record, because their stores hold the deletion. The
old blob costs quota but shows no deleted data.

### 2.8 What each party sees

| Party | Sees | Does not see |
| --- | --- | --- |
| Relay, today | The author key, the kind of the draft and the `k` tag inside it, the keyed `d` tag, each version and checkpoint, the count, the exact sizes, the times | The content |
| Relay, with storage citizens | One sealed storage list of the citizen, its size and time. That a key sent one call to the storage citizen, its time and size. With STUN: the public addresses of both machines. With TURN: the ciphertext of the pipe and its volume. | The storage citizens, the account, the blobs, the count |
| Storage operator | The account key, each device key and its grant, the count of wraps (so the count of devices and generations), the generation of each blob, the IP address of each device (unless TURN carries the bytes), the time of each session, the count of blobs, the padded sizes, which blobs a session puts and deletes | The citizen key, the device list, the kinds, the `d` tags, the content, which blob is a journal page and which is a file |
| A second device of the citizen | Everything | — |

Section 0 states the limit of this table. A storage citizen sees sizes and
times.

### 2.9 What a hostile storage citizen can do

| Attack | Result | Defense |
| --- | --- | --- |
| Read a blob | It cannot. | AEAD with a key that the storage citizen never gets. |
| Change a blob | The device refuses it. | AEAD, then the signature of the record. |
| Make a blob | The device refuses it. | Same as above. |
| Withhold a blob | A device does not see a new version. It sees an older one, or none. | Two storage citizens. A device that holds a newer version puts it again. Not built: a signed head for each account, as in the Merkle trees of Keybase. |
| Delete everything | The data is gone from this storage citizen. | Each device keeps a full copy. A second storage citizen keeps one too. |
| Serve a revoked device | The revoked device gets new blobs, but it cannot open them: they use a new generation. | The revocation, and the new generation of #173. |
| Accept a blob from a granted device that is hostile | Other devices refuse the record: it does not open, or the citizen key did not sign it. The blob costs quota. | Revoke the device. |
| Join the account to a person | It can use the IP address and the times. | TURN hides the IP address from the storage citizen, at the cost of the TURN quota. Section 0 says the rest. |

### 2.10 Prior art

| System | What it shows for this design |
| --- | --- |
| [Blossom](https://github.com/hzrd149/blossom), [BUD-02](https://github.com/hzrd149/blossom/blob/master/buds/02.md), [BUD-11](https://github.com/hzrd149/blossom/blob/master/buds/11.md) | The closest shape: blobs named by SHA-256 on servers that a Nostr key uses. A signed event of kind 24242 authorizes each upload. The server links the blob with the public key, and the blobs are public by design. The storage app keeps the shape and drops the public key and the public URL. |
| [NIP-96](https://github.com/nostr-protocol/nips/blob/master/96.md) | HTTP file storage with NIP-98 auth and plans with a size limit. It is now marked `unrecommended`, in favor of NIP-B7 (Blossom). Plans with a quota are prior art for the invite of section 2.2. |
| [NIP-37](https://github.com/nostr-protocol/nips/blob/master/37.md) | What ARC uses today. The draft is encrypted, but the wrap is signed by the author. Relays for private content are only "recommended" to ask for NIP-42. That is the leak of section 1.1. |
| [Tarsnap](https://www.tarsnap.com/crypto.html), [design talk](https://www.tarsnap.com/download/EuroBSDCon13.pdf) | The server holds blocks named by HMAC, encrypted on the client. Separate keys sign reads, writes and deletes. Every operation is idempotent, so a client can retry. The blob ID of section 2.4 follows this. |
| [Syncthing untrusted devices](https://docs.syncthing.net/specs/untrusted.html) | A device that holds only ciphertext. It cannot see names, times or content. It sees file sizes, rounded up to 1 KiB, and which blocks change and when. This is the honest limit of section 0. |
| [iCloud Advanced Data Protection](https://support.apple.com/guide/security/advanced-data-protection-for-icloud-sec973254c5f/web), [data overview](https://support.apple.com/en-us/102651) | The trusted devices hold the keys. Recovery needs a recovery contact or a recovery key. Even then, checksums, sizes and dates stay under keys that Apple holds. Recovery belongs to the base design (#173), not to the storage citizen. |
| [Keybase KBFS](https://book.keybase.io/docs/crypto/kbfs), [files](https://book.keybase.io/docs/files/details) | A key for each device, and a paper key for recovery. A revoked device gets no key for new data. Signed Merkle trees stop the server from a rollback. Section 2.9 lists that as not built. |

## 3. Existing data and section 4.11 of the delivery spec

### 3.1 The change to the spec

Section 4.11 of the delivery spec is normative, and the spec is `built`. This
change is a protocol change, and ships as a breaking commit with a
`BREAKING CHANGE` footer.

The new section 4.11 says:

- A data app is a local app. It writes only to the local store. It never
  publishes a record to a relay.
- Sync is a choice. If the citizen chooses it, the device encrypts each record
  and sends it to its storage citizens over the direct pipe. The relay
  carries only the introduction.
- A relay MUST refuse events of kinds 31234, 1234, 3275 and 10013. It replies
  `blocked: self data goes to a storage citizen, see section 4.11`.

These documents change with it:

| Document | Change |
| --- | --- |
| Interface spec 3.2 | The `sealed` visibility means "local, and synced to the storage citizens of the citizen". It no longer goes to "the relays of the NIP-37 list". |
| Interface spec 3.7.1 | The keyed root draft goes to the storage citizens. A machine that signs through NIP-46 gets the keyed root from the signer. |
| Interface spec 3.9.1 | `append` reads from the local store only. |
| `docs/KINDS.md` | Kind 10273 is added: the sealed storage list. Kind 10013 is removed from use. Kinds 31234, 1234 and 3275 stay in the local store and inside blobs. They no longer go to relays. |
| `core/draft` | `RelayList` and `ReadRelayList` go. |
| `adapters/relay/sealed` | It refuses the sealed kinds, instead of hiding them. |

### 3.2 How an older client behaves

- An older client keeps writing drafts to its relays. A new relay refuses
  them with the reason above. The write stays in the local store, so the
  journal still works on that device. `arc sync` reports `not sent` with the
  reason. Nothing fails silently.
- An older client cannot read the data of a newer device, because the newer
  device syncs only with storage citizens. The user upgrades the older
  device, or reads on the newer one.
- A standard Nostr relay that the citizen used for drafts keeps accepting
  them. A newer client stops writing to it.

### 3.3 Existing data on relays

The relay operator and the client move the data in three releases.

1. **Release N.** Storage citizens exist, and a citizen can add one.
   `arc sync` gets the drafts from the relays, puts them in the storage
   citizen, and keeps writing to the relays. Nothing breaks.
2. **Release N+1, breaking.** Data apps stop writing to relays. On its first
   sync the client gets every draft from each relay, puts each one in a
   storage citizen, and then sends a NIP-09 deletion to each relay for its
   drafts, checkpoints, parts and kind 10013. The relay refuses new self
   data, and keeps old data for reads for 90 days.
3. **After 90 days.** The relay operator removes every event of these kinds.

The old drafts on relays are a risk until they go. A revoked device keeps
the old data roots, so it can still open each old draft that a relay gives
it. A standard Nostr relay can give drafts to anyone. A storage citizen
refuses a revoked device. This is one more reason to take the drafts off the
relays.

If a citizen has no storage citizen at release N+1, the client keeps the
data local only, and says so on each sync. It deletes nothing from the relays
until a storage citizen holds the data.

## 4. App on sdk/, or changes below it

The server is an app. The client cannot be one. An app reaches the citizen
only through `arc` commands, and cannot read the store or the keys. The
client must read the store, derive keys from the data root, and give records
to the store. That is the work of the runtime and an adapter.

| Layer | Change | Why |
| --- | --- | --- |
| `apps/storage` | New. The server: invites, accounts, quota, blob storage on disk, the messages of section 2.6. Program `cmd/arc-storage`. | It is a service of one citizen for another. It uses only `sdk/`. |
| `sdk/direct` | New, from #190: the pipe, moved from `apps/transfer/direct`. | `apps/storage` cannot import `apps/transfer`, and an adapter cannot import an app. Both need the pipe. |
| `adapters/transport/storage` | New. The client: a `core/transport.Reconciler` over `sdk/direct`. | It is concrete I/O for core, as `file.Dir` and the relay transport are. |
| `runtime` | The list of storage citizens in the local configuration, the account and blob keys, `arc storage add|ls|rm`, the sync tick. Data apps write only to the local store. | Runtime chooses the transports, as `runtime/citizen.Open` does. |
| `cmd/arc` | `arc sync` syncs self data with the storage citizens, and mail with the relays. | — |
| `core` | No new code. `core/draft` loses the relay list. | The client uses the existing `Reconciler` and store. The key derivation comes from #173. |
| `adapters/relay/sealed` | It refuses the sealed kinds. | Section 3.1. |

No change to core rules comes from this design. The changes in core come
from #173.

## 5. Recommendation

Build storage as a role: a blob store on `sdk/` that any citizen can serve, a
client that is a transport adapter, and Negentropy over keyed blob IDs on the
direct pipe. Make the data apps local. Then take self data off the relay with
one breaking release.

The order of the work:

1. **PR 1, partial (#190).** Move the pipe from `apps/transfer/direct` to
   `sdk/direct`, with a spec. No behavior changes. This PR is a
   prerequisite. It does not finish #172.
2. **PR 2, the smallest first step.** `apps/storage`, the adapter, and
   `arc storage add`. The relay keeps its drafts too. Nothing breaks.
3. **PR 3, breaking.** Data apps write only locally. Section 3.3, release
   N+1.

The smallest first step that a user can try, after PR 2. It uses the user's
own machine as the storage citizen, with no billing:

1. On a machine that is always on, such as a Mac mini at home or a virtual
   machine of the user, serve the storage app and make an invite:
   `arc-storage invite --quota 1GiB`.
2. On the laptop, run `arc storage add <link>`, and then `arc sync`.
3. On a second device of the same citizen, run `arc storage add <link>`, and
   then `arc sync`. Run `arc journal ls`. The pages of the laptop appear.
4. Stop the relay, and run `arc sync` again. It fails at the introduction.
   That is expected: the relay still introduces the two sides.
5. Measure the first sync of 1,000 pages. Compare it with 1.42 s from a local
   relay (#161).

PR 2 does not need to wait for all of #173. Until device keys exist, the
first version uses the citizen key as the only device key, with a grant that
it signs for itself, and one random data root of generation 1. This gives no
device revocation. The blob format of section 2.4 does not change when #173
lands.

## 6. Open questions for Zim

Zim answered three questions on 2026-10-11:

- The storage role: storage citizens are the lockers of issue #172. Storage
  is a role that a citizen serves.
- The journal: a local app with sync, through a storage citizen that holds
  encrypted bytes.
- The first storage citizen: the user's own machine, with no billing. A paid
  operator comes later.

These questions are open:

1. **Does the storage citizen know the citizen by a pseudonym for each
   storage citizen, or by the citizen key?** Recommended: a pseudonym, which
   the signer derives (section 2.3). It costs one HKDF on the signer, and it
   stops two storage citizens from joining their accounts by key. #173
   agrees.
2. **Does the sealed storage list (kind 10273) stay on the relay?**
   Recommended: yes. It is coordination data, as the relay lists of NIP-65
   and NIP-17 are. It is the only way back for a user who has only the seed.
   On 2026-10-11 the kind table of the NIPs README did not list 10273. No
   other kind registry was checked.
3. **How long does a relay keep old drafts for reads after the breaking
   release?** Recommended: 90 days, then the operator removes them.
4. **Must a local record stay a signed Nostr event?** Recommended: yes in the
   first version. The store and its deletion rules then work as they are, and
   a revoked device cannot forge a record. The cost: each write needs the
   citizen key, so a device that is not the signer needs a path to it. NIP-37
   named a draft by its author only because relays held it. Once nothing goes
   to relays, a later version can let the device key sign a record. Decide
   this with #173.
5. **Do all storage citizens of one citizen share one id key?** Recommended:
   yes in the first version. A device can then copy a blob from one storage
   citizen to another without opening it. Two storage citizens that compare
   IDs can see one citizen. Use a key for each storage citizen later, if this
   matters.
6. **How does a device pad a blob?** Recommended: to the next power of two,
   with a minimum of 4 KiB. The cost is at most twice the bytes.
7. **When does a device sync?** Recommended: on `arc sync`, when `arc serve`
   starts, and every 15 minutes while it runs. Never after each write.
8. **Do files keep the parts of 32 KiB?** Recommended: yes in the first
   version, so the record format does not change. Later, a file can be one
   large blob, and the limit of about 6 MiB goes away.
