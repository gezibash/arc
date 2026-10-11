Status: proposed

# The base: identity, device keys and sealing

This proposal answers [issue #173](https://github.com/gezibash/arc/issues/173).
No code implements it. It also covers key backup and recovery from issue #49.

Each claim about ARC names a file and a line, at commit `7eaeed8`. Each number
says "measured" or "estimated". The sources are in section 9.

## 0. Summary

- The citizen key stays one secp256k1 key. Nostr clients see one public key,
  as they do today.
- The citizen key leaves the phones. It lives on one signer, a machine that is
  always on, and on paper.
- Each device has its own device key. A device signs and opens mail through
  the signer. The signer keeps a list of devices, and refuses a revoked device.
- Self data is sealed with a random data root, not with a key that comes from
  the citizen key. The data root is wrapped to each device key and to the
  citizen key. A revocation makes a new data root.
- The first step changes only the signer: one authorization for each device,
  and a command that revokes a device.

## 1. Corrections to the issue

The issue makes three claims about the code. Two are true. One is partly
false.

| Claim in the issue | Result | Evidence |
| --- | --- | --- |
| One key is the citizen on every device, with no revocation. | True | `docs/delivery/SPEC.md:84-91` defines one key. No code revokes or rotates a citizen key. Kind 10272 is reserved for a migration record and is not used (`docs/delivery/SPEC.md:856`, `runtime/iface/consent.go:21`, `docs/KINDS.md:37`). |
| Data keys come from the identity key. | True | `core/draft/draft.go:100` seals a draft with `k.Encrypt(..., me)`. `core/keys/keys.go:123` makes the NIP-44 conversation key from the secret key and the citizen's own public key. The keyed root is `HKDF(secret key)` (`runtime/iface/run.go:85`). |
| Data apps send every blob to every relay, with no cursor. | Partly true | Each sealed publish goes to all relays of the citizen (`runtime/iface/data.go:289`, `runtime/citizen/environment.go:70-71`, `core/node/node.go:44-56`). Sync uses NIP-77 Negentropy when a relay offers it (`core/node/node.go:78-90`). Without NIP-77, sync fetches the whole filter, with no stored `since` (`core/node/node.go:92-121`). |

The carriage question belongs to the locker design (issue #172). This
proposal does not change carriage.

The bunker of today has no revocation. It admits any client that gives the
bunker secret (`cmd/arc/keys.go:557-563`). The secret never changes
(`cmd/arc/keys.go:658-662`). A thief who has a copy of the `bunker://` URI
connects again after each restart. This result comes from reading the code.
No test shows it.

## 2. The story, step by step

A citizen travels with one phone. A laptop stays at home. The citizen loses
the phone in the morning. By evening, a new phone must read the journal and get
mail. The old phone must be dead to everyone.

The citizen has no second device on the trip. Thus paper, or a machine at
home, must prove that the new phone belongs to the citizen.

| Step | What happens | Identity needs | Key needs | Data needs |
| --- | --- | --- | --- | --- |
| 1 | The phone is lost. The thief has it, possibly unlocked. | Nothing that the phone holds can change the identity. | The phone holds no citizen key. | The phone holds only data that it already synced. |
| 2 | The citizen buys a new phone and installs `arc`. | None yet. | The new phone makes its own device key. | None yet. |
| 3 | The citizen proves that the new phone is theirs. | Something outside the phone: paper, or the signer at home. | The paper holds the citizen key, sealed with a passphrase (NIP-49). | None. |
| 4 | The old phone is revoked. | The signer drops the old device key from its list. | The signer refuses each later request of the old device key. | A new data root replaces the one that the old phone knows. |
| 5 | The new phone reads the journal. | The new phone is on the device list. | The new phone gets the data roots, wrapped to its device key. | Sealed drafts come from the relays, or from a locker (issue #172). |
| 6 | The new phone gets mail. | Senders still use the one public key. Nothing changes for them. | The signer opens each gift wrap for the new phone. | Mail on the relays stays sealed to the citizen key, as NIP-17 requires. |
| 7 | The thief tries to use the old phone. | The signer refuses it. Relays that ask NIP-42 refuse it, because the signer does not answer. | The old phone has a device key that nothing accepts. | It keeps the data that it cached. See section 4.3. |

Today, step 4 is impossible. The phone holds the citizen key, so the thief
is the citizen until the citizen moves to a new key. Section 4.4 describes
that cost.

## 3. Options for identity

A Nostr client knows one public key for each person. NIP-17 mail is sealed to
that key. Thus each device that opens mail must hold the secret key, or must
ask a machine that holds it. This fact decides most of the choice.

| Option | How it works | Cost for a Nostr client that knows one public key | Revocation |
| --- | --- | --- | --- |
| A. One key on each device | Today. Each device holds the citizen key (`docs/delivery/SPEC.md:84-91`). | None. | None. A lost device is a lost key. The only remedy is a new identity (section 4.4). |
| B. Root key and device keys that sign | Matrix cross-signing, Keybase sibkeys, NIP-102 subkeys, linked subkeys (issue nips#1810). Each device signs with its own key. A list, signed by the root, names the devices. | High. A plain client sees each device as a stranger. Mail to the root key cannot be opened by a device unless the device holds the root, or the sender wraps to each device, which a NIP-17 client does not do. Each client and relay must learn the device list. | Good inside ARC. Invisible outside ARC. |
| C. Delegation | NIP-26. ARC's proposal for machines that live for one task uses a similar signed delegation (`docs/proposals/machine-lifecycle.md:93-130`). | High. NIP-26 is "unrecommended". Relay support was about 0.01% (reported in nips#1116, not measured by ARC). A delegation does not cover NIP-44 decryption. | Weak. NIP-26 has no revocation, and a time limit fails because `created_at` can be set to the past. |
| D. A signer device | NIP-46. One machine holds the key. Each other device is a client with its own client key. ARC has this: `arc keys bunker` and `arc keys add bunker://` (`docs/interface/SPEC.md:574-610`). | None. Each event has a normal signature of the one key. | Good, after the signer keeps one authorization for each client. Today it does not (section 1). |
| D2. A threshold signer | FROSTR splits the key into t-of-n shares. The devices sign together. The public key does not change. | None. | Good. New shares replace the old ones, and the public key stays. |

**Recommendation: D, with device keys for data only.** The device keys do
not sign Nostr events that other people read. They identify a device to the
signer, receive wrapped data roots, and identify a device to a locker. Thus a
plain Nostr client sees no change. NIP-4e (nips#1647, open) has the same shape:
one client key for each device, and one random encryption key sealed to each
client key.

Option B fails for ARC because mail is NIP-17 (`docs/delivery/SPEC.md:202-218`
uses the `p` tag form so that mail reaches any NIP-17 client). Option C repeats
the failure of NIP-26. Option D2 is a good later step, see section 7, question 3.

The costs of option D:

- A signer must be on and reachable. This is the same machine that the locker
  needs for a user with no machine that is always on (issue #172).
- Each signature and each mail open is one round trip through a relay to the
  signer. ARC allows 20 seconds for each operation
  (`docs/interface/SPEC.md:593-597`). That is a limit, not a measurement. The
  real time is not measured.
- A phone with no path to the signer cannot sign or open mail. It can still
  read sealed data, because it holds the data roots (section 5).
- The signer of today opens all ciphertext for a client with `--decrypt all`,
  which mail needs (`docs/interface/SPEC.md:600-604`, `:848-849`). A device
  that reads mail can therefore open everything sealed to the citizen key,
  until it is revoked.

## 4. Revocation

### 4.1 What "dead to everyone" means

| Party | With option D | Today |
| --- | --- | --- |
| Mail | The signer refuses to open gift wraps for the old device key. Senders change nothing. | The thief opens all mail, because the phone holds the key. |
| Calls | The old phone cannot sign a reply or a request. Callers change nothing. | The thief answers calls as the citizen. |
| Relays | The old phone cannot answer a NIP-42 challenge as the citizen, so a relay serves it no sealed data (`adapters/relay/sealed/sealed.go:25`). | The thief authenticates as the citizen. |
| Other citizens | No change. Their install trust holds the public key (`runtime/catalog/catalog.go:293`), and the public key does not change. | They cannot know. Only a new identity helps. |
| Locker | The locker refuses a device key that is not on the device list. | Not applicable. No locker exists. |

### 4.2 The signer must keep a device list

The signer must keep one entry for each device key. Each entry holds the
public key of the device, a name, the time of admission, and a policy: the
kinds that it may sign, and whether it may open mail. The signer must refuse
each request from a device key that is not on the list. The list must persist
across restarts.

Today the signer keeps its authorizations only in memory
(`cmd/arc/keys.go:557`), and one fixed secret admits each client
(`cmd/arc/keys.go:559`). The client key that each device needs exists already:
`cmd/arc/keys.go:85-90` makes and keeps one key for each client.

A new device gets on the list in one of two ways:

1. A device that is on the list approves it. The signer shows a one-time
   pairing code.
2. The citizen key signs the admission. The paper key gives the citizen key,
   see section 6.

On the trip, the citizen has only way 2.

### 4.3 What a thief can do with the old phone

Before revocation, the window is from the loss to step 4. In the story, the
window is some hours (estimated, from "by evening").

In that window, with option D, the thief can:

- Sign any kind that the device policy allows, as the citizen.
- Open all mail and all ciphertext sealed to the citizen key, if the device
  may open mail.
- Write drafts. NIP-37 replaces a draft with a newer draft of the same `d`
  tag, so the thief can overwrite journal pages. The checkpoints of kind 1234
  keep each earlier version (`docs/interface/SPEC.md:327-337`), so the citizen
  can restore a page.
- Read the data that the phone cached, and the data roots that it holds.

After revocation, the thief can:

- Read the data that the phone already holds, with the data roots that it
  already holds. No key design removes this. Only the disk encryption of the
  phone and the lock of the app protect it.
- Not sign, not open new mail, not authenticate again to a relay or a
  locker, and not read data under the new data root. A relay connection that
  authenticated before the revocation stays open until it ends.
- Read old data on a relay that serves drafts to anyone. Only a relay that
  enforces NIP-42 reads keeps drafts from others
  (`docs/interface/SPEC.md:852-854`). The thief holds the old data roots, so
  the thief can open each old draft that such a relay serves. Only a re-seal
  under the new generation, or a locker that holds the drafts, stops this.

Sent mail text is not stored on disk. The outbox keeps only the ID of the
sender's own seal (`core/mail/mail.go:72-74`), and the seal is sealed to the
citizen key. Thus mail on the phone is safe after revocation, with option D.

### 4.4 Without a signer: a new identity

If a citizen keeps the key on the phone (option A), revocation means a new
key. The costs:

- The petname changes, because it follows from the public key
  (`core/keys/petname.go:9-17`). The citizen gets a new name.
- Each install trust of each other citizen names the old key
  (`runtime/catalog/catalog.go:293`).
- Each contact must learn the new key.
- The thief also holds the old key, and can publish a competing migration.
  Only a successor key that the citizen named in advance can win. NIP-41
  drafts (nips#829, nips#2137) use this method. None is merged.

ARC reserves kind 10272 for a migration record (`docs/delivery/SPEC.md:856`).
This proposal keeps it reserved. A migration is for a stolen signer or paper,
not for a lost phone.

## 5. Sealing

### 5.1 Today: keys from the citizen key

A draft, a checkpoint, a part and the private relay list are NIP-44
ciphertext from the citizen to itself (`core/draft/draft.go:100`, `:201`,
`:259`). The conversation key is ECDH of the citizen key with its own public
key (`core/keys/keys.go:123`). The keyed root, which makes the `d` tags, is
HKDF of the secret key (`runtime/iface/run.go:85`).

The results:

- Each holder of the citizen key reads all self data, of the past and the
  future.
- A device cannot read self data without the citizen key, or without one
  round trip to the signer for each event.
- A device cannot lose access. Revocation of read access is impossible.

### 5.2 Proposed: random data roots

- A **data root** is 32 random bytes with a generation number.
- The content of each draft, checkpoint and part is NIP-44 ciphertext under a
  conversation key: `HKDF-SHA256(ikm = data root, info = "arc-data-v1")`.
  NIP-44 uses a new random nonce for each message, so one conversation key
  serves many events.
- Each sealed event carries a tag that names the generation of its data root.
- A **wrap** of a data root is a sealed event with the shape of a NIP-37
  draft. The citizen key signs it. Its content is the data root, sealed to one
  device key. One more wrap seals it to the citizen key. `core/draft` cannot
  open a wrap to a device key unchanged, because `draft.Open` accepts only a
  draft whose author opens it (`core/draft/draft.go:153`). One wrap is 565
  bytes of signed JSON, and its content is 176 characters (measured, NIP-44 of
  the root as hex, one `d`, `k` and `-` tag).
- The citizen key still signs each draft, checkpoint and part, through the
  signer. A draft is named by its author and its `d` tag
  (`core/draft/draft.go:70-72`), and a relay replaces a draft only for the
  same author. If device keys signed drafts, each device would have its own
  copy of each page. Thus a device reads with no network, and writes only
  through the signer.
- On revocation, the signer makes a new generation and wraps it to each
  remaining device key and to the citizen key. New writes use the new
  generation. Old data stays under the old generation.
- Each new generation carries the previous data root, sealed under the new
  one. Thus the newest data root opens each older generation. ARC never
  deletes or changes a generation.
- The wrap of each generation to the citizen key means that the citizen key
  always recovers each generation. The paper therefore recovers all data.

This is the per-user key of Keybase: a random seed, boxed to each device and
to each paper key, and rotated when a device is revoked. age and Tink use the
same pattern for each file: a random key, wrapped to each recipient.

The keyed root for `d` tags does not change. It is a key for names, not for
content. A new keyed root would change each `d` tag, and move each draft to a
new coordinate. Only `KeyedValue` uses the root (`runtime/citizen/environment.go:40-56`).

### 5.3 What stored data must migrate

| Data | Kind | Change |
| --- | --- | --- |
| Drafts, checkpoints, parts | 31234, 1234, 3275 | Generation 0 is today's ciphertext under the citizen key. A device opens it through the signer, with `--decrypt self`. ARC re-seals a draft under the newest generation when the draft changes. A command can re-seal all drafts at once. |
| Private relay list | 10013 | Issue #172 removes it. No change. |
| Keyed root draft | 31234, `d` = `arc-keyed-root` | It stays sealed to the citizen key. It is also wrapped to each device key, like a data root. |
| Mail seals | 13 in 1059 | No change. NIP-17 requires mail sealed to the citizen key. |

A re-seal only when a draft changes has a cost: a revoked device holds the
old data roots, and can open each old draft that a relay without NIP-42 reads
serves (section 4.3). A full re-seal after each revocation, or a locker,
removes this cost.

The cost of a full re-seal is one decrypt through the signer and one
signature for each sealed event. The number of sealed events of a real
citizen is not measured.

A Nostr client that holds the nsec opens today's drafts, because NIP-37 seals
them to the author. After this change, such a client cannot open generation 1
and later. ARC data becomes readable only by ARC. This is a real cost.

### 5.4 How the new phone gets the data roots

1. The new phone is on the device list (section 4.2).
2. The new phone asks the signer for the data roots.
3. The signer opens each wrap to the citizen key with `--decrypt self`, and
   wraps each data root to the device key of the new phone.
4. The new phone opens the wraps with its own device key, and keeps the data
   roots on its disk.
5. The new phone reads drafts with no further call to the signer.

If the signer is lost too, the paper key gives the citizen key, which opens
the wraps directly (section 6).

A new device must also find the lockers of the citizen (issue #172). The
signer holds the link of each locker, and gives the links to a new device with
the data roots. The signer also publishes the links to the relays of the
citizen, in one replaceable event sealed to the citizen key. The paper holds
only the citizen key, because a locker link can change and paper cannot. With
the paper key, a new device fetches that event and opens it. This event is
coordination data, so it stays on the relay.

## 6. Backup and recovery of the citizen key

The citizen key is the last resort for each step above. Today its loss is
fatal (`docs/delivery/SPEC.md:847`).

| Method | What it gives | Cost | Status in ARC |
| --- | --- | --- | --- |
| Paper key | The citizen key as a NIP-49 `ncryptsec`, protected by a passphrase (scrypt, XChaCha20-Poly1305). | One passphrase to remember. A thief with the paper must still guess the passphrase. | Built: `arc keys gen --encrypt`, `arc keys encrypt`, `arc keys add` (`core/keys/keys.go:70-85`). No export command and no printed form. |
| Threshold | The citizen key split into shares, for example 2 of 3 with SLIP-39. Shares go to paper, a friend, a second place. | More pieces. No single paper gives the key. | Not built. |
| Threshold signer | FROSTR shares that sign together. The key never exists in one place. | Each signature needs two online shares. | Not built. |
| Another device | A device on the list admits a new device (section 4.2, way 1). | It does not recover the citizen key if the signer is lost. | Not built. |

**Recommendation: the paper key first.** It is built. In the story, the
citizen types the `ncryptsec` and the passphrase on the new phone. The phone
opens the citizen key in memory, signs the admission of its own device key and
the removal of the old one, and erases the citizen key. The signer refuses
NIP-46 requests from a device key that is not on its list, so the phone
cannot send the change as a request. It publishes the device list, signed by
the citizen key, to the relays of the citizen. The signer watches for that
event and reloads its list, as a machine watches for the keyed root draft
today (`runtime/citizen/identity.go:16-17`). The citizen key is on the new phone for some seconds (estimated). That is
the cost of recovery without a second device.

A threshold split is a later step. Apple's Advanced Data Protection shows the
same choice: a recovery contact or a recovery key, and no recovery if both
fail.

## 7. Recommendation and the first step

**Recommendation.** Use option D with device keys for data only:

1. The citizen key lives on the signer and on paper, not on phones.
2. The signer keeps a device list, and refuses a revoked device.
3. Self data uses random data roots, wrapped to each device and to the citizen
   key, with a new generation on each revocation.
4. Mail and Nostr identity stay as they are.

The reasons:

- Nothing changes for other citizens or for plain Nostr clients. This is the
  only option where "dead to everyone" needs no action from anyone else.
- The parts exist: the bunker, the client key of each device, NIP-37 drafts,
  NIP-49 paper keys, and the keyed root draft, which already wraps a key in a
  draft (`runtime/citizen/identity.go:23-42`).
- Nostr tried delegation and device subkeys, and they failed or stalled
  (NIP-26 deprecated, NIP-41 and NIP-102 still open). Bunkers are what Nostr
  users use.
- Random data roots let a phone read the journal with no network, and make
  revocation of read access possible.

**The smallest first step that a user can try.** Change only the signer:

- `arc keys bunker` keeps a device list on disk, one entry for each client
  key, with a name.
- A new device pairs with a one-time code that the signer prints. The fixed
  secret goes away.
- `arc keys devices` lists the devices. `arc keys revoke <device>` removes
  one.

The test: pair two machines with a signer, revoke one, and show that its next
signature request fails and the other machine still works. This step does not
change sealing, mail or any wire format.

Until then, a user can do a crude revocation with today's code: delete the
`bunker-secret` file of the signer, restart the signer, and add the bunker
again on each remaining device. This comes from reading the code
(`cmd/arc/keys.go:557-563`, `:658-662`) and is not tested.

The second step is the data root (section 5). The third step is the device
list for the locker (issue #172).

## 8. Open questions for Zim

1. **Do phones give up the citizen key?** Recommended: yes. The key lives on a
   signer and on paper. Otherwise a lost phone means a new identity.
2. **Who runs the signer for a user with no machine that is always on?**
   Recommended: the same machine that runs the locker of issue #172. Until one
   exists, that user keeps the key on the phone, and accepts that a loss means
   a new identity.
3. **Must a phone sign with no internet?** Recommended: no, for now. Mail
   waits in the outbox until the phone reaches the signer. A FROSTR 2-of-3
   signer (phone, laptop, home) is the later answer.
4. **What does the paper hold?** Recommended: the citizen key as an
   `ncryptsec`. A separate recovery key needs the signer to be alive, and
   the paper must work when the signer is lost too. The paper holds no
   locker links. They are in a sealed event on the relays (section 5.4).
5. **Do random data roots replace keys from the citizen key?** Recommended:
   yes. Accept that a plain Nostr client with the nsec no longer reads new ARC
   drafts.
6. **Re-seal old drafts at once, or when they change?** Recommended: after a
   revocation, the signer re-seals in the background each draft under a
   generation that the revoked device held. Other drafts, and generation 0,
   re-seal when they change. A revoked device never held generation 0,
   because it opened that data only through the signer.
7. **Is the device list public or sealed?** Recommended: sealed to the
   citizen key. A public list tells everyone how many devices a citizen has.
   The locker gets no copy. The signer signs a grant or a revocation for each
   device key with an account key for that one locker, as the locker
   proposal (issue #172) describes. Thus a locker never sees the citizen key.
8. **Name a successor key now, for a stolen signer or paper?** Recommended:
   not now. Keep kind 10272 reserved. Do it after the device list works.
9. **May a device key sign drafts for a locker directly?** Recommended: no.
   A draft is named by its author, so drafts of two device keys never replace
   each other. The citizen key signs each draft through the signer. The cost:
   a write needs a path to the signer. A read does not.

## 9. Sources

Nostr, checked 2026-10-11:

- NIP-17: https://github.com/nostr-protocol/nips/blob/master/17.md
- NIP-26, marked unrecommended: https://github.com/nostr-protocol/nips/blob/master/26.md
- Deprecation of NIP-26, merged 2025-04-29: https://github.com/nostr-protocol/nips/pull/1051
- NIP-26 relay support of 0.01%: https://github.com/nostr-protocol/nips/pull/1116
- NIP-37: https://github.com/nostr-protocol/nips/blob/master/37.md
- NIP-44: https://github.com/nostr-protocol/nips/blob/master/44.md
- NIP-46: https://github.com/nostr-protocol/nips/blob/master/46.md
- NIP-49: https://github.com/nostr-protocol/nips/blob/master/49.md
- NIP-41 drafts, not merged: https://github.com/nostr-protocol/nips/pull/158 ,
  https://github.com/nostr-protocol/nips/pull/829 ,
  https://github.com/nostr-protocol/nips/pull/2114 ,
  https://github.com/nostr-protocol/nips/pull/2137
- NIP-102 subkey attestation, open: https://github.com/nostr-protocol/nips/pull/1450
- Linked subkeys, open: https://github.com/nostr-protocol/nips/issues/1810
- NIP-4e, one encryption key sealed to each client key, open:
  https://github.com/nostr-protocol/nips/pull/1647
- A separate encryption key for NIP-17, open, with objections:
  https://github.com/nostr-protocol/nips/pull/2361
- FROSTR threshold signing: https://frostr.org/ ,
  https://github.com/frostr-org/bifrost

Other systems:

- Signal Sesame: https://signal.org/docs/specifications/sesame/
- Signal linked devices share the identity key (provisioning message):
  https://github.com/signalapp/Signal-iOS/blob/b4295d7839b922e5ae28e1e4c3ed7d2bf5f059c6/SignalServiceKit/protobuf/Provisioning.proto
- Signal history transfer to a linked device:
  https://signal.org/blog/a-synchronized-start-for-linked-devices/
- Matrix cross-signing and key backup:
  https://github.com/matrix-org/matrix-spec/blob/main/content/client-server-api/modules/end_to_end_encryption.md ,
  https://github.com/matrix-org/matrix-doc/blob/master/proposals/1756-cross-signing.md ,
  https://github.com/matrix-org/matrix-doc/blob/master/proposals/1946-secure_server-side_storage.md
- Keybase devices, paper keys, per-user keys and the sigchain:
  https://book.keybase.io/account ,
  https://book.keybase.io/docs/teams/puk ,
  https://book.keybase.io/docs/teams/sigchain
- age: https://age-encryption.org/v1
- Tink envelope encryption: https://developers.google.com/tink/client-side-encryption
- Apple Advanced Data Protection recovery: https://support.apple.com/en-us/108756
- SLIP-39 Shamir backup: https://github.com/satoshilabs/slips/blob/master/slip-0039.md
