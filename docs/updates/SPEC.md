# Live updates and release channels

## Status and boundary

This document defines the channel and rollout policy. The experimental
[managed relay implementation](OPERATIONS.md) provides `arc update`, signed
metadata verification and a release provider. `arc update` without `--socket`
replaces a local installation with a complete archive fetched through the
configured relay; it is a full replacement, not a hot upgrade, and follows the
same channel, publisher and distribution rules. Official signed channels and
automatic application are not enabled. The accompanying isolated proof
exercises a supported application upgrade and downgrade; it does not establish
that every ARC release can be hot-upgraded.

ARC currently ships complete `mix release` archives. Its installer replaces the
installation directory. Running services are started by the CLI; the relay and
its connection processes are not all children of an upgrade-aware application
supervision tree. There are no authored production `.appup`/`relup` packages.
An explicitly prepared upgrade-capable native base must be installed before channel-driven
live updates are enabled. Do not use the current installer on the running
release tree as an update mechanism.

## Run the isolated proof

```sh
mise exec -- mix run --no-start scripts/test-hot-upgrade.exs
```

The [proof fixture](../../test/fixtures/upgrades/README.md) constructs synthetic
old/new application packages from the real relay source and exercises
`release_handler.upgrade_app/2` and `downgrade_app/3`. Both fixtures explicitly
support the state migration and start the relay under a static application
supervisor. The proof checks code behavior and a reversible state field, sends
encrypted, sequenced traffic during each migration, and preserves the listener,
route tables, acceptors, shards, and existing connection processes.

The accept loop now lives in the separate `Arc.Net.Relay.Acceptor` module.
Previously it retained old `Arc.Net.Relay` code after an upgrade, preventing a
clean soft purge. This extraction prepares a future base; it does not change
already-running processes from an older installation. Changes to the acceptor
itself, federation processes, and external providers are outside this proof.

The test barriers establish that traffic passes during each transition. They do
not measure maximum throughput or establish a latency guarantee. This proof is
part of CI. A separate managed lifecycle proof builds a native base and synthetic
candidate, installs through the operator commands, observes existing connections,
and confirms permanent boot selection by restarting and querying relay status:

```sh
mise exec -- mix run --no-start scripts/test-managed-update.exs
```

That proof uses an explicitly local release source. Process interruption is
covered by a separate native proof:

```sh
mise exec -- mix run --no-start scripts/test-update-recovery.exs
```

It kills only owned disposable services at durable update phases, including the
gap between boot commitment and the success record. Restart verifies the chosen
release through public relay status, and interrupted mutations remain blocked.
A corrupt artifact is rejected without changing the boot choice; after repair,
an explicit operator retry succeeds. Fault barriers are compiled only into test
releases, not enabled by production configuration. This checks process death,
not host power loss, packaged downgrade, or arbitrary migration rollback.

The native proof also supports `--federation`: metadata and artifact retrieval
cross an approved partner link, while a sequenced encrypted event stream runs
throughout the update. It asserts exact delivery and unchanged citizen and
partner connection identities, then verifies permanent restart. Source tests
separately cover multiple federation edges and denial/interruption cases.
These tests use local isolated relays and synthetic releases, not a deployed
multi-machine network or real production migration.

## User contract

A service follows a channel selected by its operator. A channel says which
releases the publisher recommends; it does not grant permission to execute them.
Each running service has its own policy. Updating a local CLI installation must
not implicitly update a configured remote relay or every federated partner.

The intended outcomes are:

- `up_to_date`: the selected channel has no newer eligible release.
- `available`: a compatible update is ready for an operator decision.
- `scheduled`: an opted-in automatic update is waiting for its rollout window.
- `applying`: one update is running; concurrent attempts are rejected.
- `observing`: code has changed and runtime health is being checked.
- `current`: the new release passed checks and is committed as the boot default.
- `restart_required`: the newer release cannot preserve this running service.
- `blocked`: metadata, compatibility, storage, or health checks prevent updating.

A short pause in affected processes is allowed. Deliberately closing a citizen
socket, reconnecting on their behalf, restarting the node, or replaying an
application request is not a successful hot update. Report failures explicitly.
A hot-update failure is not a promise that connections can always be preserved.

## Channel selection

Initial channels are `stable` and `beta`. `stable` is the default and contains
only promoted final releases. `beta` also permits prereleases. Nightly builds
are outside the initial policy. Promotion changes a channel reference to an
immutable tested artifact; it must not rebuild different bytes under a version.

The operator can pin an exact release, which disables automatic advancement.
Changing a channel clears no pin implicitly. A selected channel and its last
verified state persist across restart; they are service configuration, separate
from citizen identity selection. Switching from beta to stable never silently
downgrades the running service. If stable is older, report the situation and
require an explicit supported downgrade decision.

Show both the channel's latest release and the newest eligible release for the
installed base. Never label an old installation globally up-to-date merely
because the latest release needs a restart. If an upgrade requires intermediate
versions, each edge must be explicitly supported and independently verified.
Do not infer compatibility from semantic version numbers alone.

## Automatic rollout policy

The selected initial policy is `notify`: checking may report availability, but
the operator starts every update. Selecting or changing a channel never applies
an update by itself. `auto_hot` below is a design for a later, separately enabled
mode; it is not part of the initial rollout. Neither mode grants automatic
restart permission. Package staging is also subject to an operator's
download/storage policy; selecting a channel alone does not imply background
bandwidth usage in a CLI that has no resident service.

Before `auto_hot` can apply an update, all of these must hold:

- The manifest belongs to the selected channel and trusted publisher.
- The exact source build, target build, operating system, architecture, and
  runtime combination have a declared, tested upgrade path.
- The upgrade uses the existing Erlang runtime and preserves the protocol and
  state required by already-connected citizens and federation partners.
- The release is admitted by the local version pin, maintenance window, and
  rollout cohort. Fleet membership does not override an operator's settings.
- Download verification and local preflight pass. No other update is running.
- A tested recovery path exists and the service meets its pre-update health
  requirements. Snapshotting metadata must not expose citizen keys or content.

Roll out first to explicitly designated canaries. Wider cohorts advance only
when the publisher marks the same artifact eligible and operators' own rules
allow it. Use per-service jitter to avoid simultaneous updates across partners.
An operator can pause or revoke eligibility at any time before application.
Do not change a running update mid-migration based on a newly fetched manifest.
An interrupted or failed update blocks automatic retries until its outcome has
been reconciled. Never loop through failures or force-purge processes to advance.

## Distribution and trust

Normal network discovery, channel metadata, and package transfer go through ARC
providers over the configured relay, following the existing federation sharing
rules. A local package may be supplied explicitly for offline operation. A
first installation remains a bootstrap operation. GitHub may produce or store
artifacts, but the future updater must not silently bypass the selected relay to
fetch them. Large artifacts need bounded chunked transfer with verified final
length and digest; ordinary bounded request/reply limits still apply.

Trust is pinned to a release publisher, separately from the chosen relay or
provider. Hosting or forwarding an artifact does not grant publishing authority.
The publisher signs versioned channel metadata with domain-separated signatures
and an unambiguous canonical encoding. The metadata binds at least:

- Channel, publisher, monotonic publication sequence, and expiry.
- Immutable release/build identity, target platform, byte length, and digest.
- Exact supported source builds and runtime versions.
- Upgrade and downgrade plan digests, compatibility requirements, and whether
  restart is required.
- Rollout eligibility and withdrawal state.

The eventual wire specification must freeze the encoding and signature domain
before interoperability is claimed. Persist the highest accepted sequence;
reject replayed metadata, expired metadata, and unexplained key changes. Cache
expiration cannot authorize an automatic update. Clock uncertainty or an
unavailable relay leaves the current service running. Resume partial downloads
only against the same immutable digest. Archive extraction must reject escaping
paths and links before writing to a staging directory.

Keep signing keys out of artifacts and public status. Publisher-key rotation
requires an explicit trust transition; an arbitrary new provider cannot rotate
it. A rollback uses an already verified retained artifact and an operator or
pre-authorized recovery decision, not stale channel metadata.

## Applying a live update

Retain the current release in its own versioned directory. Stage the candidate
beside it; never overwrite modules that an existing process might still load.
Use OTP's release handling and authored application/release upgrade plans.
Changes to process state need explicit, versioned forward and reverse migrations.

The base deployment must provide a writable, persistent release store and boot
from the committed release selection. Read-only or externally managed installs
must report that they cannot apply in place. An external deployment manager
replacing the installation can otherwise restore its own older image regardless
of an in-memory upgrade. Integrate that boot contract explicitly; do not inspect
or rewrite deployment-platform configuration from the generic updater.

Preflight the exact plan. Reject runtime restarts, process-killing purges, and
unsupported process or supervisor changes for the hot-only path. A signature
establishes publisher authority, not the correctness of an upgrade script.
The release pipeline must independently exercise every supported version edge.

After application, observe application-level traffic and inspect the same
pre-existing connections. A process answering status is insufficient evidence.
Only after the observation period succeeds should the release be made permanent
for the next boot. Persist the update journal outside either version directory.

Recovery is conditional: apply a downgrade only if its preconditions still hold
and the backward migration is tested. External side effects and durable schema
changes may make rollback unsafe. If safe hot recovery is unavailable, report a
blocked/failed update and require operator intervention. OTP can reboot after
certain release-handler failures; production planning must account for that.
Do not advertise an unconditional no-disconnect guarantee on failure.

A runtime upgrade, native-library incompatibility, or external provider runtime
change is a separate operation unless that exact transition has been proven.
Hot-loading ARC's Elixir code does not upgrade a Python SQLite provider, change
a container image, or reload arbitrary external executables.

## Administrative surface

The initial local `arc update` interface is described in [OPERATIONS.md](OPERATIONS.md).
It must show the
service being controlled, running versus staged versions, selected channel, pin,
automatic mode, compatibility reason, and last outcome. Public `arc status`
remains read-only. Knowing a relay address or connecting as a citizen does not
grant update authority.

Initially, mutating controls should require the service's local protected
administrative interface. Any later remote control must use explicit operator
authorization over ARC, with a distinct scope for update operations. Federation
partners never inherit this authority. Background checks must not turn a public
status call into an automatic update trigger.

## Evidence required before shipping automatic hot updates

The isolated proof is a first gate. Production readiness additionally requires:

- Actual packaged base-to-candidate installation through `install_release`,
  commitment with `make_permanent`, and a restart that boots the chosen version.
- Supported downgrade edges, failure injection around each durable update phase,
  and recovery from interruption without losing the previous bootable release.
- Signed-channel verification, replay/expiry/withdrawal checks, key-rotation tests,
  bounded transfer, and archive validation.
- Continuous encrypted traffic on existing citizen and federation connections,
  with unchanged connection identity, ordering, and no duplicate delivery.
- Real durable state migrations, overload and slow-peer tests, and upgrade
  coverage for all affected supervised and special processes.

## References

- [Elixir release documentation](https://mix.hexdocs.pm/Mix.Tasks.Release.html)
- [OTP release handling](https://www.erlang.org/doc/system/release_handling.html)
- [OTP application upgrade API](https://www.erlang.org/doc/apps/sasl/release_handler.html)
- [OTP state and supervisor migration examples](https://www.erlang.org/doc/system/appup_cookbook.html)
