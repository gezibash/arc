Status: proposed

# Secrets: credentials for devices and agents

This proposal stores the credentials of a citizen in ARC: API keys, tokens
and passwords that the citizen's programs and agents need. It replaces a
hosted secret manager, such as 1Password with a service account, for those
credentials. No code implements it.

Secrets are self data. A device seals them with the data roots and syncs them
with the storage citizens, as it does the journal. An agent is not a device.
It gets secrets through calls to a keeper: a device of the citizen that is
always on, serves a secrets app, and checks a grant for each agent.

Each claim about ARC names a file and a line, at commit `5dca4cd`. Each
number says "measured", "estimated" or "proposed".

## 0. What this does not fix

Read this first.

- **A secret that a program has read is out.** The program holds it in
  memory, and may write it to a log or a transcript. Nothing in ARC takes it
  back. A revoked device keeps what it already decrypted
  (`docs/devices/SPEC.md:354`). The only real self-destruct is a credential
  that its provider ends: a Cloudflare token with an end date, AWS STS keys,
  a lease of a database login. ARC can store such a credential and give it
  out. It cannot make a long-lived credential short.
- **The data roots cannot forget.** The signer never deletes a generation
  (`docs/devices/SPEC.md:177`). A device never removes a root from its cache
  (`docs/devices/SPEC.md:208`). Each newer root opens each older one through
  the chain (`docs/devices/SPEC.md:154`). A citizen has at most 128
  generations (`docs/devices/SPEC.md:160`). So a secret sealed with a data
  root cannot expire by losing its key, and a generation for each secret or
  each rotation would use up the 128 fast. This is by design: a lost root
  is lost data, and the seed must open everything.
- **Crypto-shredding needs keys of its own.** Section 2.7 proposes shred
  epochs: keys that only move forward, apart from the data roots. They
  protect an expired secret from a later thief. They do not protect it from
  a device that is hostile now, or that never deletes its old key.
- **The keeper sees every secret that it serves.** It is a device of the
  citizen, so it holds the data roots and reads all self data. A thief with
  the keeper reads all secrets. This is the same trust as a Vault server or
  the 1Password service, moved to a machine of the citizen.
- **The root of trust stays one secret.** Today it is the 1Password service
  account token in a Keychain item. Here it is the citizen key and its seed
  of 24 words (`docs/proposals/base.md`, section 6). The seed alone opens
  every generation (`docs/devices/SPEC.md`, section 6).
- **Uptime is the uptime of the citizen's machines.** Each device keeps a
  full copy, so a device reads offline. An agent caches what the keeper
  gave it (section 2.5). Storage citizens on two networks are not tested
  (`apps/storage/README.md:31`).

## 1. Use cases

### 1.1 Today

Gezim's dotfiles keep only references, such as
`op://Agents/Cloudflare/R2/SecretAccessKey`. The script `op-agent` reads a
1Password service account token from the macOS Keychain, and runs a command
under `op run`, which puts the values in its environment. A project has its
own vault and token. A cloud agent sets `OP_SERVICE_ACCOUNT_TOKEN`.

This proposal keeps that shape: references in tracked files, values only in
the environment of one command.

### 1.2 A command on a device of the citizen

The citizen runs `arc secrets run --env-file .env.arc -- npm run dev` on a
laptop. The laptop is a device on the device list. It opens the secrets from
its local store, with no request to the signer, the keeper or a relay. The
command gets the values in its environment, and nothing else.

### 1.3 A local agent

An agent, such as Claude Code, runs on the same laptop. It must read the
Cloudflare token of one project. It must not read the journal, the mail, or
the secrets of another project.

If the agent runs `arc` as the laptop's device, it reads everything that the
device reads. So the agent has its own key, and calls the keeper with a
grant for one vault.

### 1.4 A cloud agent

A cloud agent runs on a machine that the citizen does not own. It gets one
agent key, as it gets `OP_SERVICE_ACCOUNT_TOKEN` today. It calls the keeper
through the relay. Its grant names the vaults it reads, and ends.

### 1.5 An agent writes a secret

An agent makes an API token for a new project, and must keep it. It calls
`put` on the keeper. Its grant must allow writes to that vault. The keeper
writes the secret as self data, and the storage citizens get it on the next
sync.

### 1.6 A lost laptop, or a leaked agent key

- The citizen revokes the laptop on the signer, as today. The laptop keeps
  each secret that it already synced. The citizen rotates those credentials
  at their providers.
- The citizen removes the agent's grant on the keeper. The keeper refuses
  the agent's next call. The agent keeps its cache until its end (section
  2.5). The citizen rotates the credentials of that vault.

## 2. The protocol shape

### 2.1 A secret is a sealed record

A secret is a sealed record of a data app with the capability id `secrets`.
It is an event of kind 30078 inside a draft, as the journal KPI is
(`docs/KINDS.md:50`). It needs no new kind.

| Field | Value |
| --- | --- |
| `d` | The keyed reference, `vault/item/[section/]field`, as the files app keys its `d` |
| `content` | The value |
| tag `vault` | The vault name. Inside the draft, so the storage citizen does not see it. |
| tag `expiration` | Optional. NIP-40, Unix seconds. |

The content key comes from the data root and the capability id
(`docs/interface/SPEC.md:377`). Thus another app cannot open a secret. A
device can: each device on the list holds every data root, and derives the
content key of every capability. Section 2.3 says why that is acceptable for
devices and not for agents.

A reference in a tracked file is `arc://<vault>/<item>/[<section>/]<field>`,
the shape of `op://`. A reference is not a secret.

### 2.2 Devices read and write directly

A device reads and writes secrets through the local store, as it does
journal pages. The storage citizens hold the blobs. Nothing new is needed on
the pipe or on the storage app.

Writes need care. The store keeps one version of a draft for each citizen and
`d` tag, whichever key signed it (`docs/interface/SPEC.md:388`). Any device
can delete a record that another device wrote
(`docs/proposals/storage.md:376`). So each device can change and delete each
secret. That is the same power that a device has over the journal.

### 2.3 Agents are not devices

An agent never joins the device list, and never gets a data root. Three
reasons:

- A data root opens every capability. A content key for each capability is
  separation between apps, not access control between programs of one
  citizen.
- With `--decrypt self`, the default, the signer opens each content that the
  citizen sealed to itself, for each device on the list
  (`cmd/arc/keys.go:959`, `docs/devices/SPEC.md:362`). A device can read the
  device list, the storage list and the data roots event.
- A device can change and delete each secret (section 2.2).

An agent has its own key. It is a citizen key with no device list, no
storage and no data roots: only a key that signs ARC calls and opens their
replies. The agent keeps it where `op-agent` keeps its token today: a
Keychain item, or an environment variable on a cloud machine.

A vault on a device is a name. A vault on the keeper is a name and a set of
grants.

### 2.4 The keeper

The keeper is a device of the citizen that serves the app `secrets`. It
runs the program `arc-secrets`, on `sdk/` only, as the storage app runs
`arc-storage`.

The keeper keeps a grant for each agent key:

```json
{"agent": "<64 hex>", "name": "claude-tezca", "read": ["Tezca"], "write": ["Tezca"], "end": 1793318400, "cache": 3600}
```

An agent calls the keeper:

```bash
arc call 'secrets+arc://<keeper>/Tezca' '{"op":"get","refs":["Tezca/Cloudflare/R2/SecretAccessKey"]}'
arc call 'secrets+arc://<keeper>/Tezca' '{"op":"put","ref":"Tezca/Stripe/SecretKey","value":"...","expiration":1793318400}'
```

The keeper finds the grant from the author of the call. It answers only for
a vault that the grant names, and only before `end`. The reply is sealed to
the agent key, as each call reply is (delivery spec, section 4.10). The relay
sees that the agent sent a call to the keeper, its time and its size.

The keeper writes a `put` to its local store as itself, a device. The
storage citizens and the other devices get it on the next sync.

Only the signer, or the citizen at the keeper, changes a grant. An agent
cannot widen its own grant. The grants live on the keeper, sealed with the
keeper's device key, and not in self data. Section 6 asks where they should
live.

### 2.5 The agent's cache

An agent caches each reply on its disk, sealed with its own key, for at
most `cache` seconds of its grant. Proposed: 1 hour by default, 24 hours at
most. A keeper that is down then blocks new and changed secrets for that
time. It blocks nothing that the agent read within the hour.

A removed grant takes effect on the next call. The cache lasts until its
end. So the cache time is also the time that a leaked agent key keeps
working without the keeper.

### 2.6 Expiry

A secret with `expiration`:

- A device refuses to give it to a command after the time, and the keeper
  refuses to serve it. The next device that syncs after the time deletes
  it, with a blank draft and a NIP-09 deletion. The storage citizens then
  drop its blobs (`docs/proposals/storage.md`, section 2.7).
- This is policy on each reader, not cryptography. A device that never
  syncs again keeps the value.

A leased secret, a later step: the keeper holds it under a random key of its
own, and never writes it to self data. It never reaches a storage citizen or
another device. At `expiration` the keeper deletes the key. A copy of the
sealed value on the keeper's disk is then noise. This is the only
crypto-shredding in this design. It needs the keeper for each read, and a
lost keeper loses each leased secret. That is the point of it.

### 2.7 Shred epochs

Crypto-shredding works when a key exists in few places and each of them
can delete it. A data root fails both: it sits in each device cache, in the
chain, and behind the seed. A shred epoch is a key that meets both.

The keys:

- An epoch is one UTC day (proposed). The key of epoch `e + 1` is
  `HKDF-SHA256(ikm = key of epoch e, salt = none, info = "arc-shred-v1")`.
  The key of an epoch opens no earlier epoch: HKDF has no inverse.
- The first key is random. The signer makes it, and steps it forward each
  epoch as a device does. It never wraps an epoch key to the citizen key.
  The seed therefore opens no shred epoch.
- A device asks the signer for the current epoch key once, when it joins
  the list, in a live call. The device makes a one-time key for that call.
  The signer seals the epoch key to the one-time key, and the device
  deletes the one-time key once it has opened the reply. No event keeps a
  wrap. A wrap to the device key would break the epochs: a thief who later
  steals the device key, with a recorded copy of the wrap, would get the key
  of that epoch, and step it forward to each later one.
- A device keeps only the key of the oldest epoch that it still needs, and
  steps it forward. It derives each later key when it needs it. When an
  epoch ends, the device steps its key past it, and overwrites the old key.

A secret with `expiration` T:

- The device seals the value with `HKDF-SHA256(ikm = key of epoch(T), info = "arc-shred-secret-v1" || 0x00 || d)`,
  and then seals the record with the data root, as each secret is sealed
  (section 2.1). Two layers: the data root says who reads, the epoch says
  until when.
- After T, each device that follows the rule holds no key of epoch(T). The
  sealed value on each storage citizen, in each backup and in each old copy
  of the store is then noise to anyone, with the device key, with the data
  roots, and with the seed.

What a shred epoch gives:

| Thief or holder | Before T | After T, once each device stepped past it |
| --- | --- | --- |
| Steals a device later | Reads the secret | Cannot read it |
| Holds a copy of a blob from a storage citizen | Needs a device | Cannot read it |
| Holds the seed | Cannot read it: no wrap to the citizen key | Cannot read it |
| A device that is hostile now | Reads the secret, and can keep the old key | Keeps what it kept |
| A device that was offline past T | Still holds the old key | Deletes it at its next start |

What a shred epoch costs:

- **The seed does not recover such secrets.** A citizen who loses every
  device loses each secret that has not expired. These are short-lived
  credentials, so the citizen makes new ones at their providers.
- **A backup of a device holds its epoch key.** Time Machine copies files.
  The epoch key must live where no backup reads it: the macOS Keychain with
  `kSecAttrAccessibleWhenUnlockedThisDeviceOnly`, which a backup restores
  to no other machine, or a file that the backup excludes. On a machine
  with neither, a shred epoch gives nothing, and `arc` warns.
- **The clock decides.** A device with a clock far behind keeps old keys
  too long. A device with a clock far ahead drops keys early, and loses
  secrets that have not expired. A device should warn of a clock more
  than 10 minutes off, the limit that a reader of a token already uses
  (`docs/devices/SPEC.md:256`).
- **A new device gets only the current epoch.** It reads no secret of an
  epoch that has ended, which is correct.
- **An agent** gets values from the keeper, not keys. The keeper holds the
  epoch key as a device does. An agent's cache ends at its `cache` time or
  at T, whichever comes first.

A leased secret (section 2.6) is stronger: one holder, and the keeper
deletes the key at T exactly. A shred epoch keeps offline reads and many
holders, and accepts that each holder must follow the rule.

### 2.8 What each party sees

| Party | Sees | Does not see |
| --- | --- | --- |
| Relay | That an agent key sent a call to the keeper, its time and size. The same as for any call. | Vaults, references, values |
| Storage citizen | The count of blobs, padded sizes, times, generations. The same as for the journal (`docs/proposals/storage.md`, section 2.8). It cannot tell a secret from a journal page. | References, vaults, values |
| Keeper | Each secret, each grant, each call of each agent | — |
| Device of the citizen | Each secret | — |
| Agent | The secrets of the vaults in its grant, until the end of its cache | Other vaults, all other self data |

### 2.9 What a hostile party can do

| Party | Attack | Result | Defense |
| --- | --- | --- | --- |
| Storage citizen | Read, change or make a blob | It cannot read. A device refuses a changed or made blob. | As for the journal: AEAD, and the signature against the device list. |
| Storage citizen | Withhold a new value | A device gives an older value. | Two storage citizens. The old credential fails at its provider, which shows the problem. |
| Relay | Withhold a call to the keeper | The agent uses its cache, then fails. | Another relay of the keeper. |
| Agent | Read another vault | The keeper refuses. | The grant. |
| Agent | Write a bad value to its vault | Each reader of that vault gets the bad value. | A write scope per vault. The store keeps a checkpoint of each revision of a draft (`docs/KINDS.md:26`), so the citizen can go back. |
| Thief with an agent key | Read the vaults of its grant | Until the grant is removed, and then until the cache ends. | Short grants, short caches, and rotation at the providers. |
| Thief with a device | Read every secret that the device synced | Out for good (section 0). | Revoke the device. Rotate each credential. |
| Thief with the keeper | Read every secret, and each leased secret | Out for good. | The keeper is a machine that the citizen trusts most, as the signer is. |

### 2.10 Prior art and alternatives

| System | What it shows for this design |
| --- | --- |
| 1Password service accounts | What `op-agent` uses today. A read-only token for each vault. The provider holds the data and can read the metadata. This proposal keeps the reference format and the per-vault scope. |
| HashiCorp Vault, OpenBao | Leases and dynamic secrets: the server makes a credential for each request, and revokes it at the end of the lease. This is the real self-destruct of section 0. The keeper can later make such credentials for providers that allow it. |
| sops with age | Secrets encrypted in a git repository. Offline and with no server, but each reader holds the whole key, and history keeps each old value. |
| Keybase KBFS | A key for each device, and no key of new data for a revoked device. ARC's data roots follow the same rule. |
| Lit Protocol | Access rules on a chain and a key split across a network of nodes. It shows threshold release. Old ciphertext stays public. |
| A contract on Base | Considered and refused. A chain is append-only, so a value and each old value stay in its history for good. A contract can limit writes, not reads. The wallet, the times and the sizes are public. |

## 3. Relation to private environments

[The private-environment proposal](private-environment.md) has a
controller that holds key-release authority, and gives secrets only to a
protected guest that it verified (`docs/proposals/private-environment.md:32`).
The keeper is a simpler role: it checks a grant, not hardware evidence. A
controller can call the keeper as an agent with a grant, and pass a secret
to a guest that it verified. The secrets app then is where the controller
reads. Neither proposal replaces the other.

## 4. App on sdk/, or changes below it

| Layer | Change | Why |
| --- | --- | --- |
| `apps/secrets` | New. A manifest data app, as the files app is: `put`, `get`, `list`, `delete`, with `stdin` for the value (`docs/interface/SPEC.md:212`). | Devices read and write secrets as sealed records. It needs no spec (`docs/SPEC-TEMPLATE.md`, section 1). |
| `apps/secrets/cmd/arc-secrets` | New. The keeper: grants, the calls of section 2.4, and later leased secrets. | A service of one citizen for its agents. It uses only `sdk/`. |
| `arc-env` | New. A small program that reads a refs file, gets each value from `arc secrets get --json` or from a call to the keeper, and runs a command with the values in its environment. It drops its own settings from that environment, as `op-agent` does. | No manifest action puts values in the environment of a child process. |
| `core`, `runtime`, `adapters` | No change for PR 1 to 3. | Sealing, sync and calls exist. |
| `core/devices`, `runtime`, `cmd/arc` | Shred epochs, PR 4: the epoch key on the signer and each device, the call that gives it with a one-time key, the step each epoch, and the second layer of section 2.7. | The signer and the root cache are in core and runtime. This change needs an amendment to [the devices spec](../devices/SPEC.md). |

Reader-side expiry of section 2.6 is the one open point. A manifest can
write an `expiration` tag. No reader of a manifest command refuses an event
by it today. If the refusal belongs in `runtime`, it needs a spec.

## 5. Recommendation

Build secrets as a data app first, then the keeper.

1. **PR 1, the smallest first step.** `apps/secrets/manifest.json` and
   `arc-env`. Devices only: no agents, no grants, no expiry. Each device
   reads each secret.
2. **PR 2.** The keeper, agent keys and grants. `arc-env` calls the keeper
   when it runs with an agent key.
3. **PR 3.** Expiry on readers, and leased secrets on the keeper.
4. **PR 4.** Shred epochs (section 2.7), with the amendment to the devices
   spec.

The smallest step that Gezim can try after PR 1:

1. On the laptop: `arc install` the secrets app, then
   `printf %s "$VALUE" | arc secrets put Agents/Cloudflare/R2/SecretAccessKey`.
2. Run `arc sync`. On a second device, run `arc sync`, then
   `arc secrets get Agents/Cloudflare/R2/SecretAccessKey`. The value appears.
3. Write `.env.arc` with `R2_SECRET=arc://Agents/Cloudflare/R2/SecretAccessKey`.
   Run `arc-env --env-file .env.arc -- sh -c 'test -n "$R2_SECRET" && echo ok'`.
4. Stop the relay and the storage citizen. Run step 3 again. It still prints
   `ok`: the value is in the local store.
5. Measure the time of step 3 against `op-agent` on the same command.

PR 1 does not separate agents. An agent that runs `arc-env` on the laptop
reads every secret, as a 1Password service account reads its whole vault.
PR 2 closes that.

## 6. Decisions

Nothing here is decided. Each item gives the recommended answer.

1. **Agents are not devices.** Recommended: yes. Reason: section 2.3.
2. **The keeper runs on its own device, not on the signer.** Recommended:
   yes, with the signer's machine allowed for the first try. Reason: the
   signer holds the citizen key, and the keeper holds plaintext. One stolen
   machine then gives one of them.
3. **A reference is `arc://<vault>/<item>/[<section>/]<field>`.**
   Recommended: yes. Reason: it moves each `op://` reference by a change of
   scheme, and keeps Gezim's naming rules.
4. **A secret is kind 30078 in a draft, capability `secrets`.** Recommended:
   yes. Reason: no new kind, and storage citizens cannot tell it from other
   app data.
5. **Grants live on the keeper, not in self data.** Recommended: yes for
   PR 2. Reason: a device could otherwise change a grant. A later version
   can have the signer sign each grant.
6. **The agent cache lasts 1 hour by default and 24 hours at most.**
   Recommended: yes. Reason: section 2.5. The numbers are proposed, not
   measured.
7. **Leased secrets are built.** Recommended: only after a credential at a
   provider needs it. Reason: a short credential at its provider gives most
   of the gain, with no keeper on each read.
8. **Shred epochs are built, and a secret with `expiration` uses them.**
   Recommended: yes, as PR 4, with an epoch of one UTC day. Reason: they
   are the only way that an expired secret becomes unreadable on storage
   citizens and backups, and they keep offline reads. The day is proposed:
   a shorter epoch shreds sooner, and costs nothing but more steps.
9. **An import from 1Password.** Recommended: a later command that reads a
   vault through `op` and puts each field. Reason: Gezim's vaults already
   follow the reference rules of decision 3.
