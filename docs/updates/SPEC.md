# Updates and release channels

- Status: built
- Layers: runtime
- Owns: none
- Proof: go test -count=1 -run '^TestDelivery$' ./internal/proof/
- Unverified: Official signed channels and automatic application are not enabled. The proof signs its own channel and serves it through a local relay. No test sends an archive on a pipe between two machines, or through the TURN server of a relay.

`arc update` replaces the program on this machine with one that the publisher
signed. It reads a channel document from a citizen that serves releases,
checks the signature, picks the release for this platform, downloads the
archive, checks its hash, and swaps the binary. The old binary stays beside
the new one as `<name>.previous`.

## 1. Purpose

This document defines the channel and rollout policy, the signed channel
document, and how `arc update` applies a release. The
[releases app](../../apps/releases/README.md) serves the channels and the
archives, and documents how a publisher signs a channel.

ARC does not hot-upgrade. A running service keeps its old program in memory
until it restarts. An update therefore needs a restart to take effect, and
the operator chooses when. Official signed channels and automatic application
are not enabled.

## 2. Terms

| Term | Meaning |
| --- | --- |
| Publisher | The Nostr key that signs a channel document. |
| Channel | A named list of releases that the publisher recommends: `stable` or `beta`. |
| Channel document | The signed JSON document of one channel. |
| Releases provider | A citizen that serves channel documents and archives with the releases app. |
| Archive | A release tarball, looked up by its SHA-256. |
| Pin | An exact release that the operator selects. It stops automatic advancement. |
| Sequence | The monotonic publication number of a channel document. |

## 3. Rules

### 3.1 The channel document

Trust is pinned to a release publisher, separately from the relay or the
provider. To host or forward an artifact grants no publishing authority.

- The publisher MUST sign the channel document with a domain-separated
  signature over a canonical encoding. The domain is
  `ARC-RELEASE-CHANNEL-V<schema>` followed by one zero byte, and then the
  canonical JSON of the unsigned document.
- The schema is 3. The publisher is a Nostr key, 32 bytes as hex.
- The signature has the algorithm `bip340`: a BIP-340 Schnorr signature over
  the SHA-256 of the signed bytes.

The document binds:

- Channel, publisher, a monotonic publication sequence, and an expiry.
- An immutable release identity, the target platform, the byte length, and
  the digest of each artifact.
- Whether a restart is required.
- Rollout eligibility and withdrawal state.

A release that carries an archive names it like this:

```json
{
  "version": "0.7.0",
  "build": "BUILD_ID",
  "runtime": "go1.27.1",
  "platform": {"os": "linux", "arch": "amd64"},
  "size": 41234567,
  "sha256": "ARCHIVE_SHA256",
  "sources": [],
  "restart_required": true,
  "withdrawn": false,
  "eligible": true,
  "install": {"size": 41234567, "sha256": "ARCHIVE_SHA256"}
}
```

A release without `install` is reported as the latest of the channel, and
cannot be installed.

A client:

- MUST keep the highest accepted sequence.
- MUST refuse a replayed document, an expired document, and an unexplained
  change of key. An expired cache authorizes no update.

### 3.2 Channels

A service follows a channel selected by its operator. A channel says which
releases the publisher recommends. It does not grant permission to run them.
Each running service has its own policy. Updating a local CLI installation
must not update a configured remote relay, or any other machine.

- The channels are `stable` and `beta`. `stable` is the default, and holds
  promoted final releases only. `beta` also permits prereleases. Nightly
  builds are outside this policy.
- Promotion changes a channel reference to an immutable tested artifact. It
  must not rebuild different bytes under one version.
- The operator can pin an exact release, which stops automatic advancement.
  A change of channel clears no pin.
- A selected channel and its last verified state survive a restart. They are
  service configuration, separate from the selection of a citizen identity.
- A move from beta to stable never silently downgrades a service. If stable
  is older, report that, and require an explicit decision.
- Show both the latest release of the channel and the newest release that
  this platform can run. Never call an old installation up to date because
  the latest release needs a restart.

### 3.3 Rollout

The policy is `notify`. A check may report that a release is available. The
operator starts every update. To select or change a channel applies nothing
by itself, and grants no permission to restart.

- Roll out first to designated canaries. A wider group advances only when the
  publisher marks the same artifact eligible, and the rules of the operator
  allow it.
- An operator can pause or revoke eligibility at any time before
  application.

### 3.4 Distribution

- Discovery and channel metadata go through a releases provider, as live
  calls over the configured relays. The archive goes over a pipe of a
  session with that provider (section 3.7), or in chunks of live calls.
- A local package may be supplied for offline operation. A first
  installation is a bootstrap operation.
- GitHub may build or store artifacts, but the updater must not bypass the
  selected relay to fetch them.
- A large artifact moves in bounded chunks, with a verified final length and
  digest. One reply carries at most 64 KiB of an archive. A gift wrap grows a
  reply about 2.4 times, and a relay takes an event of at most 256 KiB.
- Resume a partial download only against the same digest.
- A session over the relay can also carry the archive, as raw bytes. The
  provider keeps this path for older clients. `arc update` does not use it,
  because chunks were faster on a local relay (section 8).

### 3.5 Applying a release

- The program downloads the archive to a staging file, checks its length and
  its digest, and reads one program out of it.
- Archive extraction must refuse a path that escapes the staging directory,
  and refuse a link. A member that is not a regular file is refused.
- The program writes the candidate beside the target as `<name>.new`, runs it
  once with `--version` to prove that it starts and reports the expected
  version, and only then renames it over the target.
- The old binary becomes `<name>.previous`. A failed rename puts the old
  binary back.
- A lower version is never installed.

### 3.6 What one machine remembers

`<identity directory>/update/<publisher>/<channel>.json` holds the highest
sequence that this identity accepted, and the digest of that document. The
identity directory is `~/.config/arc/citizens/<name>`.

- Each publisher and each channel holds its own record.
- A record that cannot be read is an error, not an empty record: the update
  stops, and the operator looks. To start again, remove the file.

### 3.7 The archive on a pipe

A consumer can get the archive on a pipe of
[the direct spec](../direct/SPEC.md), section 3.7. The session stays on the
relay. It carries the request, the `direct` record, the `end` record and the
final outcome. The pipe carries only the archive bytes of that one session.

| Order | On the relay (session records) | On the pipe |
| --- | --- | --- |
| 1 | Open: `{"op":"archive","digest":"sha256:<hex>","direct":<offer>}` | |
| 2 | `{"type":"direct","sdp":"<answer>"}` | |
| 3 | | The data channel opens. The provider has not opened the archive. |
| 4 | | The archive bytes, then `end`; the consumer answers `ok` |
| 5 | `{"type":"end","body_bytes":<n>}` | |
| 6 | Close: the final outcome | |

The session is a `server_stream` session. A session request holds at most
16 KiB, and the offer adds about 1 to 3 KiB, so the manifest of the
releases app declares `max_bytes` 16384.

The provider:

1. If the request has no `direct`, it MUST send the raw archive bytes on the
   session, as before.
2. If the digest is not valid, it MUST close the session with
   `invalid_request`, before the pipe.
3. If its policy is `session`, it MUST close the session with `no_pipe`.
   `ARC_PATH` sets the policy, as the direct spec defines.
4. If it holds 4 pipes, it MUST close the session with `busy`.
5. It answers the offer with the `direct` record, and waits for the pipe. If
   the pipe does not open, it MUST close the session with `no_path`. It MUST
   NOT open the archive before the pipe opens.
6. It writes the archive on the pipe, and closes the write side of the
   pipe. After the consumer has the last byte, it sends the `end` record.
   `n` is the number of archive bytes on the pipe.
7. If the pipe breaks during the archive, it MUST close the session with
   `pipe_failed`.

The consumer:

- It picks the path before the first archive byte: `--path`, else the
  setting of `arc path`, else `auto`. It MUST NOT move a download to
  another path.
- It offers a pipe only to a provider that this identity installed, and
  whose interface declares `server_stream`. A session needs both.
- Each attempt of the direct spec is one new session. An attempt that
  fails sent no archive bytes, so the next attempt is not a replay.
- It MUST compare the number of bytes on the pipe with `body_bytes`. A
  different number is an error.
- It MUST check the length and the SHA-256 of the archive against the
  channel document, as for chunks (section 3.5). A pipe gives no other trust.
- If the pipe closes before its end, the consumer MUST stop with an error
  that says the outcome is not known. It MUST NOT ask again, and it MUST
  NOT replace the program.
- In `auto`, a pipe that does not open is an error. The consumer MUST NOT
  then use chunks.
- In `auto`, if the provider refuses the offer with `invalid_request`,
  `no_pipe` or `request_too_large`, the consumer reads the archive in
  chunks, and writes `the provider has no pipe; using chunks over the
  relay`. In `direct` and `turn`, it stops with an error.
- In `auto`, if this identity did not install the releases app with
  `server_stream`, the consumer reads the archive in chunks, and writes the
  reason. In `direct` and `turn`, it stops with an error.
- In `session`, the consumer reads the archive in chunks.

## 4. Behavior

### 4.1 The commands

```sh
arc update                       # read the channel, and say what it would do
arc update check                 # the same, said plainly
arc update apply                 # download, verify, and replace the program
```

Each command takes:

| Flag | Meaning |
| --- | --- |
| `--provider` | The citizen that serves the releases, by public key |
| `--publisher` | The public key that signs the channel |
| `--channel` | The channel to read. The default is `stable` |
| `--program` | The program to replace. The default is this one |
| `--path` | The path of the archive: `auto`, `direct`, `turn` or `session`. The default is the setting of `arc path`, else `auto` |

The relays are the relays of the identity, from `arc relay add`. The identity
comes from `arc keys use`, from `ARC_KEY`, or from `--key`.

### 4.2 What one run does

1. Asks the provider for the channel document with a live call, the way
   `arc call` does. Before the call, `arc` runs the wake hook of the
   provider, when it has one.
2. Verifies the document: the publisher signed it, the signature covers the
   canonical bytes under a domain that names the schema, the channel is the
   one that was asked for, the document has not expired, and its sequence
   does not go backwards.
3. Selects the newest release for this operating system and CPU that carries
   an archive and is eligible.
4. Reports what it found. `apply` continues.
5. Downloads the archive, and checks its length and its SHA-256. If this
   identity installed the releases app, and its interface declares
   `server_stream`, the archive comes on a pipe (section 3.7). Otherwise,
   and with `--path session`, it comes in chunks of 64 KiB through the
   relay. At the end, arc writes one line on standard error with the path,
   the bytes, the time and the speed:

   ```text
   path direct (host to host): 21056992 bytes in 0.3 s, 529.9 Mbit/s
   path chunks (relay replies): 21056992 bytes in 2.9 s, 59.0 Mbit/s
   ```
6. Reads one program out of the archive, see section 3.5.
7. Writes the candidate, proves that it starts, and renames it over the
   target, see section 3.5.

`arc update` keeps calling the releases provider itself, because it replaces
`arc` and verifies what it installs.

### 4.3 Outcomes

- `up_to_date`: the selected channel has no newer eligible release.
- `available`: a compatible update is ready for an operator decision.
- `applying`: one update is running. Another attempt is refused.
- `restart_required`: the program is replaced, and the service still runs the
  old one.
- `blocked`: metadata, platform, storage, or a failed check prevents updating.

The local `arc update` interface must show the program being replaced, the
running version, the selected channel, any pin, and the last outcome.

### 4.4 After an update

Restart each service that runs the old program. To see the program on disk,
run `arc version`. If the new program does not start, the old one is still
there as `<name>.previous`. Move it back.

Replacing the ARC binary does not change a container image, and does not
reload an external program that a provider runs.

## 5. Failures

- Report every failure explicitly. Deliberately closing the connection of a
  citizen, reconnecting on their behalf, or replaying an application request
  is not a successful update.
- A failed update blocks automatic retries until its outcome is reconciled.
  Never loop through failures to advance.
- An uncertain clock or an unreachable relay leaves the service running.
- A read-only or externally managed installation must report that it cannot
  apply in place. An external deployment manager that replaces the
  installation would otherwise restore its own older image. Do not inspect or
  rewrite the configuration of a deployment platform from the updater.
- A record that cannot be read stops the update, see section 3.6.

- A pipe that does not open, or that breaks, stops the update with an
  error. The program stays, see section 3.7.

## 6. Security

- A publisher, a provider, or anyone between them could otherwise serve an
  older document and hold the citizen on an old release. The signature of an
  old document still verifies, because it was real once. The sequence is what
  refuses it.
- Keep signing keys out of artifacts and out of public status.
- Rotation of a publisher key needs an explicit trust transition. An
  arbitrary new provider cannot rotate it.
- A rollback uses a retained verified artifact and an operator decision, not
  stale channel metadata.
- A signature establishes publisher authority, not the correctness of a
  release. The release pipeline must exercise every supported platform.
- To know a relay address, or to connect as a citizen, grants no update
  authority.
- A pipe shows the IP address of each end to the other end, as the direct
  spec says. `arc update` offers a pipe by default. An operator who does
  not want this runs `arc path turn` or `arc path session`. A provider sets
  `ARC_PATH`.
- Any later remote control must use explicit operator authorization over ARC,
  with its own scope for update operations. A relay or a provider never
  inherits that authority. A background check must not turn a status call
  into an update.

## 7. Compatibility

- Schemas 1 and 2 used Ed25519 keys of the older stack, and this build does
  not read them.
- A client of v0.9.0 or older reads only schemas `1` and `2`, so it must
  install a newer release with `install.sh` first.
- An older client never sends `direct`. It gets the archive in chunks, or
  as raw bytes on a session, as before. No older client breaks.
- An older provider decodes the archive request strictly, and refuses
  `direct` with `invalid_request` before it opens the archive. A provider
  with `max_bytes` 4096 refuses the larger request with
  `request_too_large`. In `auto`, a new client then reads chunks, and says
  so. With `--path direct` or `--path turn`, it stops:
  `the provider has no pipe; run again with --path session`.
- `arc update apply` now offers a pipe by default. It then shows the IP
  address of this machine to the provider, and needs UDP. `--path session`
  keeps the old chunk path.

## 8. Proof

```sh
go test ./runtime/release/...
```

The tests cover the signature domain, a changed document, an expired
document, a replayed sequence, platform selection, a wrong hash, a short
download, an archive that escapes its directory, and a program that does not
start after the swap.

These tests prove section 3.7, each through a local relay:

```sh
go test -count=1 -run '^(TestArchiveOverPipeThroughRelay|TestOlderReleasesProviderFallsBackToChunks|TestArchivePipeFailureIsAnError|TestBrokenArchivePipeIsAnError|TestBadArchiveOverPipeIsRefused)$' ./cmd/arc/
go test -count=1 -run '^TestArchiveOfferRefusals$' ./apps/releases/server/
```

They cover the archive on the pipe and the path line, `--path session`, a
citizen that did not install the app, an older provider, a pipe that does
not open, a pipe that breaks, a changed archive byte, `no_pipe` and `busy`.

A run on one Mac, through a local relay with no rate limit, on the loopback
address, 2026-10-12. The archive held a build of `arc`: 21,056,992 bytes.
Three runs of each path:

| Path | Time | Speed |
| --- | --- | --- |
| pipe, `--path direct` and `auto` | 0.3 s | 523.5 to 533.4 Mbit/s |
| chunks (a citizen that did not install the app) | 2.8 to 2.9 s | 58.1 to 59.2 Mbit/s |
| session frames (the raw-byte session that v0.24.0 reads) | 5.9 s | 28.3 to 28.5 Mbit/s |

The loopback address and a relay with no rate limit flatter each path.

`mise run delivery` proves the whole path. A publisher signs a channel with
`arc release sign`, a releases provider serves it through a relay, and an
older `arc` replaces itself with `arc update apply`.
