# Devices

- Status: built
- Layers: core, adapters, runtime
- Owns: core/devices
- Proof: go test -count=1 -run '^(TestDevices|TestDeviceList|TestDataRoots|TestRootsEvent|TestContentKey|TestRootsSize)$' ./internal/proof/ ./core/devices/
- Unverified: No test runs the signer and a device on separate machines or networks. No test measures the time of one request through the signer. No test publishes a device list from a machine other than the signer, as recovery from the seed will do. No test runs a device with no path to the signer: today each `arc` command of a device connects to the signer first, so a device cannot open the data roots that it holds without the signer. No test shows a revoked device that holds the newest roots event and fails to open its wrap through the CLI: the signer refuses the device first. The proof of the data roots in self sync reads the root cache of the revoked phone from its disk, because the signer and the storage citizen refuse the phone before it gets a blob.

A citizen keeps the citizen key on one signer. Each other device has its own
random device key, and signs and decrypts through the signer with NIP-46. The
citizen key signs a device list. The signer admits only the device keys on
the list. The signer also wraps random data roots to each device on the list,
and makes a new data root when it revokes a device. These are steps 1 and 2
of [the base proposal](../proposals/base.md).

## 1. Purpose

Today one key is the citizen on each device. A lost phone is a lost key, and
the only remedy is a new identity. With a signer, a lost phone holds no
citizen key. The citizen removes its device key from the list, and the signer
refuses each later request of that key. Other citizens and plain Nostr
clients see no change, because each event still has a signature of the one
citizen key.

Before this spec, the bunker admitted each client that gave one fixed secret,
and kept its admissions only in memory. A copy of the `bunker://` URI admitted
a thief again after each restart.

This spec does not define the sealing of journal data into blobs, pairing
codes, a policy for each device, or storage citizens. The base
proposal and [the storage proposal](../proposals/storage.md) describe them.

## 2. Terms

- **Citizen key**: the key of the citizen, as
  [the delivery spec](../delivery/SPEC.md), section 3.2, defines.
- **Signer**: the machine that holds the citizen key and runs
  `arc keys bunker`. It is a machine of the citizen, never a storage citizen.
- **Device key**: a random secp256k1 key of one device. The device uses it as
  its NIP-46 client key. It never signs an event that another citizen reads.
- **Device list**: the device keys that the signer admits, with a name and a
  time for each.
- **Data root**: 32 bytes with a generation number. A data root seals the
  self data of the citizen. Generation 0 is the keyed root of
  [the interface spec](../interface/SPEC.md), which follows from the citizen
  key. Each later generation is 32 random bytes.
- **Wrap**: one data root as 64 hex characters, encrypted with NIP-44 from the
  citizen key to one device key, or to the citizen key itself.
- **Content key**: the key of one app under one data root:
  `HKDF-SHA256(ikm = data root, salt = none, info = "arc-data-v1" || 0x00 || app id)`.

## 3. Rules

### 3.1 The device key

- A device MUST make its device key from random bytes. A device key MUST NOT
  come from the seed.
- The device key MUST NOT leave its device. A device MUST NOT restore a
  device key. A lost device gets a new key on a new device.
- `arc` keeps the device key of a machine in `<home>/device`, in the format
  of a key file. An identity that `arc keys add bunker://...` adds uses this
  key as its NIP-46 client key, in `<home>/citizens/<name>/bunker-client`.
- An identity that was added before the device key keeps its own
  `bunker-client` key. That key is its device key.

### 3.2 The device list event

The device list is one replaceable event of kind 10274:

| Field | Value |
| --- | --- |
| `kind` | 10274 |
| `pubkey` | the citizen key |
| `tags` | exactly `[["-"]]`, the NIP-70 tag |
| `content` | the list as JSON, encrypted with NIP-44 from the citizen key to the citizen key |

The JSON inside is:

```json
{"devices": [{"key": "<64 hex>", "name": "laptop", "added": 1760000000}]}
```

- Only the citizen key MUST sign a device list.
- A list MUST hold at most 64 devices. A name MUST have 1 to 64 bytes and no
  control characters. Two devices MUST NOT have the same key or the same
  name.
- A new list MUST have a `created_at` later than the list that it replaces.
  If the clock gives the same second, the new list takes the second after it.
- A reader MUST ignore a JSON field that it does not know. A later step adds
  a policy for each device in this way.
- A reader MUST refuse a list with another author, a bad signature, content
  that does not open, or a limit that is broken. Of the lists that open, the
  reader takes the newest, as NIP-01 orders replaceable events.

### 3.3 The relay

- A relay MUST serve kind 10274 only to its author after NIP-42
  authentication, as it serves a NIP-37 draft. `adapters/relay/sealed`
  enforces this for an ARC relay.
- The list is coordination data. It stays on the relays of the citizen.

### 3.4 The signer

- If no device list exists, the signer MUST admit a device that gives the
  bunker secret, as before this spec.
- If a device list exists, the signer MUST answer only a request of a device
  key on the list. The bunker secret MUST NOT admit a device.
- The signer MUST check the list for each request, not only for `connect`.
- The relay of the bunker MUST be one of the relays of the citizen. The
  bunker watches only its own relay, and `add` and `revoke` send the list to
  the relays of the citizen. The bunker warns at start when its relay is not
  one of them.
- The signer MUST refuse to sign kind 10274 for any device, whatever its
  `--allow-kind` flags say. A device must not add itself back.
- A manifest MUST NOT name kind 10274. `runtime/iface` reserves it.

### 3.5 The data roots

The data roots are one replaceable event of kind 10275:

| Field | Value |
| --- | --- |
| `kind` | 10275 |
| `pubkey` | the citizen key |
| `tags` | exactly `[["-"]]`, the NIP-70 tag |
| `content` | the roots as JSON, encrypted with NIP-44 from the citizen key to the citizen key |

The JSON inside is:

```json
{"gen": 2, "citizen": "<wrap>", "devices": {"<64 hex>": "<wrap>"}, "chain": "<sealed older roots>"}
```

- `gen` is the newest generation. `citizen` wraps its root to the citizen
  key. `devices` wraps its root to each device key.
- `chain` holds each older random root, as the JSON object
  `{"1": "<64 hex>", ...}`, encrypted with the NIP-44 payload format and the
  conversation key `HKDF-SHA256(ikm = newest root, salt = none, info = "arc-data-chain-v1")`.
  Generation 0 is not in the chain. The first generation has no chain.
- Only the citizen key MUST sign the roots. Readers apply the rules of
  section 3.2 for the author, the signature, the content and the newest event.
- `gen` MUST be from 1 to 128. The event MUST hold at most 64 device wraps.
  With 64 devices and 128 generations, the event is 44,125 bytes (measured by
  `TestRootsSize`), below the limit of 65,536 bytes for one kept event.
- The relay rules of section 3.3 apply to kind 10275. The signer MUST refuse
  to sign kind 10275 for any device. A manifest MUST NOT name kind 10275.

The signer keeps the roots in line with the device list:

- When the signer publishes the first device list, it MUST make generation 1
  and wrap it to the citizen key and to each device on the list.
- When the list adds a device, the signer MUST wrap the newest root to that
  device. The generation does not change. Through the chain, the new device
  opens each older generation.
- When the list drops a device that holds a wrap of the newest root, the
  signer MUST make a new random root of the next generation. It wraps the new
  root only to the citizen key and to the devices on the list, and seals each
  older random root into the chain under the new root.
- The signer MUST NOT change or delete an older generation. Data sealed under
  an older generation stays under it. Nothing seals old data again.

A device reads the roots like this:

1. The device fetches the newest roots event, and opens its content through
   the signer, which decrypts as the citizen.
2. The device opens the wrap to its own device key with its device key, and
   opens the chain with the newest root.
3. The device keeps each root on its disk, and adds generation 0 from the
   keyed root.

A machine that holds the citizen key, such as the signer or a machine that
recovered the seed, opens the wrap to the citizen key in step 2.

Data sealed with a data root:

- New data MUST be sealed under the newest generation that the machine holds.
- The sealed data MUST name its generation in clear text, so that the reader
  knows which root opens it.
- `arc keys seal <app>` writes the generation in decimal, a colon, and the
  NIP-44 payload of the text with the content key of the app as the
  conversation key. A self-sync blob names its generation in its header, and
  its blob key comes from the data root of that generation, as
  [the storage spec](../storage/SPEC.md), section 3.4, defines.

Issue #204 asked that data of an old generation is sealed again in the
background, and named decision 6 of the base proposal. Section 5.2 of the
proposal uses a chain instead, and this spec follows it. A revoked device already
held the old root and the data that it sealed, so a new seal of old data
hides nothing from it. A new generation hides only the data that comes after
the revocation, and the chain lets each later root open the older data.

## 4. Behavior

### 4.1 Add a device

1. On the device: `arc keys device` prints the device key. It makes the key
   first if there is none.
2. On the signer: `arc keys devices add <device key> --name <name>`. The
   signer reads the newest list from its store and its relays, adds the
   device, seals the new list, keeps it, and sends it to its relays. It then
   sends the grant set to each storage citizen that it installed
   ([the storage spec](../storage/SPEC.md), section 4.4).
3. The running bunker watches its relay for kind 10274 of the citizen, and
   loads each newer list.
4. On the device: `arc keys add bunker://<citizen>?relay=<url>`. With a
   device list, the bunker prints its URI without the secret.

If the signer refuses the device in step 4, `arc` names the device key and
the command to run on the signer.

### 4.2 List the devices

`arc keys devices` prints one line for each device: the name, the device key
and the day of admission.

### 4.3 Revoke a device

On the signer: `arc keys devices revoke <name or device key>`. The signer
publishes a new list without the device. The bunker loads it from the relay.
The next request of the revoked device gets the refusal "the signer refuses
this device: its key is not on the device list". The other devices go on.
The signer also sends the new grant set to each storage citizen that it
installed, and each of them refuses the device key with `not_owner`.

### 4.4 The data roots

- `arc keys devices add` and `revoke` print `data roots: generation <n>`
  after they publish the list.
- `arc keys roots` lists each generation that the machine holds, the newest
  first. On the signer, it first makes the roots follow the device list, and
  then names each device key that holds a wrap of the newest generation.
- `arc keys seal <app>` seals standard input under the newest generation.
  `arc keys unseal <app>` opens it on each machine that holds its generation.

### 4.5 Restart

At start, and before it prints its URI, the bunker loads the newest list from
its store, from the relays of the citizen, and from its own relay. Thus a
restart keeps the list.

## 5. Failures

| Failure | What happens |
| --- | --- |
| A device that is not on the list asks the signer | The signer answers each request with a refusal. The command of the device fails, and says that the key is not on the device list. |
| A device asks the signer to sign kind 10274 | The signer refuses, and says that it never signs a device list. |
| `arc keys devices add` or `revoke` runs on a device | The device has no citizen key. The command fails, and says that only the signer changes the device list. |
| No relay takes a new list | The command fails. The list stays in the store of the signer, so the bunker loads it at its next start. The next `add` or `revoke` sends a newer list to the relays. The running bunker keeps the list that it has. |
| No relay answers when `add` or `revoke` starts, and the store holds no list | The command fails. It does not make a list that could replace a list on a relay. |
| A list does not open | The bunker logs a warning and keeps the list that it has. |
| No relay takes the roots after `add` or `revoke` | The command fails after the list is published, and names `arc keys roots`. The signer then makes the roots follow the list. Until then, the other devices seal under the old newest root, which the dropped device holds. |
| A device cannot reach the signer or the relays | It uses the data roots that it keeps on its disk, and warns that the newest roots did not come. |
| Data names a generation that the machine does not hold | `arc keys unseal` fails, and names the generation. |
| The citizen has 128 generations | The signer refuses to make another one. `revoke` fails after it publishes the list, so the signer refuses the device, but the device keeps the newest root, and data sealed after the revocation stays readable to it. No command repairs this yet. |
| The bunker starts with no relay, and its store holds no list | The bunker admits by the bunker secret until it loads a list. On the signer, `add` and `revoke` keep each list in the store, so this happens only when another machine made the list. |

## 6. Security

- **Before revocation.** A device on the list signs each kind that the
  bunker allows, and decrypts what `--decrypt` allows, as the citizen. A thief
  with an unlocked device has this power until the citizen revokes it.
- **After revocation.** The signer refuses each request of the device key.
  The device keeps what it already decrypted and stored. A relay connection
  that authenticated before the revocation stays open until it ends.
- **A hostile relay** can withhold a new list, or serve an older one. The
  bunker never goes back to an older list than the one it holds. A relay that
  withholds a revocation delays it until the bunker reads the list from
  another relay or from its store. The relay cannot read the list, and serves
  it only to the citizen.
- **A device can read the list.** With `--decrypt self`, the default, the
  signer opens each content that the citizen sealed to itself. The device
  list is such content. Thus a device on the list can learn the other device
  keys and names.
- **A device cannot change the list.** The signer never signs kind 10274 for a
  device. A device key cannot sign a list that a reader accepts.
- **Data roots after revocation.** A revoked device keeps each root that it
  opened before. It never gets a root of a later generation: the new root has
  no wrap to its key, and the chain opens only with the new root. Thus it
  cannot read data that a device seals after the revocation.
- **The seed** derives the citizen key, which opens the wrap of the newest
  root to the citizen key, and through the chain each older root. The seed
  also derives the keyed root, which is generation 0. Thus the seed alone
  reads all data, also when the signer is lost.
- **A device can read the roots event.** Through the signer, it opens the
  content, and sees the device keys that hold wraps. It cannot open the wraps
  to other device keys.
- **The list size** is at most 64 devices. A list is one small event, so the
  relay stores one event for each citizen.

## 7. Compatibility

- **A signer with no list** behaves as before this spec. Its URI keeps the
  secret, and each existing `bunker://` identity keeps working.
- **An older device** that connects to a new signer with a list: the signer
  admits it if its `bunker-client` key is on the list, and refuses it if not.
  The device needs no update.
- **An older signer** ignores kind 10274, and admits by its secret.
- **An older relay** that does not know kind 10274 serves it to anyone who
  asks by author. The content stays sealed. Only the number of lists shows.
- **Key files** keep their format. An identity with a key file of its own
  needs no device key.
- **Data sealed before the data roots** is generation 0: the keyed root of
  the citizen key. A citizen with no device list has no roots event, and
  seals under generation 0, as before this spec. Each reader opens
  generation 0 with the keyed root.
- **A signer with a list from before the data roots** makes generation 1 at
  its next `add`, `revoke` or `arc keys roots`.
- **An older device** ignores kind 10275, and seals under generation 0.
  Data of a later generation does not open on it. It gets the roots after
  its update.
- **An older relay** that does not know kind 10275 serves it to anyone who
  asks by author. The content stays sealed.
- Kind 10272 stays reserved for a migration record. Kind 10273 is the
  storage list of [the storage spec](../storage/SPEC.md), section 3.8.

## 8. Proof

- `TestDevices` in `internal/proof` runs the real `arc` program and a real
  local relay. A signer holds the key. Two devices make their own keys, join
  the list, and send mail that the recipient reads as the citizen. The signer
  revokes one device. The signer refuses that device, the other device still
  sends, and a restart keeps the list. A new device with the old secret URI is
  refused.
- `TestDeviceList` in `core/devices` checks the event: the kind, the NIP-70
  tag, the sealed content, the refusal of another author and of changed
  content, and a revocation in the same second.
- `TestTheBunkerPolicy` in `cmd/arc` checks that the bunker never signs kind
  10274, also with `--allow-kind 10274`, and never signs kind 10275.
- `TestDataRoots` in `internal/proof` runs the real `arc` program, a real
  local relay, a signer made from a seed, and two devices. A device seals
  data before the first list, as generation 0. After the signer adds both
  devices, each device opens the data of the other, of generation 0 and 1,
  and another app cannot open it. The signer revokes the phone, and
  the signer shows that generation 2 has a wrap only to the laptop. The
  laptop seals under generation 2 and opens each generation. The phone
  cannot open generation 2, because the signer refuses it before the phone
  reaches its roots. The core test below proves that the phone key cannot
  open generation 2 even with the event. A new machine with only the seed
  opens each generation.
- `TestDataRoots` in `core/devices` checks that a revoked device key gets no
  wrap of a new generation, that an older root cannot open the chain of a
  newer one, that a new device opens each older generation, and that data
  opens only with its own generation and app. `TestRootsEvent` checks the
  kind, the NIP-70 tag, the sealed content, and the refusal of another author
  and of changed content. `TestRootsSize` measures the largest event, and
  checks the refusal of generation 129. `TestContentKey` checks the content key against a
  value from the HMAC of the Python standard library.
