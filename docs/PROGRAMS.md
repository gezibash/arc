# Small programs and small files

ARC follows the Unix rule: each program does one job, and each file holds
one concern. This document gives the two rules, the three programs, and how
the citizen key stays out of `arc`. [AGENTS.md](../AGENTS.md) states the
rules for contributors; [the devices spec](devices/SPEC.md), sections 3.7
and 3.8, defines the signer protocol.

## 1. The file rule

- No source file holds more than 600 lines. A `_test.go` file is not a
  source file.
- A larger file splits by concern. The file name says the concern. A split
  moves code and changes no behavior.
- `TestFileSize` in `internal/architecture` checks each file under `core`,
  `sdk`, `adapters`, `runtime`, `apps`, `cmd` and `internal`. It runs in
  `go test ./...` and in CI.

The largest source file is 586 lines (measured), of 300 source files. Before
the rule, 18 files were over 600 lines, and the largest was 1,596 lines
(measured at commit `9a07387`).

## 2. The program rule

A role gets its own program, `cmd/arc-<role>`, only when one of these is
true:

1. It runs on another machine than the citizen.
2. It holds a different key, or no citizen key.
3. It serves for months, while the commands of a citizen run and exit.

A role that runs on the citizen's machine, with the citizen's key, for the
length of a command, stays in `arc`. So app hosting (`arc serve`,
`session`, `call`, `install`) and the bunker (`arc keys bunker`) stay in
`arc`: they need the citizen's store and runtime.

`TestPackageBoundaries` in `internal/architecture` keeps `cmd/arc-relay` and
`cmd/arc-signer` free of `runtime/`.

## 3. The programs

| Program | Job | Holds |
| --- | --- | --- |
| `arc` | The citizen: identities, mail, sync, apps, updates | No secret key. The device key of the machine, and the public key of each identity. |
| `arc-relay` | A relay: khatru, sealed data, limits, STUN, TURN, rings | No citizen key. Its store, `relay.db`. |
| `arc-signer` | The program signer of one identity | The citizen key, in `signer/key` in the folder of the identity. |

Each release tarball holds the three programs. `install.sh` links each one
into `ARC_BIN_DIR`. The Docker image runs `arc-relay`.

### 3.1 `arc-relay`

`adapters/relay/relayserver` composes the relay. `cmd/arc-relay` reads the
flags that `arc relay serve` had, and runs it. With no `--db`, the store is
`relay.db` in `ARC_HOME`, or in `~/.arc`, where `arc relay serve` kept it.
No event or wire format changed.

### 3.2 `arc-signer`

`arc` never reads, holds or derives from the citizen secret.

- The key file of an identity holds `program:arc-signer`, or a `bunker://`
  URI. It holds no secret.
- `arc` starts `arc-signer stdio` in the folder of the identity at the
  first request of a command, and sends one JSON line for each request. It
  finds the program beside itself, then on the PATH. A command that signs
  nothing starts no signer.
- The requests are those of NIP-46, and five holder requests that need the
  secret: the keyed root, the account key for a storage citizen, and the
  signatures of a storage join, a release channel and a self-data change.
  The signer computes each digest from the params.
- `arc keys gen`, `recover`, `add` and `encrypt` run `arc-signer gen`,
  `recover`, `import` and `encrypt`. arc-signer shows the seed and asks for
  each passphrase; arc reads only the public key.
- `arc keys bunker` answers devices over a relay. It sends each request
  that it admits to the program signer, and refuses each holder request: an
  account key or a release signature would give a device the powers of the
  owner.

A hardware signer comes with its own program that speaks the same lines.
`arc` needs no change for it. The key file names that program.

A program signer is a boundary of processes, not of users: a program of the
same user can read `signer/key`.

### 3.3 Migration

A key file of an older `arc` holds the secret. At its first use, the new
`arc` runs `arc-signer import`, which moves the secret to `signer/key`, and
then writes `program:arc-signer` to the key file. The public key stays, so
mail, drafts and lists on the relays stay the same. An older `arc` cannot
read the new key file.

## 4. Proof

- `go test ./internal/architecture`: the file rule and the program
  boundaries.
- `go test ./core/keys`: each request through a signer gives what the key
  gives, a signer that signs with another key is refused, and bad requests
  get errors.
- `go test ./adapters/signer/program`: arc-signer makes, imports and seals
  a key, and answers over its lines; a wrong passphrase does not open it.
- `go test ./internal/proof`: the proofs run every relay through
  `arc-relay`, every key through `arc-signer`, and the homes of older `arc`
  versions through the migration.

## 5. Unverified

- No hardware signer exists, so no test runs one.
- No test measures the time of a request to a program signer.
- The Fly relay and aurelia still run `arc relay serve` of v0.37.0. Their
  move to `arc-relay` is part of the next release deploy.
