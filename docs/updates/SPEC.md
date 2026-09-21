# Updates and release channels

## Status and boundary

This document defines the channel and rollout policy. The
[operations guide](OPERATIONS.md) describes the commands.

`arc update` replaces the program on this machine with one that the publisher
signed. It reads a channel document from a citizen that serves releases,
checks the signature, picks the release for this platform, downloads the
archive, checks its hash, and swaps the binary. The old binary stays beside
the new one as `<name>.previous`.

ARC does not hot-upgrade. A running service keeps its old program in memory
until it restarts. An update therefore needs a restart to take effect, and
the operator chooses when. Official signed channels and automatic application
are not enabled.

## Run the proof

```sh
go test ./release/...
```

The tests cover the signature domain, a changed document, an expired
document, a replayed sequence, platform selection, a wrong hash, a short
download, an archive that escapes its directory, and a program that does not
start after the swap. One test verifies a channel document that the Elixir
implementation signed, which pins the canonical encoding.

## User contract

A service follows a channel selected by its operator. A channel says which
releases the publisher recommends. It does not grant permission to run them.
Each running service has its own policy. Updating a local CLI installation
must not update a configured remote relay, or any federated partner.

The outcomes are:

- `up_to_date`: the selected channel has no newer eligible release.
- `available`: a compatible update is ready for an operator decision.
- `applying`: one update is running. Another attempt is refused.
- `restart_required`: the program is replaced, and the service still runs the
  old one.
- `blocked`: metadata, platform, storage, or a failed check prevents updating.

Report every failure explicitly. Deliberately closing a citizen socket,
reconnecting on their behalf, or replaying an application request is not a
successful update.

## Channel selection

The channels are `stable` and `beta`. `stable` is the default, and holds
promoted final releases only. `beta` also permits prereleases. Nightly builds
are outside this policy. Promotion changes a channel reference to an
immutable tested artifact. It must not rebuild different bytes under one
version.

The operator can pin an exact release, which stops automatic advancement.
A change of channel clears no pin. A selected channel and its last verified
state survive a restart. They are service configuration, separate from the
selection of a citizen identity. A move from beta to stable never silently
downgrades a service. If stable is older, report that, and require an
explicit decision.

Show both the latest release of the channel and the newest release that this
platform can run. Never call an old installation up to date because the
latest release needs a restart.

## Rollout policy

The policy is `notify`. A check may report that a release is available. The
operator starts every update. To select or change a channel applies nothing
by itself, and grants no permission to restart.

Roll out first to designated canaries. A wider group advances only when the
publisher marks the same artifact eligible, and the rules of the operator
allow it. An operator can pause or revoke eligibility at any time before
application. A failed update blocks automatic retries until its outcome is
reconciled. Never loop through failures to advance.

## Distribution and trust

Discovery, channel metadata and package transfer go through ARC providers
over the configured relay, and follow the federation sharing rules. A local
package may be supplied for offline operation. A first installation is a
bootstrap operation. GitHub may build or store artifacts, but the updater
must not bypass the selected relay to fetch them. A large artifact moves in
bounded chunks, with a verified final length and digest.

Trust is pinned to a release publisher, separately from the relay or the
provider. To host or forward an artifact grants no publishing authority. The
publisher signs the channel document with a domain-separated signature over
a canonical encoding: the domain is `ARC-RELEASE-CHANNEL-V<schema>` followed
by one zero byte, and then the canonical JSON of the unsigned document.

The document binds:

- Channel, publisher, a monotonic publication sequence, and an expiry.
- An immutable release identity, the target platform, the byte length, and
  the digest of each artifact.
- Whether a restart is required.
- Rollout eligibility and withdrawal state.

Keep the highest accepted sequence. Refuse a replayed document, an expired
document, and an unexplained change of key. An expired cache authorizes no
update. An uncertain clock or an unreachable relay leaves the service
running. Resume a partial download only against the same digest. Archive
extraction must refuse a path that escapes the staging directory, and refuse
a link.

Keep signing keys out of artifacts and out of public status. Rotation of a
publisher key needs an explicit trust transition. An arbitrary new provider
cannot rotate it. A rollback uses a retained verified artifact and an
operator decision, not stale channel metadata.

## Applying an update

The program downloads the archive to a staging file, checks its length and
its digest, and reads one program out of it. It writes the candidate beside
the target as `<name>.new`, runs it once with `--version` to prove that it
starts, and only then renames it over the target. The old binary becomes
`<name>.previous`. A failed rename puts the old binary back.

A read-only or externally managed installation must report that it cannot
apply in place. An external deployment manager that replaces the
installation would otherwise restore its own older image. Do not inspect or
rewrite the configuration of a deployment platform from the updater.

A signature establishes publisher authority, not the correctness of a
release. The release pipeline must exercise every supported platform.

Replacing the ARC binary does not change a container image, and does not
reload an external program that a provider runs.

## Administrative surface

The local `arc update` interface is described in
[OPERATIONS.md](OPERATIONS.md). It must show the program being replaced, the
running version, the selected channel, any pin, and the last outcome. Public
`arc status` stays read-only. To know a relay address, or to connect as a
citizen, grants no update authority.

Any later remote control must use explicit operator authorization over ARC,
with its own scope for update operations. Federation partners never inherit
that authority. A background check must not turn a public status call into
an update.
