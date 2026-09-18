# Managed relay updates

This document covers two update paths that share the signed channel format
and the release provider:

- [Updating a local installation](#updating-a-local-installation) replaces
  the complete release tree an `arc` binary runs from, fetched through the
  configured relay. This is what `arc update` does without `--socket`.
- The managed relay hot updater, below it, applies an authored OTP upgrade
  package to one running relay service through a private local socket.

The hot path is the initial relay-only implementation. Existing released
installations must first move to a prepared, stopped native base. Do not run
the replacement installer against a live release tree. This work does not
update external providers, the local host service, container images, or
Erlang itself.

## Updating a local installation

```sh
arc update --publisher PUBLISHER_PUBLIC_KEY_HEX   # trust and remember the publisher
arc update                                        # search the relay, verify, install
arc update check                                  # report availability only
arc update status                                 # local state; no network
```

The command runs from the release tree the installer script unpacks
(`RELEASE_ROOT`, normally `~/.local/share/arc`). It refuses to run from a
source checkout. Each run:

1. Resolves the relay saved by `arc join` (or `--relay`/`--relay-pubkey`), and
   connects with a fresh ephemeral identity, so no citizen key is needed and
   the user's own announcements are untouched.
2. Searches the relay directory for capabilities with the `releases` scheme,
   unless `--source releases+arc://PROVIDER_KEY/releases` names one provider.
   Up to five providers are tried in discovery order; the first channel
   document that verifies wins. Hosting a channel grants no authority: only
   the publisher signature is trusted.
3. Fetches the selected channel (`stable` by default, `--channel beta` for
   prereleases) and verifies it with `Arc.CLI.Update.Manifest`: pinned
   publisher, channel, signature, expiry, and a monotonic sequence recorded
   per channel in the checkpoint file.
4. Selects the newest eligible release for this OS and CPU that carries an
   `install` object. The channel's latest release is reported separately, so
   an installation is never called up to date when the newest release ships
   without a complete archive. Lower versions are never installed.
5. With `apply` (the default), streams the archive through the relay in
   bounded chunks, checks its exact size and SHA-256, and validates the tar
   members: every member is a regular file or directory under `arc/`, and
   `arc/releases/<version>/` exists.
6. Extracts beside the installation, runs the candidate's `bin/arc version`
   with the running release's `RELEASE_*` variables cleared, and requires it
   to report the expected version. Only then are the two directories swapped
   with renames: the old tree becomes `<root>.previous`, and the candidate
   becomes the root. The previous release is removed by the next successful
   update, never by a failed one.

Local state lives in a mode-0700 directory, `~/.config/arc/update/` by default:

| File              | Contents                                                     |
| ----------------- | ------------------------------------------------------------ |
| `settings.json`   | Trusted publisher and selected channel                       |
| `checkpoint.json` | Highest accepted sequence and digest per channel             |
| `journal.json`    | Last update phase (`staging`, `installing`, `current`, `failed`) |
| `staging/`        | The archive while it is downloaded and validated             |

A different `--publisher` is refused until `--replace-publisher` is passed,
which also restarts the checkpoint. A journal left in `installing` by a crash
blocks further updates until the operator has checked the root and
`<root>.previous` and removed the journal. Exit status is 1 when the update is
blocked or fails; `--format json` prints the same report as a document.

The running process keeps the code it already loaded; nothing is hot-loaded.
Running services started from the old tree keep running until restarted.

### Publishing a complete archive

Release archives are the tarballs the release workflow builds:
`arc-<version>-<os>-<arch>.tar.gz`, with every member under `arc/`. To offer
one through the [release provider](../../providers/releases/README.md), store
it as `blobs/<sha256>.tar.gz` and add an `install` object to the platform's
release entry in the signed channel document:

```json
{
  "version": "0.5.0",
  "build": "BUILD_ID",
  "runtime": "16.3.1",
  "platform": {"os": "linux", "arch": "x86_64"},
  "size": 1,
  "sha256": "HOT_PACKAGE_SHA256",
  "sources": [{"build": "PREVIOUS_BUILD", "runtime": "16.3.1",
               "upgrade_plan_sha256": "RELUP_SHA256", "downgrade_plan_sha256": "RELUP_SHA256"}],
  "restart_required": true,
  "withdrawn": false,
  "eligible": true,
  "install": {"sha256": "TARBALL_SHA256", "size": 41234567}
}
```

`install` is optional and separate from the hot package fields, which keep
their meaning for managed relays. A release without `install` is still
reported as the channel's latest but cannot be installed by `arc update`.
Verifiers from before `install` existed reject a document that carries it.

## Operator controls

## Managed relay operator controls

```sh
arc service start --config /absolute/path/service.json
arc update status --socket /absolute/path/update-state/admin.sock
arc update check --socket /absolute/path/update-state/admin.sock
arc update apply --socket /absolute/path/update-state/admin.sock
```

`check` and `apply` return immediately with an operation state. Use `status` to
observe completion. Checks run shortly after startup and hourly thereafter.
Availability is recorded in status and logged. Checks fetch metadata only;
every archive download and installation needs an explicit `apply` request.
There is no automatic restart or automatic downgrade command.

Example configuration (replace all placeholders with operator-selected values):

```json
{
  "role": "relay",
  "key": "stored-relay-key-name",
  "port": 7331,
  "peers": [],
  "transit": false,
  "update": {
    "state_dir": "/absolute/path/update-state",
    "publisher": "PINNED_RELEASE_PUBLISHER_PUBLIC_KEY_HEX",
    "channel": "stable",
    "pin": null,
    "source": {
      "uri": "releases+arc://PROVIDER_PUBLIC_KEY_HEX/releases",
      "relay": "relay.example:7331",
      "relay_pubkey": "PINNED_RELAY_PUBLIC_KEY_HEX"
    }
  }
}
```

For explicitly local distribution replace `source` with
`{"local_dir":"/absolute/path/release-provider-root"}`. There is no HTTP fallback.
The publisher pin authorizes release bytes; the provider and relay pins identify
transport participants. They are separate identities.

`pin` freezes advancement. Change the service file to select a channel or pin;
the running service reads those settings at startup. A channel change does not
clear a pin. The highest accepted sequence is retained separately per channel.
Changing the publisher on an existing state directory is refused; key rotation
does not have an implemented trust-transition command.

The local administrative socket lives inside a mode-0700 state directory and
has mode 0600. Local filesystem access is the administrative authority. Ordinary
citizens and federation partners cannot invoke it through the relay protocol.
An existing socket path is refused, not silently removed.
An abrupt process kill can leave that socket behind. Before removing it, an
operator must establish that the service owning that exact state directory has
exited. Remove only the stale `admin.sock`, retaining the journal and release
files. Restarting then exposes the recovery block; it does not approve retrying
an interrupted update.
The managed entrypoint disables distributed Erlang. It does not expose an
additional network administration port or trust the cookie bundled in an archive.

## Release authoring

The release publisher must assign an immutable build identifier and author
application upgrade instructions for the exact old and new builds. Standard
`mix release` does not produce these instructions.

Prepare an unused, stopped copy of a base release:

```sh
mise exec -- mix run --no-start scripts/prepare-hot-base.exs \
  --root /absolute/path/base --build BASE_BUILD_ID --os darwin --arch aarch64
```

The helper writes `RELEASES` and `arc-build.json`. It refuses an already
initialized tree and its own running runtime root. It cannot detect every
other process using a tree: ensuring the target is stopped is the operator's
responsibility. The complete release root must remain writable and persistent.
Use a dedicated release root for each managed service. Sharing one mutable
release tree between running nodes is not supported.

Build a candidate from separately built release trees with authored `.appup`
files already present in the candidate's application directories:

```sh
mise exec -- mix run --no-start scripts/build-hot-release.exs \
  --base /absolute/path/base --candidate /absolute/path/candidate \
  --build CANDIDATE_BUILD_ID --os darwin --arch aarch64 \
  --output /absolute/path/candidate.tar.gz
```

Each changed application directory must have a distinct version. Unchanged
directories are omitted from the package. The running release's files must
never be overwritten during unpacking. Target release versions must also be
distinct; reusing a version for a different build is unsupported.

The initial hot path permits loading only `Arc.Net.Relay`, using soft purge and
explicit state changes. Changes to the acceptor, federation workers, route
shards, updater, application supervision, or runtime require additional proof
and are currently refused. A release signature does not override this check.

Publish with the read-only [release provider](../../providers/releases/README.md).
`Arc.CLI.Update.Manifest.sign/2` signs the strict channel document using an
explicit publisher identity. No official signing key or live channel is
provisioned by this implementation.

## Channel encoding

The implementation in `Arc.CLI.Update.Manifest` defines the closed JSON schema.
The signature covers `ARC-RELEASE-CHANNEL-V1` followed by one zero byte and
canonical JSON: sorted string keys, preserved array order, JSON strings,
integers, booleans and null. Floats and unknown fields are rejected. Signature
bytes use lowercase hexadecimal Ed25519. Sequence equality requires identical
canonical unsigned bytes. Expired or replayed metadata is rejected.

A release may carry an optional `install` object with the `sha256` and `size`
of its complete installation archive; see
[publishing a complete archive](#publishing-a-complete-archive). Every other
release key is required.

Each source edge's `upgrade_plan_sha256` and `downgrade_plan_sha256` bind the
**same complete raw `relup` file**, which contains both directions. Both fields
must equal that file's SHA-256. They are not hashes of separately encoded
instruction lists. Runtime identity is the exact ERTS version. Platform names
are the OS atom and the first component of `system_architecture`.

## Failure and recovery boundaries

Before mutation the updater writes a synced journal. It stages and checks the
archive, validates the source edge, then uses OTP's full release handler. Two
ephemeral citizens continuously exchange encrypted messages across the relay;
the observer checks their existing connections and the recorded routing
processes. Only a successful observation permits `make_permanent`.

The observer conservatively fails on unrelated citizen connection churn too.
It is not a federation traffic probe or a load benchmark.

Interrupted staging, application, observation, or commitment blocks further
operations. There is deliberately no `--force` or automatic rollback. Retain
the journal, package, old release and candidate directories, and reconcile
`release_handler.which_releases`, `RELEASES`, and `start_erl.data` before repair.
Do not delete the journal merely to retry an unknown installation outcome.
The journal retains the original release identities and interrupted phase when
a worker fails. The public status omits those private recovery details. If the
journal cannot be read or persisted during failure handling, further update
attempts remain blocked.
While the service is running, background channel checks preserve a blocked
status and its journal instead of overwriting the failure. For failures before
mutation that do not require reconciliation, an operator can explicitly check
or retry after addressing the cause.
An unsuccessful preflight can also leave a retained archive requiring inspection
before another staging attempt.

OTP can restart the node after certain non-recoverable installation failures.
Preflight rejects explicit restart instructions; it cannot promise that an
arbitrary migration will never fail. This implementation must not be described
as an unconditional no-disconnect guarantee.

Run the isolated packaged lifecycle proof with:

```sh
mise exec -- mix run --no-start scripts/test-managed-update.exs
```

It builds a disposable native base and synthetic candidate, drives signed local
channel checks and operator application, observes existing relay connections,
then restarts and checks the target application version through relay status.
It does not touch installed services or user keys.

Run process-kill recovery checks with:

```sh
mise exec -- mix run --no-start scripts/test-update-recovery.exs
```

The proof compiles fault barriers only into disposable fixture releases. It
kills the owned service after synced staging, applying, observing, committing
and success records, and after boot commitment but before recording success.
Before commitment, the old release remains bootable. After commitment, the
target boots. An interrupted update remains blocked in either case; a completed
success record does not need reconciliation. Each restart verifies the actual
relay version. A corrupt download is rejected before mutation and succeeds only
after restoring the correct bytes and explicitly applying again.

These are process-crash checks, not simulated power loss or disk-controller
failure. The proof removes only its own stale administrative socket after
observing process exit. It never deletes a recovery journal to allow a retry.

Run the native federated update proof with:

```sh
mise exec -- mix run --no-start scripts/test-managed-update.exs --federation
```

The managed relay fetches signed metadata and the archive from a network-shared
provider behind an approved partner. An encrypted event stream runs throughout
the update; the proof checks exact sequence delivery and unchanged citizen and
federation connection identities before restarting into the permanent target.
The complete update uses one federation edge. Separate source integration tests
exercise a relay chain with an intermediate transit operator, denied sharing,
denied transit and a broken link after partial archive download.

Production qualification still needs real release artifacts, packaged downgrade
and the remaining gates in [SPEC.md](SPEC.md). Synthetic lifecycle proofs do not
establish arbitrary migration rollback safety or host power-loss durability.
