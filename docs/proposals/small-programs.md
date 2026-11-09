Status: proposed

# Small programs and small files

This proposal moves ARC towards the Unix rule: each program does one job,
and each file holds one concern. No code implements it.

Each claim about ARC names a file and a line, at commit `9a07387`. Each
number says "measured" or "estimated". Line counts exclude `_test.go` files
and cover `core`, `sdk`, `adapters`, `runtime`, `apps`, `cmd` and `internal`.

## 0. Summary

- ARC has two different problems. Large files are a layout problem. One
  program for every role is a process problem. Each problem gets its own fix.
- A check in `go test ./internal/architecture` refuses a source file over 600
  lines. A ratchet list holds the files that are larger today. A listed file
  can shrink. It cannot grow, and the list never gets a new entry.
- Each large file splits by concern, in its own package, with no change in
  behavior. One pull request for each file.
- A role gets its own program only when it runs on a different machine,
  inside a different trust boundary, or with a different lifetime.
- By that test, two roles leave `arc`. The relay becomes `arc-relay`. The
  signer becomes `arc-signer`, and `arc` never holds the citizen secret
  again. App hosting stays in `arc`.
- `arc` talks to its signer only through signer requests. A signer can be
  a local program, a daemon behind a relay, or later a hardware device.
- The layer rules of [the architecture map](../ARCHITECTURE.md) do not change.

## 1. The problem, measured

### 1.1 Large files

There are 233 source files (measured). Of these, 18 are over 600 lines, and
12 are over 800 lines (measured).

| File | Lines (measured) | Concerns in the file |
| --- | --- | --- |
| `core/mail/mail.go` | 1,596 | Send, reply and forward; receive, requests and answers; carry, expiry and eviction; inbox and outbox views. |
| `cmd/arc/main.go` | 1,462 | Home and root lookup (line 83 to 325); relay config files (line 326); relay commands (line 392); TURN credentials (line 525); the relay server (line 680 to 987); `message` (line 988); `sync` (line 1176). |
| `runtime/iface/data.go` | 1,437 | Publish, query, watch and removal of data entries, for both sealed and plain entries. |
| `cmd/arc/keys.go` | 1,251 | Identity files (line 48); passphrase prompts (line 170); `keys` commands (line 440); the bunker signer (line 817); the lazy and timed signer wrappers. |
| `adapters/transport/relay/relay.go` | 1,242 | Kept connections, publish and auth, fetch, negentropy reconciliation, watch. |
| `adapters/relay/rings/rings.go` | 1,104 | The ring server. |
| `runtime/selfsync/network.go` | 970 | Sync over the network. |
| `apps/dm/cmd/arc-dm/main.go` | 965 | The dm client. |
| `runtime/iface/entries.go` | 963 | Data entries. |
| `cmd/arc/capability.go` | 940 | App commands. |
| `apps/storage/server/server.go` | 862 | The storage server. |
| `runtime/citizen/selfsync.go` | 816 | The citizen side of self sync. |

The other six files over 600 lines are `runtime/iface/manifest.go` (777),
`runtime/selfsync/repair.go` (770), `apps/dm/cmd/arc-dm/conversations.go`
(744), `apps/storage/cmd/arc-storage/main.go` (680),
`runtime/iface/notebook.go` (637) and `sdk/storage/client.go` (623), all
measured.

A large file is hard to review, and it collects unrelated changes. Two
sessions that change unrelated behavior in `cmd/arc/main.go` get a merge
conflict.

### 1.2 One program for every role

README.md line 19 says "`arc` is the one program". AGENTS.md line 29 and
docs/ARCHITECTURE.md line 41 say the same for `cmd/arc`. The v0.11.0
switchover chose this on purpose. This proposal reverses that choice for
the relay.

The `arc` program keeps identities, sends and reads mail, syncs, hosts
apps, updates itself, and runs a relay. The relay is a different role:

- It runs on other machines: the Fly relay and aurelia.
- It holds no citizen key.
- It is a server that runs for months. The citizen commands run and exit.
- The Dockerfile starts it with `arc relay serve` (Dockerfile line 39).

Every release of `arc` therefore ships the relay to each citizen, and the
relay server code (308 lines, measured) lives in the citizen's CLI file.

`cmd/arc-ble-probe` is already a second program in `cmd/`.

### 1.3 The citizen secret inside `arc`

The key file of an identity holds a secret key as hex or nsec, a NIP-49
ncryptsec, or a NIP-46 bunker URI (cmd/arc/keys.go line 46 to 72). In the
first two cases, `arc` reads the citizen secret into its own memory for each
command.

The runtime then uses the raw secret for more than signatures:

- The keyed root is an HKDF of the secret (runtime/iface/run.go line 85,
  used at runtime/citizen/identity.go lines 31 and 51).
- The account key for each storage citizen is an HKDF of the secret
  (runtime/selfsync/account.go line 18, used at
  runtime/citizen/selfsync.go lines 98, 112 and 163, and
  runtime/citizen/storagejoin.go lines 29 and 73).
- The join to a storage citizen is signed with the raw secret
  (runtime/citizen/storagejoin.go line 35).

A machine that uses a bunker already works without the secret for most of
this. It reads the keyed root from a draft that the signer publishes, and it
calls storage citizens with its device key (runtime/citizen/selfsync.go
line 95 to 104). It cannot derive an account key or join a storage citizen.

The signer code is 434 lines of cmd/arc/keys.go (line 817 to 1251) and 97
lines of cmd/arc/devices.go (line 364 to 460), measured. It shares two files
with the commands of the citizen.

## 2. Terms

- **Source file**: a `.go` file that is not a `_test.go` file.
- **Size limit**: 600 lines for a source file.
- **Ratchet list**: the files over the size limit when the check lands, each
  with its line count at that time as its ceiling.
- **Role**: a job that a party does in ARC, such as citizen, relay or
  signer.

## 3. The file rule

### 3.1 The check

`go test ./internal/architecture` gets a new test. It walks the same layers
as `TestPackageBoundaries` (internal/architecture/architecture_test.go line
21), plus `internal`. It refuses:

- a source file over 600 lines that is not on the ratchet list;
- a listed file over its ceiling;
- a listed file at or under 600 lines, because its entry must go.

The ratchet list is a map in the test, from path to ceiling. It follows the
rule of [the grandfathered list](../GRANDFATHERED.md): it is debt, and it
never gets a new entry. A pull request that shrinks a listed file lowers its
ceiling in the same commit.

The check runs in `go test ./...` and in CI, the same as `mise run
boundaries`.

### 3.2 Why 600 lines

At 600 lines, 18 files are over (measured). At 500 lines, 25 are over
(measured). A limit of 600 gives a ratchet list that a person can read, and
it still catches each file in section 1.1. The limit can go down after the
list is empty.

### 3.3 How a file splits

A split moves code. It does not change behavior.

- Each file holds one concern. The file name says the concern.
- A split stays in its package, unless the concern is already a separate
  package in all but name. Then it becomes a subpackage.
- Each split is one pull request, with the type `refactor`.
- A split under `core/` or `sdk/` changes no normative section of a spec.
  `mise run specs` stays green.

## 4. The file splits

The splits in this table are estimated from the function list of each file.
The pull request for each file confirms its split.

| File | Split into |
| --- | --- |
| `cmd/arc/main.go` | `home.go` (home and root lookup), `relayconfig.go` (relay and indexer files), `relay.go` (relay commands), `turn.go` (TURN credentials), `message.go` (the existing file takes `messageCommand`), `sync.go`. The relay server moves out in section 6. |
| `core/mail/mail.go` | `send.go` (send, reply, rumor, forward, queue), `receive.go` (sync, watch, open, requests, answers), `carry.go` (carry, expiry, eviction), `views.go` (inbox, outbox, rumors). |
| `runtime/iface/data.go` and `entries.go` | `publish.go`, `query.go`, `watch.go`, `remove.go`. Then `manifest`, `notebook` and `pipeline` become subpackages of `runtime/iface`. |
| `cmd/arc/keys.go` | `identity.go` (identity files), `prompt.go` (passphrase and seed prompts), `keys.go` (commands), `bunker.go` (the signer and its wrappers). The signer files move out in section 7. |
| `adapters/transport/relay/relay.go` | `conn.go` (kept connections), `publish.go` (publish and auth), `fetch.go`, `reconcile.go` (negentropy), `watch.go`. |
| Other files on the list | Split by concern when the pull request reads them. |

These pull requests are independent. They can run at the same time, one
writer for each file.

## 5. The program rule

A role gets its own program when at least one of these is true:

1. It runs on a different machine from the citizen.
2. It has a different trust boundary: it holds a key that the citizen
   commands must not hold, or it holds no citizen key at all.
3. It has a different lifetime: it is a server that runs for months, and
   the commands of the citizen run and exit.

A program for each command is not the goal. A role that runs on the
citizen's machine, with the citizen's key, for the length of a command,
stays in `arc`.

Each program follows the layer rules. It is a directory `cmd/arc-<role>`.
It imports no app. `mise run build` already builds `./cmd/...` (mise.toml
line 11).

| Role | Decision | Reason |
| --- | --- | --- |
| Relay (`arc relay serve`, STUN, TURN, the ring server) | `arc-relay` | Rules 1, 2 and 3. |
| Signer (`arc keys bunker`, and each use of the citizen secret) | `arc-signer` | Rule 2, and rule 1 for a signer on another machine or on hardware. It is the one process that holds the key, as `ssh-agent` does. |
| App hosting (`serve`, `session`, `call`, `install`) | Stays in `arc` | It runs on the citizen's machine with the citizen's key. |
| `runtime/iface` | Stays a package | It interprets manifest apps for `arc`. The file splits in section 4 fix its size. |
| `arc update`, `arc release` | Stay in `arc` | They act on the citizen's own install and keys. |

## 6. `arc-relay`

### 6.1 What moves

- `newRelay`, `serveCommand`, `ringSetting`, `relayHandler`, `turnSetting`,
  `stunListen` and `writePolicy` (cmd/arc/main.go line 680 to 987) move to
  `cmd/arc-relay`.
- `arc-relay` uses `adapters/relay/*` and khatru directly. It should import
  no `runtime/` package. The pull request checks this, and a boundary rule
  in `internal/architecture` keeps it.
- `arc relay add`, `rm`, `ls`, `ring` and `turn` stay in `arc`. They are the
  citizen's side of a relay.

### 6.2 What changes outside the code

- The Dockerfile starts `arc-relay` (Dockerfile line 39).
- The release tarballs and `install.sh` ship `arc-relay`.
- The Fly relay and aurelia move to `arc-relay` in the release deploy. The
  Mac gets the release as in each deploy.
- README.md, AGENTS.md and docs/ARCHITECTURE.md stop calling `arc` the one
  program. They state the program rule of section 5.

### 6.3 Compatibility

`arc relay serve` goes away. This is a breaking change, with a
`BREAKING CHANGE` footer. The footer gives the one-line migration: run
`arc-relay` with the same flags. `arc` does not call `arc-relay` on behalf
of the old command.

No wire format changes. A citizen on an older `arc` talks to `arc-relay`
as it talked to `arc relay serve`. The relay keeps its data directory and
its on-disk layout.

### 6.4 Approval

This changes the shape of the product and the deploy. It needs Otto's OK, a
release, and a deploy to the Fly relay, aurelia and the Mac.

## 7. `arc-signer`

### 7.1 The rule

`arc` MUST NOT read, hold or derive from the citizen secret. Each operation
that needs the secret is a signer request. A signer answers it. The signer
is a separate program, a daemon behind a relay, or a hardware device.

This is the shape that a hardware signer needs. `arc` already treats a
bunker as one kind of signer (section 1.3). This proposal makes every
signer that kind.

### 7.2 Signer requests

A signer answers the requests of NIP-46: `get_public_key`, `sign_event`,
`nip44_encrypt` and `nip44_decrypt`. ARC adds the requests that today read
the raw secret in `runtime/citizen` (section 1.3):

| Request | Answer | Replaces |
| --- | --- | --- |
| `arc_keyed_root` | The keyed root, under the policy of the signer | `iface.KeyedRoot(sess.Key.Secret)` |
| `arc_account_key` with the key of a storage citizen | The account key for that storage citizen | `selfsync.AccountKey(sess.Key.Secret, storage)` |
| `arc_sign_join` with the key of a storage citizen | The signed join | `storage.SignJoin(sess.Key.Secret, ...)` |

The exact names and the wire format belong to a spec. The devices spec
(docs/devices/SPEC.md) is the likely owner, because it already defines the
bunker. A change to that spec follows section 3 of
[how to write a spec](../SPEC-TEMPLATE.md).

The signer keeps its policy: the kinds it signs, what it decrypts, and the
device list (cmd/arc/keys.go policy type, cmd/arc/devices.go line 364).

### 7.3 How `arc` reaches a signer

The key file of an identity names its signer. It holds no secret. There are
two forms:

1. **A program.** `arc` starts the signer program for the command and
   speaks the signer requests over its standard input and output, one JSON
   line each. This is the shape of a git credential helper. The default
   program is `arc-signer`. A hardware signer comes with a small program of
   its own that speaks the same lines and talks to the device over USB or
   Bluetooth. `arc` needs no change for a new kind of hardware.
2. **A bunker URI.** `arc` sends the same requests through a relay, as
   NIP-46 does today (cmd/arc/keys.go line 80 to 167).

`arc` keeps its lazy connection (cmd/arc/keys.go line 99 to 106). A command
that signs nothing starts no signer.

A program signer asks for its passphrase on the terminal itself, through
`/dev/tty`, as `pinentry` does for gpg. `arc` never sees the passphrase.

### 7.4 What moves

| Today in `arc` | Goes to |
| --- | --- |
| `arc keys gen`, `recover`, `encrypt`, and the seed and passphrase code (cmd/arc/keys.go line 444 to 815) | `arc-signer gen`, `recover`, `encrypt` |
| `arc keys bunker` (cmd/arc/keys.go line 817 to 1251) | `arc-signer serve --relay <url>` |
| Signing the device list (`arc keys devices add`, `revoke`) | `arc-signer devices` |
| The guard and the device watch (cmd/arc/devices.go line 364 to 460) | `arc-signer` |
| Reading a secret, an nsec or an ncryptsec from the key file (cmd/arc/keys.go line 54 to 72) | `arc-signer` only |

`arc keys add`, `list`, `use`, `remove`, `whoami`, `device`, `roots`,
`seal`, `unseal` and `storage` stay in `arc`. They keep using the signer
through requests.

`arc-signer` must not import `runtime/`. It needs `core/keys`,
`core/devices`, the key file adapter and the relay adapter. A boundary rule
in `internal/architecture` keeps it small. A hardware signer cannot run
ARC's runtime, so the software signer must not need it either.

### 7.5 Compatibility

This is a breaking change, with a `BREAKING CHANGE` footer.

- A key file that holds a secret stops working in `arc`. The footer gives
  the migration: `arc-signer import` moves the secret into the store of the
  signer, and writes the program form of section 7.3 into the key file.
- A key file that holds a bunker URI keeps working with no change.
- An older bunker does not answer the new requests of section 7.2. A
  machine that uses it cannot derive an account key or join a storage
  citizen. That is true today too (section 1.3).
- No event and no kind changes. Mail, drafts and the keyed root on the
  relays stay the same. A citizen keeps its public key.

### 7.6 Hardware

This proposal does not build a hardware signer. It makes one possible with
no change to `arc`. A hardware signer must:

- sign events and do NIP-44 with the citizen key;
- derive the keyed root and the account keys (section 7.2), or hold them
  from setup;
- keep its policy, or leave the policy to its small program.

The two HKDF derivations of section 1.3 are the hard part. No standard
Nostr signer does them. A hardware signer needs firmware for them, or
`arc-signer` derives the values once at setup and the hardware keeps them.
Section 9 lists this question.

### 7.7 Approval

This changes how each citizen holds their key. It needs Otto's OK, a
release, and a migration on each machine that holds a key file with a
secret: the Mac, the storage citizens and the Fly relay if it holds one.

## 8. Order and cost

| Step | Pull requests | Approval | Cost (estimated) |
| --- | --- | --- | --- |
| 1. The size check and the ratchet list | 1 | Normal review | Small |
| 2. The file splits | About 18, one for each listed file | Normal review | Mechanical; they can run at the same time |
| 3. `arc-relay` | 1, breaking | Otto, then a release and a deploy | Medium |
| 4. The signer requests of section 7.2, in the devices spec | 1 | Otto | Small |
| 5. `arc-signer`, and `arc` without the secret | 2 or 3, the last one breaking | Otto, then a release and a migration on each machine | Large |

Step 1 lands first, so no file grows while the splits run. Step 3 can start
after the split of `cmd/arc/main.go`. Step 5 can start after the split of
`cmd/arc/keys.go` and step 4. Steps 3 and 5 are independent, and they can
ship in one release with one deploy.

## 9. Open questions

- Do `_test.go` files get a limit too? `adapters/relay/rings/rings_test.go`
  is 1,140 lines and `cmd/arc/home_test.go` is 1,016 lines (measured).
- Does `cmd/arc-ble-probe` stay in this repository, or move with the
  Bluetooth work?
- Does `arc-relay` need a citizen key of its own, for example for relay
  announcements? If so, it asks `arc-signer`, as `arc` does.
- How does a hardware signer get the keyed root and the account keys:
  firmware that does the HKDF, or values that `arc-signer` derives once at
  setup? See section 7.6.
- Where does `arc-signer` keep the secret on a machine: a file under its own
  home, the macOS Keychain, or both?

## 10. Sources

At commit `9a07387`:

- README.md line 19: "`arc` is the one program".
- AGENTS.md line 29 and docs/ARCHITECTURE.md line 41: "`cmd/arc` is the arc
  program".
- Dockerfile line 39: the relay starts with `arc relay serve`.
- mise.toml line 11: `mise run build` builds `./cmd/...` and `./apps/...`.
- internal/architecture/architecture_test.go line 21: the layers that
  `TestPackageBoundaries` walks.
- cmd/arc/main.go lines 83, 313, 326, 392, 525, 680, 710, 836, 864, 880,
  921, 934, 988 and 1176: the functions named in section 1.1 and 6.1.
- cmd/arc/keys.go lines 46 to 72 (the forms of the key file), 80 to 167
  (the bunker client), 170, 440, 444 to 815, and 817 to 1251 (the bunker).
- cmd/arc/devices.go line 364 to 460: the guard and the device watch.
- runtime/iface/run.go line 85 and runtime/selfsync/account.go line 18: the
  two HKDF derivations from the secret.
- runtime/citizen/identity.go lines 31 and 51, runtime/citizen/selfsync.go
  lines 95 to 112 and 163, runtime/citizen/storagejoin.go lines 29, 35 and
  73: each use of the raw secret in the runtime.
- The line counts come from this command, run at the repository root:

```bash
find core sdk adapters runtime apps cmd internal -name '*.go' ! -name '*_test.go' | xargs wc -l | awk '$2!="total" && $1>600' | sort -rn
```
