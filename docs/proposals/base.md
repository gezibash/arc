Status: proposed

# The base: identity, device keys and sealing

This proposal answers [issue #173](https://github.com/gezibash/arc/issues/173).
No code implements it. It also covers key backup and recovery from issue #49.

Each claim about ARC names a file and a line, at commit `7eaeed8`. Each number
says "measured" or "estimated". The sources are in section 9.

## 0. Summary

- The citizen key stays one secp256k1 key. Nostr clients see one public key,
  as they do today.
- A 24-word seed (NIP-06) derives the citizen key. The seed is on paper. A
  phone uses the seed only for recovery, never for daily use.
- The citizen key leaves the phones. It lives on one signer, a machine of the
  citizen that is always on.
- Each device has its own random device key. A device signs and opens mail
  through the signer. The signer keeps a list of devices, and refuses a
  revoked device.
- The journal is a local app. A device seals each change with a random data
  root and sends the encrypted bytes to a storage citizen. Another device of
  the citizen fetches the bytes and opens them. A revocation makes a new data
  root.
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

Carriage belongs to the storage citizen design (issue #172). This proposal
does not change carriage.

ARC has no seed today. No code uses BIP-39 or NIP-06. The Nostr library that
ARC pins has a `nip06` package (`fiatjaf.com/nostr/nip06`, at the version in
`go.mod:6`), but ARC does not import it.

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
| 1 | The phone is lost. The thief has it, possibly unlocked. | Nothing that the phone holds can change the identity. | The phone holds no citizen key and no seed. | The phone holds the journal pages that it already synced. |
| 2 | The citizen buys a new phone and installs `arc`. | None yet. | The new phone makes its own random device key. | None yet. |
| 3 | The citizen proves that the new phone is theirs. | Something outside the phone: the seed on paper. | The 24 words derive the citizen key in memory, for recovery only. | None. |
| 4 | The old phone is revoked. | The citizen key signs a new device list without the old device key. The signer loads it. | The signer refuses each later request of the old device key. The storage citizen refuses the old device. | A new data root replaces the one that the old phone knows. |
| 5 | The new phone reads the journal. | The new phone is on the device list, and has a grant from the storage citizen. | The new phone gets the data roots, wrapped to its device key. | The new phone fetches the encrypted bytes from the storage citizen, and opens them. |
| 6 | The new phone gets mail. | Senders still use the one public key. Nothing changes for them. | The signer opens each gift wrap for the new phone. | Mail on the relays stays sealed to the citizen key, as NIP-17 requires. |
| 7 | The thief tries to use the old phone. | The signer refuses it. Relays that ask NIP-42 refuse it, because the signer does not answer. | The old phone has a device key that nothing accepts. | It keeps the pages that it cached. See section 4.3. |

Today, step 4 is impossible. The phone holds the citizen key, so the thief
is the citizen until the citizen moves to a new key. Section 4.4 describes
that cost. A seed does not change this: if the phone holds the citizen key,
the seed recovers the key, and the thief still holds it too.

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

**Recommendation: D, with device keys inside ARC only.** The device keys do
not sign Nostr events that other people read. They identify a device to the
signer, receive wrapped data roots, sign journal changes inside the encrypted
bytes, and identify a device to a storage citizen. Thus a plain Nostr client
sees no change. NIP-4e (nips#1647, open) has the same shape: one client key for
each device, and one random encryption key sealed to each client key.

Option B fails for ARC because mail is NIP-17 (`docs/delivery/SPEC.md:202-218`
uses the `p` tag form so that mail reaches any NIP-17 client). Option C repeats
the failure of NIP-26. Option D2 is a good later step, see section 8,
decision 3.

The costs of option D:

- A signer must be on and reachable. The signer is a machine of the citizen.
  A storage citizen must not be the signer, because it is another party.
- Each signature and each mail open is one round trip through a relay to the
  signer. ARC allows 20 seconds for each operation
  (`docs/interface/SPEC.md:593-597`). That is a limit, not a measurement. The
  real time is not measured.
- A phone with no path to the signer cannot sign Nostr events or open mail.
  It can still read and write the journal, because it holds the data roots and
  its own device key (section 5).
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
| Storage citizen | The signer sends a revocation for the old device key. The storage citizen refuses it after that (issue #172). | Not applicable. No storage citizen exists. |

### 4.2 The signer must keep a device list

The signer must keep one entry for each device key. Each entry holds the
public key of the device, a name, the time of admission, and a policy: the
kinds that it may sign, and whether it may open mail. The signer must refuse
each request from a device key that is not on the list. The list must persist
across restarts.

Today the signer keeps its authorizations only in memory
(`cmd/arc/keys.go:557`), and one fixed secret admits each client
(`cmd/arc/keys.go:559`). The client key that each device needs exists already:
`cmd/arc/keys.go:85-90` makes and keeps one random key for each client.

A new device gets on the list in one of two ways:

1. A device that is on the list approves it. The signer shows a one-time
   pairing code.
2. The citizen key signs a new device list. The seed gives the citizen key,
   see section 6.

On the trip, the citizen has only way 2.

### 4.3 What a thief can do with the old phone

Before revocation, the window is from the loss to step 4. In the story, the
window is some hours (estimated, from "by evening").

In that window, with option D, the thief can:

- Sign any kind that the device policy allows, as the citizen.
- Open all mail and all ciphertext sealed to the citizen key, if the device
  may open mail.
- Send journal changes to the storage citizen, signed by the old device key.
  The citizen can find them, because each change names its device key, and
  can drop the changes of that device after the time of the loss.
- Read the pages that the phone cached, and the data roots that it holds.

After revocation, the thief can:

- Read the pages that the phone already holds, with the data roots that it
  already holds. No key design removes this. Only the disk encryption of the
  phone and the lock of the app protect it.
- Not sign, not open new mail, not authenticate again to a relay or a storage
  citizen, and not read data under the new data root. A connection that
  authenticated before the revocation stays open until it ends.

Sent mail text is not stored on disk. The outbox keeps only the ID of the
sender's own seal (`core/mail/mail.go:72-74`), and the seal is sealed to the
citizen key. Thus mail on the phone is safe after revocation, with option D.

### 4.4 Without a signer: a new identity

If a citizen keeps the key on the phone (option A), revocation means a new
key. A seed does not help: it recovers the key, and the thief holds the same
key. The costs of a new key:

- The petname changes, because it follows from the public key
  (`core/keys/petname.go:9-17`). The citizen gets a new name.
- Each install trust of each other citizen names the old key
  (`runtime/catalog/catalog.go:293`).
- Each contact must learn the new key.
- The thief also holds the old key, and can publish a competing migration.
  Only a successor key that the citizen named in advance can win. NIP-41
  drafts (nips#829, nips#2137) use this method. None is merged.

ARC reserves kind 10272 for a migration record (`docs/delivery/SPEC.md:856`).
This proposal keeps it reserved. A migration is for a stolen signer or seed,
not for a lost phone.

## 5. Sealing

### 5.1 Today: keys from the citizen key, drafts on the relays

The journal and the files app keep their data as NIP-37 drafts on the relays
(`docs/delivery/SPEC.md:719-730`). A draft, a checkpoint, a part and the
private relay list are NIP-44 ciphertext from the citizen to itself
(`core/draft/draft.go:100`, `:201`, `:259`). The conversation key is ECDH of
the citizen key with its own public key (`core/keys/keys.go:123`). The keyed
root, which makes the `d` tags, is HKDF of the secret key
(`runtime/iface/run.go:85`).

The results:

- Each holder of the citizen key reads all self data, of the past and the
  future.
- A device cannot read self data without the citizen key, or without one
  round trip to the signer for each event.
- A device cannot lose access. Revocation of read access is impossible.

### 5.2 Proposed: a local journal and random data roots

The journal is a local app. Its pages live in a store on each device. The
relays do not hold them. In the first version, each page is a signed Nostr
event that only the devices of the citizen see, and the device key signs it.
Sync works like this:

1. A device writes a change to its local store.
2. The device seals the change into encrypted bytes, and sends the bytes to a
   storage citizen. Issue #172 defines the format and the protocol.
3. Another device of the citizen fetches the bytes from the storage citizen,
   opens them, and applies the change to its own store.

The keys:

- A **data root** is 32 random bytes with a generation number.
- The content key of one app is
  `HKDF-SHA256(ikm = data root, info = "arc-data-v1" || 0x00 || app id)`.
  Thus the journal cannot open the bytes of the files app.
- The encrypted bytes name the generation of their data root in clear text.
- A **wrap** is one data root, sealed with NIP-44 to one device key. One more
  wrap seals it to the citizen key. The sealed root is 176 characters
  (measured, NIP-44 of the root as hex). The signer and the storage citizen
  keep the wraps. Neither can open a wrap to a device key.
- The device key signs each change inside the encrypted bytes. A device that
  opens a change checks that its device key is on the device list. Thus a
  phone writes the journal with no path to the signer, and the signer does
  no work for the journal. The storage citizen refuses writes from a revoked
  device (section 8, decision 9).
- On revocation, the signer makes a new generation and wraps it to each
  remaining device key and to the citizen key. New writes use the new
  generation. Old bytes stay under their old generation.
- Each new generation carries the previous data root, sealed under the new
  one. Thus the newest data root opens each older generation. ARC never
  deletes or changes a generation.
- The wrap of each generation to the citizen key means that the citizen key
  always recovers each generation. The seed therefore recovers all data.

This is the per-user key of Keybase: a random seed, boxed to each device and
to each paper key, and rotated when a device is revoked. age and Tink use the
same pattern for each file: a random key, wrapped to each recipient.

The keyed root does not change. It is a key for names, not for content.
Only `KeyedValue` uses it (`runtime/citizen/environment.go:40-56`). It
follows from the citizen key, so the seed derives it again.

### 5.3 What stored data must migrate

| Data | Kind | Change |
| --- | --- | --- |
| Journal pages and files on the relays, as drafts, checkpoints and parts | 31234, 1234, 3275 | One import. The app opens each draft through the signer, with `--decrypt self`, writes it to its local store, and sends it to the storage citizen under the newest generation. Then the app deletes the drafts on the relays with NIP-09. The ARC store applies a NIP-09 deletion and refuses the deleted event after that (`docs/delivery/SPEC.md:725-729`). |
| Private relay list | 10013 | Issue #172 removes it. |
| Keyed root draft | 31234, `d` = `arc-keyed-root` | It stays sealed to the citizen key. It is also wrapped to each device key, like a data root. |
| Mail seals | 13 in 1059 | No change. NIP-17 requires mail sealed to the citizen key. |

The cost of the import is one decrypt through the signer for each draft,
checkpoint and part. The number of drafts of a real citizen is not measured.

A Nostr client that holds the nsec opens today's drafts, because NIP-37 seals
them to the author. After the import, no Nostr client reads ARC self data.
Only ARC reads it.

### 5.4 How the new phone gets the data roots

1. The new phone is on the device list (section 4.2).
2. The new phone asks the signer for the data roots.
3. The signer opens each wrap to the citizen key with `--decrypt self`, and
   wraps each data root to the device key of the new phone.
4. The new phone opens the wraps with its own device key, and keeps the data
   roots on its disk.
5. The new phone opens the bytes from the storage citizen with no further
   call to the signer.

If the signer is lost too, the seed gives the citizen key, which opens the
wraps to the citizen key directly (section 6).

A new device must also find the storage citizens of the citizen (issue #172).
The signer holds the link of each storage citizen, and gives the links to a
new device with the data roots. The signer also publishes the links to the
relays of the citizen, in one replaceable event of kind 10273, sealed to the
citizen key with NIP-44, with the NIP-70 tag. The storage citizen proposal
defines this kind. The kind table of the NIPs README does not list 10273
(checked 2026-10-11). Another kind registry is not checked. The paper holds
only the seed, because a link can change and paper cannot. With the seed, a
new device fetches that event and opens it. This event is coordination data,
so it stays on the relay.

## 6. Backup and recovery of the citizen key

The citizen key is the last resort for each step above. Today its loss is
fatal (`docs/delivery/SPEC.md:847`).

### 6.1 The seed

- A seed is 24 words of BIP-39, from 256 bits of entropy.
- The citizen key is the NIP-06 key of the seed, at the path
  `m/44'/1237'/0'/0/0`.
- `fiatjaf.com/nostr/nip06` makes the words and derives the key at that path.
  Its `SeedFromWords` uses an empty BIP-39 passphrase.
- The seed derives the keyed root too (section 5.2), because the keyed root
  follows from the citizen key.

**The seed is for recovery, not for daily use.** In the story, the citizen
types the 24 words on the new phone. The phone derives the citizen key in
memory and signs a new device list: its own device key is in, and the old one
is out. Then it erases the words and the key, and never stores them. The
signer refuses NIP-46 requests from a device key that is not on its list, so
the phone cannot send the change as a request. The phone publishes the device
list, signed by the citizen key, to the relays of the citizen. The signer
watches for that event and reloads its list, as a machine watches for the
keyed root draft today (`runtime/citizen/identity.go:16-17`). The citizen key
is on the new phone for some seconds (estimated). That is the cost of
recovery without a second device.

A seed with no passphrase gives the citizen key to anyone who reads the
paper. A BIP-39 passphrase prevents this, but the citizen must remember it on
the trip. Thus the passphrase is an option, and off by default (section 8,
decision 6).

### 6.2 Device keys do not come from the seed

Each device makes its own random device key. The seed derives only the citizen
key. The reasons:

- To derive a device key from the seed, the device needs the seed, or the
  signer must send the device key over the network. A random key never leaves
  its device.
- ARC never restores a device key. A lost device is revoked, and a new device
  gets a new key. Thus derivation gives nothing to recover.
- With BIP-32 derivation that is not hardened, one child secret key and the
  parent public key give the parent secret key. The NIP-102 discussion dropped
  derived subkeys for this reason.

If Zim wants device keys from the seed, they must use hardened derivation at
their own index, for example `m/44'/1237'/0'/1'/<n>'`. The device list must
then record each index that it ever used, and the highest index. A revoked
index stays revoked because the signer never gives an index again: each new
device gets the highest index plus one. A thief with the key of one index
cannot derive another index, because the derivation is hardened.

### 6.3 Citizens with a key from before the seed

Each key of today is random (`core/keys/keys.go:24-28`). No seed derives it.
Such a citizen has two choices:

- Keep the key, and keep a paper copy as a NIP-49 `ncryptsec`, sealed with a
  passphrase. ARC makes this today with `arc keys encrypt`
  (`core/keys/keys.go:70-85`).
- Make a new identity from a seed, and pay the costs of section 4.4 once.

Thus `ncryptsec` stays, for these citizens only.

### 6.4 Other methods

| Method | What it gives | Cost | Status in ARC |
| --- | --- | --- | --- |
| Threshold | The seed split into shares, for example 2 of 3 with SLIP-39. Shares go to paper, a friend, a second place. | More pieces. No single paper gives the key. | Not built. |
| Threshold signer | FROSTR shares that sign together. The key never exists in one place. | Each signature needs two online shares. | Not built. |
| Another device | A device on the list admits a new device (section 4.2, way 1). | It does not recover the citizen key if the signer is lost. | Not built. |

A threshold split is a later step. Apple's Advanced Data Protection shows the
same choice: a recovery contact or a recovery key, and no recovery if both
fail.

## 7. Recommendation and the first step

**Recommendation.** Use option D with device keys inside ARC only:

1. A 24-word seed derives the citizen key. The seed is on paper.
2. The citizen key lives on the signer, not on phones. A phone uses the seed
   only to recover.
3. Each device has a random device key. The signer keeps a device list, and
   refuses a revoked device.
4. The journal is a local app. It syncs encrypted bytes through a storage
   citizen, sealed with random data roots that are wrapped to each device and
   to the citizen key. Each revocation makes a new generation.
5. Mail and Nostr identity stay as they are.

The reasons:

- Nothing changes for other citizens or for plain Nostr clients. This is the
  only option where "dead to everyone" needs no action from anyone else.
- The parts exist: the bunker, the client key of each device, NIP-06 in the
  pinned library, and the keyed root draft, which already wraps a key in a
  draft (`runtime/citizen/identity.go:23-42`).
- Nostr tried delegation and device subkeys, and they failed or stalled
  (NIP-26 deprecated, NIP-41 and NIP-102 still open). Bunkers are what Nostr
  users use.
- Random data roots let a phone read and write the journal with no network,
  and make revocation of read access possible.

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

The second step is the seed: `arc keys gen` makes 24 words, and a recovery
command derives the key from them. The third step is the data root and the
local journal with a storage citizen (issue #172).

## 8. Decisions

Zim decided each item on 2026-10-11. Items 1 and 5 came first. The other
items take the recommended answer of this proposal.

1. **Phones give up the citizen key. Phones are devices.** Zim, 2026-10-11.
   Reason: only then can a lost phone be revoked without a new identity.
2. **A machine of the citizen runs the signer. A storage citizen never runs
   it.** Zim, 2026-10-11. Reason: a storage citizen is another party. Until a
   user has such a machine, the phone holds the key, and a loss means a new
   identity.
3. **A phone does not sign Nostr events with no internet, for now.** Zim,
   2026-10-11. Reason: mail can wait in the outbox, and the journal works
   offline. A FROSTR 2-of-3 signer is the later answer.
4. **The paper holds the 24-word seed for each new citizen.** A citizen with
   a key from before the seed keeps an `ncryptsec`, or moves to a new
   identity once (section 6.3). The paper holds no links (section 5.4). Zim,
   2026-10-11. Reason: a seed derives the key again, and no seed derives a
   key of today.
5. **Random data roots replace keys from the citizen key.** Zim, 2026-10-11.
   Reason: only a random key wrapped to each device allows revocation of read
   access.
6. **The seed has no BIP-39 passphrase by default. It is an option.** Zim,
   2026-10-11. Reason: a forgotten passphrase on a trip blocks recovery. The
   paper must be kept like a passport.
7. **Device keys do not come from the seed. Each device makes a random
   key.** Zim, 2026-10-11. Reason: a random key never leaves its device, and
   ARC never restores a device key (section 6.2).
8. **The device list is sealed to the citizen key.** The storage citizen gets
   no copy. The signer signs a grant or a revocation for each device key with
   an account key for that one storage citizen (issue #172). Zim, 2026-10-11.
   Reason: a public list tells everyone how many devices a citizen has, and a
   copy shows the citizen key to each storage citizen.
9. **The device key signs each journal change inside the encrypted bytes.**
   The signer does no work for the journal. The storage citizen refuses
   writes from a revoked device. Zim, 2026-10-11. Reason: a phone then writes
   the journal offline, with no path to the signer.
10. **Today's drafts are imported once, and deleted from the relays.** Zim,
    2026-10-11. Reason: the journal is a local app, and the relays are for
    coordination only (section 5.3).
11. **No successor key now. Kind 10272 stays reserved.** Zim, 2026-10-11.
    Reason: a successor key is for a stolen signer or seed. It follows the
    device list.

## 9. Sources

Nostr, checked 2026-10-11:

- NIP-06: https://github.com/nostr-protocol/nips/blob/master/06.md
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

- BIP-39 seed words: https://github.com/bitcoin/bips/blob/master/bip-0039.mediawiki
- BIP-32 hardened derivation: https://github.com/bitcoin/bips/blob/master/bip-0032.mediawiki
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
