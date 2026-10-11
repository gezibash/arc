# Devices

- Status: built
- Layers: core, adapters, runtime
- Owns: core/devices
- Proof: go test -count=1 -run '^(TestDevices|TestDeviceList)$' ./internal/proof/ ./core/devices/
- Unverified: No test runs the signer and a device on separate machines or networks. No test measures the time of one request through the signer. No test publishes a device list from a machine other than the signer, as recovery from the seed will do.

A citizen keeps the citizen key on one signer. Each other device has its own
random device key, and signs and decrypts through the signer with NIP-46. The
citizen key signs a device list. The signer admits only the device keys on
the list. This is step 1 of [the base proposal](../proposals/base.md).

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

This spec does not define random data roots, the sealing of journal data,
pairing codes, a policy for each device, or storage citizens. The base
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
- The signer MUST refuse to sign kind 10274 for any device, whatever its
  `--allow-kind` flags say. A device must not add itself back.
- A manifest MUST NOT name kind 10274. `runtime/iface` reserves it.

## 4. Behavior

### 4.1 Add a device

1. On the device: `arc keys device` prints the device key. It makes the key
   first if there is none.
2. On the signer: `arc keys devices add <device key> --name <name>`. The
   signer reads the newest list from its store and its relays, adds the
   device, seals the new list, keeps it, and sends it to its relays.
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

### 4.4 Restart

At start, and before it prints its URI, the bunker loads the newest list from
its store, from the relays of the citizen, and from its own relay. Thus a
restart keeps the list.

## 5. Failures

| Failure | What happens |
| --- | --- |
| A device that is not on the list asks the signer | The signer answers each request with a refusal. The command of the device fails, and says that the key is not on the device list. |
| A device asks the signer to sign kind 10274 | The signer refuses, and says that it never signs a device list. |
| `arc keys devices add` or `revoke` runs on a device | The device has no citizen key. The command fails, and says that only the signer changes the device list. |
| No relay takes a new list | The command fails. The list stays in the store of the signer, and a later sync sends it. The bunker keeps the list that it has. |
| No relay answers when `add` or `revoke` starts, and the store holds no list | The command fails. It does not make a list that could replace a list on a relay. |
| A list does not open | The bunker logs a warning and keeps the list that it has. |
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
- **A device cannot change the list.** The signer never signs kind 10274 for a
  device. A device key cannot sign a list that a reader accepts.
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
- Kind 10272 stays reserved for a migration record. Kind 10273 is the
  storage list of [the storage proposal](../proposals/storage.md).

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
  10274, also with `--allow-kind 10274`.
