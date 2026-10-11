Status: proposed

# The locker: self data off the relay

This proposal answers issue #172. It moves the data that a citizen keeps for
itself from the relay to a locker. A locker is a citizen that holds sealed
data for another citizen, with a quota and with consent. The relay only
introduces the two sides. The bytes go over a direct path.

Nothing in this document is built. No production code changes with it.

## 0. What a locker does not fix

Read this first.

- A locker operator sees the count, the sizes and the times of the blobs, as
  the relay operator does today. Every system in section 2.10 leaks the same
  three things. A locker changes who sees them, and it keeps the relay small.
  It does not hide them. Padding (section 2.4) hides exact sizes only.
- A locker that the relay operator runs under a second key gains little
  privacy. The same operator then sees the relay traffic and the locker
  traffic, and can join them by IP address and time.
- A direct path needs both machines online at the same time. Two devices that
  sleep, such as a phone and a laptop, seldom meet. Sync between them needs a
  third machine that is always on. That machine is the locker.
- If no locker exists, nothing syncs. The data stays on the device. A lost
  device then loses the data that it alone held.

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
part is signed by the citizen's key and carries the NIP-70 tag. An ARC relay
serves these kinds only to their author after NIP-42 authentication
(`adapters/relay/sealed`). A standard Nostr relay can serve them to anyone,
because NIP-37 only recommends that rule. In both cases the relay operator
sees the author, the count, the times and the sizes.

On 2026-10-07, 1,000 journal pages held 16 MB on a relay after #160.

### 1.2 Two devices of one citizen

A citizen writes in the journal on a Mac, and reads it on a phone. Today both
devices sync through the relay. With a locker, each device syncs with the
locker. The devices never need to be online at the same time.

If the two devices are online at the same time, they can sync directly. A
device that holds the key of the citizen is a trusted locker for the other
device. Section 2.6 applies to it without the outer seal.

### 1.3 A new device after a lost phone

This is the story of #173. A citizen loses the phone on a trip. By evening, a
new phone reads the journal and gets mail. The old phone is dead to everyone.

The new phone needs three things:

1. The keys of the citizen. These come from the recovery material that #173
   defines, never from the locker.
2. The addresses of the lockers of the citizen. Section 2.2 says where these
   come from.
3. A sync with one locker. It returns every blob of the account.

The locker does not take part in the revocation of the old phone. If #173
gives each device its own key, the locker refuses the old device key after
the citizen revokes it (section 2.3).

### 1.4 Backup

A backup is a second locker. The citizen syncs with each locker in turn. Each
locker holds the same set of blobs, so each one is a full copy. If one locker
withholds or loses blobs, the other one shows the difference (section 2.9).

### 1.5 A user with no machine that is always on

Most people have no machine that is always on. Someone else must run the
locker.

| Locker | Good | Bad |
| --- | --- | --- |
| A machine of the citizen, such as a Mac mini at home | Full trust. No quota of someone else. | Most people have none. A home connection can be slow or behind a hard NAT. |
| A friend | No money. Mutual: each holds the data of the other. | Friends turn machines off. Nobody gives a friend an SLA. |
| A paid operator | Always on. A clear contract and a quota. | The operator sees count, sizes, times and IP addresses. Somebody must build payment. |
| Alfred's box | It exists now. It runs `arc serve` all day. | It is one virtual machine of one person. It is a demo, not a service for other people. |
| The relay operator, under a second key | One deploy. Easy to start. | Little privacy gain, see section 0. The relay and the locker fail together. |

**Recommendation.** A citizen uses two lockers: one that they trust, and one
that is always on. For a user with no machine of their own, the default is a
paid locker on a separate machine, run as an ordinary citizen. The republic.sh
operator can run the first one, on a host apart from the relay. A friend is a
good second locker, not a good only locker. Alfred's box is the first test
locker, not a product.

## 2. The protocol shape

### 2.1 Sealing: what this document assumes

Sealing depends on #173, which a parallel design covers. This proposal
assumes the four statements below. If #173 decides otherwise, sections 2.3,
2.4 and 2.8 change.

1. A citizen has one identity key. Each device has a device key, which the
   identity key certifies. The citizen revokes a lost device with a signed
   record.
2. Each device can derive a data key from the identity of the citizen, with
   HKDF and a label for each purpose. Any device of the citizen opens the
   data. A locker never gets a data key.
3. The locker knows its client by an account key, not by the identity key.
   Section 2.3 defines the account key.
4. A new device gets the identity or the data key from an old device or from
   recovery material. It never gets a key from the locker.

### 2.2 Find and pair

A locker is a citizen that serves the `locker` app. It announces the app as
any provider does (delivery spec 4.7). Discovery does not make a locker
usable. Each side consents first.

1. The locker operator makes an invite: `arc-locker invite --quota 1GiB`. The
   invite is a link, `locker+arc://<locker key>?invite=<token>`. The token is
   random, is good for one use, and names the quota.
2. The operator gives the link to the citizen, as text, a QR code or a
   message.
3. The citizen runs `arc locker add <link>`. The device sends one ARC call to
   the locker. The call holds the token and the account key.
4. The locker checks the token, opens the account with the quota, and
   replies. The token is now spent.

The device keeps the link and the account in its local configuration. Other
devices learn the lockers in one of two ways:

- From an old device, when the citizen pairs a new device (#173).
- From the recovery material, when no old device exists. The recovery
  material holds the link of each locker.

The relay keeps no list of lockers. Kind 10013 has no replacement.

### 2.3 The account, consent and the quota

The account key is a key pair that each device derives:
`HKDF(data root, info = "arc-locker-account-v1" || locker key)`. Each device
of the citizen derives the same account key for one locker. Two lockers see
two keys that they cannot join.

If #173 gives each device its own key, the account key signs a short grant for
each device key. The locker then accepts a device key only with a valid
grant, and refuses it after the citizen sends a revocation. Until then, the
account key is the only key that the locker knows.

The locker keeps these values for each account:

| Value | Rule |
| --- | --- |
| Quota in bytes | The locker refuses a put that passes it. The error names the bytes in use and the quota. |
| Bytes in use | The sum of the padded sizes of the blobs. |
| Last sync | The time of the last session. |
| Tombstones | The IDs of deleted blobs, for 90 days. See section 2.7. |

Consent works in both directions:

- The locker consents with the invite. The operator can close an account. The
  locker then tells the client on its next session, and refuses puts. It
  keeps reads for 30 days, so the citizen can move the data.
- The citizen consents with `arc locker add`. The citizen can remove a locker
  with `arc locker rm`. The device then deletes every blob of the account,
  and forgets the locker.

### 2.4 What the locker stores

The locker stores sealed blobs that it cannot read.

```text
blob    = id || AEAD(blob key, nonce, pad(signed event JSON))
id      = HMAC-SHA256(blob key, event ID)            (32 bytes)
blob key = HKDF(data root, info = "arc-locker-blob-v1")
```

- The plaintext is the signed NIP-37 event that the device holds today: a
  draft, a checkpoint, a part, or a NIP-09 deletion. The device opens the
  blob, and gives the event to its store. The store verifies the signature
  and applies the existing rules. `core/draft` does not change.
- The ID is a keyed hash of the event ID. Two devices that put the same event
  make the same blob. A put is therefore idempotent, as in Tarsnap. The locker
  cannot match an ID with an event on any relay.
- `pad` pads the plaintext up to the next power of two, with a minimum of
  4 KiB. A journal page of 2 KB and a page of 3 KB look the same.
- The locker stores the blob, the ID, the padded size and the time of the
  put. It stores nothing else.

The blob key is the same for all lockers of one citizen. Two lockers that
compare their IDs can see that they hold the same citizen. The account keys
of section 2.3 do not stop this. Open question 6 asks if this matters.

### 2.5 The introduction and the direct path

The relay carries only the introduction. Every other byte goes over the
direct path.

1. The device makes a WebRTC offer, and sends it to the locker in one ARC
   call through the relay. This is the code path of `apps/transfer` today.
2. The locker replies with the answer.
3. The two ends open a data channel. They try a direct path first, with the
   STUN server of the relay.
4. If no direct path opens, the bytes go through the TURN server of the relay,
   as the last resort (delivery spec 4.12). The device tells the user. The
   bytes count against the TURN quota of the citizen, 1 GiB a day on
   relay.republic.sh.

All of section 2.6 runs on the data channel. Nothing of it goes through the
relay. A set reconciliation message for a large set can pass the mail limit
of 24 KiB (#184), so it cannot go through the relay anyway.

Measured on a real link on 2026-10-11 (#181): a direct path ran at a median of
97 Mbit/s. A first sync of 16 MB then takes about 2 s on the wire. Through
TURN from a home Mac, about 10.5 Mbit/s was measured, so the same sync takes
about 13 s.

Setting up a path takes seconds. A device therefore syncs in batches, never
for each write.

### 2.6 Sync

Sync uses the reconciliation that ARC already has. The locker client is a
`core/transport.Reconciler`, as the relay transport and the directory
transport are. `arc sync` and the store work with it as they are.

| Message | From | Meaning |
| --- | --- | --- |
| `hello` | device | The account key, a signature over the session, and the grant of the device key if one exists. |
| `reconcile` | both | Negentropy messages (NIP-77) over the blob IDs of the account. |
| `get` | device | The blobs for a list of IDs. |
| `put` | device | One blob. The reply is `ok`, `held`, `tombstoned`, or `over quota`. |
| `delete` | device | A list of IDs. |
| `usage` | device | The bytes in use and the quota. |

The cursor is the set itself. Negentropy finds the IDs that each side lacks,
in a few round trips, with no list of every blob. The locker needs no clock
from the device and no sequence number. A device that was away for a month
sends the same messages as a device that was away for a minute.

The device follows these steps:

1. It computes the blob ID of each local event that `sealedFilter` matches.
2. It reconciles. Now it has the IDs that it lacks, and the IDs that the
   locker lacks.
3. It gets the blobs that it lacks, opens them, and gives each event to its
   store. The store refuses an event that a stored deletion names.
4. It puts each blob that the locker lacks, but only if its store still holds
   the event after step 3.

Steps 3 and 4 must run in this order. Section 2.7 explains why.

A device syncs with each locker when the user runs `arc sync`, when
`arc serve` starts, and on a tick while `arc serve` runs. A device does not
sync after each write.

### 2.7 Deletion

A citizen deletes a journal page or a file as today: core publishes a blank
draft and a NIP-09 deletion (interface spec 3.10). The deletion is an event,
so it goes to the locker as a blob. Each other device gets it on its next
sync, and its store applies it.

The device that deletes also sends `delete` for the blobs of the old
versions, checkpoints and parts. The locker removes their bytes at once, and
the quota goes down.

A device that was offline still holds the old events. Two rules stop it from
putting them back:

- Steps 3 and 4 of section 2.6 run in order. The offline device gets the
  deletion first. Its store drops the old events. The device then has nothing
  old to put.
- The locker keeps the ID of each deleted blob for 90 days, and answers a put
  of that ID with `tombstoned`. This covers a device whose store failed to
  apply the deletion.

After 90 days a device that never synced can put an old blob back. The other
devices then refuse its event, because their stores hold the deletion. The
old blob costs quota but shows no deleted data.

### 2.8 What each party sees

| Party | Sees | Does not see |
| --- | --- | --- |
| Relay, today | The author key, the kind of the draft and the `k` tag inside it, the keyed `d` tag, each version and checkpoint, the count, the exact sizes, the times | The content |
| Relay, with a locker | That a key sent one call to the locker key, its time and size. With STUN: the public addresses of both machines. With TURN: the ciphertext of the data channel and its volume. | The account, the blobs, the count |
| Locker operator | The account key, the IP address of each device (unless TURN carries the bytes), the time of each session, the count of blobs, the padded sizes, which blobs a session puts and deletes | The identity key of the citizen, the kinds, the `d` tags, the content, which blob is a journal page and which is a file |
| A second device of the citizen | Everything | — |

Section 0 states the limit of this table. A locker sees sizes and times.

### 2.9 What a hostile locker can do

| Attack | Result | Defense |
| --- | --- | --- |
| Read a blob | It cannot. | AEAD with a key that the locker never gets. |
| Change a blob | The device refuses it. | AEAD, then the signature of the event inside. |
| Make a blob | The device refuses it. | Same as above. |
| Withhold a blob | A device does not see a new version. It sees an older one, or none. | Two lockers. A device that holds a newer version puts it again. Not built: a signed head for each account, as in the Merkle trees of Keybase. |
| Delete everything | The data is gone from this locker. | Each device keeps a full copy. A second locker keeps one too. |
| Join the account to a person | It can use the IP address and the times. | TURN hides the IP address from the locker, at the cost of the TURN quota. Section 0 says the rest. |

### 2.10 Prior art

| System | What it shows for this design |
| --- | --- |
| [Blossom](https://github.com/hzrd149/blossom), [BUD-02](https://github.com/hzrd149/blossom/blob/master/buds/02.md), [BUD-11](https://github.com/hzrd149/blossom/blob/master/buds/11.md) | The closest shape: blobs named by SHA-256 on servers that a Nostr key uses. A signed event of kind 24242 authorizes each upload. The server links the blob with the public key, and the blobs are public by design. The locker keeps the shape and drops the public key and the public URL. |
| [NIP-96](https://github.com/nostr-protocol/nips/blob/master/96.md) | HTTP file storage with NIP-98 auth and plans with a size limit. It is now marked `unrecommended`, in favor of NIP-B7 (Blossom). Plans with a quota are prior art for the invite of section 2.2. |
| [NIP-37](https://github.com/nostr-protocol/nips/blob/master/37.md) | What ARC uses today. The draft is encrypted, but the wrap is signed by the author. Relays for private content are only "recommended" to ask for NIP-42. That is the leak of section 1.1. |
| [Tarsnap](https://www.tarsnap.com/crypto.html), [design talk](https://www.tarsnap.com/download/EuroBSDCon13.pdf) | The server holds blocks named by HMAC, encrypted on the client. Separate keys sign reads, writes and deletes. Every operation is idempotent, so a client can retry. The blob ID of section 2.4 follows this. |
| [Syncthing untrusted devices](https://docs.syncthing.net/specs/untrusted.html) | A device that holds only ciphertext. It cannot see names, times or content. It sees file sizes, rounded up to 1 KiB, and which blocks change and when. This is the honest limit of section 0. |
| [iCloud Advanced Data Protection](https://support.apple.com/guide/security/advanced-data-protection-for-icloud-sec973254c5f/web), [data overview](https://support.apple.com/en-us/102651) | The trusted devices hold the keys. Recovery needs a recovery contact or a recovery key. Even then, checksums, sizes and dates stay under keys that Apple holds. Recovery belongs to the base design (#173), not to the locker. |
| [Keybase KBFS](https://book.keybase.io/docs/crypto/kbfs), [files](https://book.keybase.io/docs/files/details) | A key for each device, and a paper key for recovery. A revoked device gets no key for new data. Signed Merkle trees stop the server from a rollback. Section 2.9 lists that as not built. |

## 3. Existing data and section 4.11 of the delivery spec

### 3.1 The change to the spec

Section 4.11 of the delivery spec is normative, and the spec is `built`. This
change is a protocol change, and ships as a breaking commit with a
`BREAKING CHANGE` footer.

The new section 4.11 says:

- A citizen keeps its self data in its local store. Sync is a choice.
- If the citizen chooses sync, the data goes to its lockers over a direct
  path. The relay carries only the introduction.
- A relay MUST refuse events of kinds 31234, 1234, 3275 and 10013. It replies
  `blocked: self data goes to a locker, see section 4.11`.

These documents change with it:

| Document | Change |
| --- | --- |
| Interface spec 3.2 | The `sealed` row goes to "the lockers of the citizen", not "the relays of the NIP-37 list". |
| Interface spec 3.7.1 | The keyed root draft goes to the lockers. A machine that signs through NIP-46 gets it from a locker. |
| Interface spec 3.9.1 | `append` reads from the store, then from the lockers. |
| `docs/KINDS.md` | Kind 10013 is removed from use. Kinds 31234, 1234 and 3275 stay, because the locker blobs carry them. |
| `core/draft` | `RelayList` and `ReadRelayList` go. |
| `adapters/relay/sealed` | It refuses the sealed kinds, instead of hiding them. |

### 3.2 How an older client behaves

- An older client keeps writing drafts to its relays. A new relay refuses
  them with the reason above. The write stays in the local store, so the
  journal still works on that device. `arc sync` reports `not sent` with the
  reason. Nothing fails silently.
- An older client cannot read the data of a newer device, because the newer
  device writes to a locker. The user upgrades the older device, or reads on
  the newer one.
- A standard Nostr relay that the citizen used for drafts keeps accepting
  them. A newer client stops writing to it.

### 3.3 Existing data on relays

The relay operator and the client move the data in three releases.

1. **Release N.** Lockers exist, and a citizen can add one. `arc sync`
   gets the drafts from the relays, puts them in the locker, and keeps
   writing to the relays. Nothing breaks.
2. **Release N+1, breaking.** The client stops writing self data to relays.
   On its first sync it gets every draft from each relay, puts each one in a
   locker, and then sends a NIP-09 deletion to each relay for its drafts,
   checkpoints, parts and kind 10013. The relay refuses new self data, and
   keeps old data for reads for 90 days.
3. **After 90 days.** The relay operator removes every event of these kinds.

If a citizen has no locker at release N+1, the client keeps the data local
only, and says so on each sync. It still deletes nothing from the relays
until a locker holds the data.

## 4. App on sdk/, or changes below it

The server can be an app. The client cannot. An app reaches the identity of
the citizen only through `arc` commands, and cannot read the store or the
key. The client must read the store, derive the data key, and give events to
the store. That is the work of the runtime and an adapter.

| Layer | Change | Why |
| --- | --- | --- |
| `apps/locker` | New. The server: invites, accounts, quota, blob storage on disk, the messages of section 2.6. Program `cmd/arc-locker`. | It is a service of one citizen for another. It uses only `sdk/`. |
| `sdk/direct` | New, moved from `apps/transfer/direct`: the offer and answer call, the ICE order, STUN and TURN, the steps of 8 KiB. It needs a spec. | `apps/locker` cannot import `apps/transfer`, and an adapter cannot import an app. Both need the pipe. |
| `adapters/transport/locker` | New. The client: a `core/transport.Reconciler` over `sdk/direct`. | It is concrete I/O for core, as `file.Dir` and the relay transport are. |
| `runtime` | The list of lockers in the local configuration, the account and blob keys, `arc locker add|ls|rm`, the sync tick. | Runtime chooses the transports, as `runtime/citizen.Open` does. |
| `cmd/arc` | `arc sync` syncs with the lockers instead of the relays. | — |
| `core` | No new code. `core/draft` loses the relay list. | The locker client uses the existing `Reconciler` and store. The key derivation comes from #173. |
| `adapters/relay/sealed` | It refuses the sealed kinds. | Section 3.1. |

No change to core rules comes from this design. The changes in core come
from #173.

## 5. Recommendation

Build the locker as described: a blob store on `sdk/` that a citizen serves,
a client that is a transport adapter, and Negentropy over keyed blob IDs on a
direct path. Then take self data off the relay with one breaking release.

The order of the work:

1. **PR 1, partial.** Move the pipe from `apps/transfer/direct` to
   `sdk/direct`, with a spec. The transfer app uses it. No behavior changes.
   This PR is a prerequisite. It does not finish #172.
2. **PR 2, the smallest first step.** `apps/locker`, the adapter, and
   `arc locker add`. The relay keeps its drafts too. Nothing breaks.
3. **PR 3, breaking.** Section 3.3, release N+1.

The smallest first step that a user can try, after PR 2:

1. On Alfred's box, run the locker app and make an invite:
   `arc-locker invite --quota 1GiB`.
2. On the Mac, run `arc locker add <link>`, and then `arc sync`.
3. On a second machine with the same key, run `arc locker add <link>`, and
   then `arc sync`. Run `arc journal ls`. The pages of the Mac appear.
4. Stop the relay, and run `arc sync` again. It fails at the introduction.
   That is expected: the relay still introduces the two sides.
5. Measure the first sync of 1,000 pages. Compare it with 1.42 s from a local
   relay (#161).

Until #173 decides sealing, PR 2 can use a blob key that the runtime derives
from the citizen's key today. This is one key on every device, so it gives no
device revocation. The blob format of section 2.4 does not change when #173
lands. Only the source of the data root changes.

## 6. Open questions for Zim

1. **Who runs the default locker for a user with no machine that is always
   on?** Recommended: a paid locker citizen that the republic.sh operator
   runs on a host apart from the relay. A friend is the second locker.
   Alfred's box is the test locker only.
2. **Does the locker know the citizen by a pseudonym for each locker, or by
   the identity key?** Recommended: a pseudonym for each locker (section
   2.3). It costs one HKDF, and it stops two lockers from joining their
   accounts by key.
3. **Where does a new device learn its lockers?** Recommended: from an old
   device at pairing, or from the recovery material. No list on the relay,
   and kind 10013 has no replacement.
4. **Does the relay keep any self data, such as the keyed root draft?**
   Recommended: no. The keyed root goes to the lockers. A machine that signs
   through NIP-46 gets it from a locker.
5. **How long does a relay keep old drafts for reads after the breaking
   release?** Recommended: 90 days, then the operator removes them.
6. **Do all lockers of one citizen share one blob key?** Recommended: yes in
   the first version. It lets a device copy a blob from one locker to another
   without opening it. Two lockers that compare IDs can then see one
   citizen. Use a key for each locker later, if this matters.
7. **How does a device pad a blob?** Recommended: to the next power of two,
   with a minimum of 4 KiB. The cost is at most twice the bytes.
8. **When does a device sync?** Recommended: on `arc sync`, when `arc serve`
   starts, and every 15 minutes while it runs. Never after each write.
9. **Do files keep the parts of 32 KiB inside a locker?** Recommended: yes in
   the first version, so the event format does not change. Later, a file can
   be one large blob, and the limit of about 6 MiB goes away.
10. **Is the move of the pipe to `sdk/direct` the first PR?** Recommended:
    yes. The locker cannot share the pipe of the transfer app in another way,
    and the rule against copied provider code forbids a second copy.
