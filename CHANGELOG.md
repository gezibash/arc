# Changelog

All notable changes to ARC are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/). Versions follow
[Semantic Versioning](https://semver.org/).

## [Unreleased]

## [0.6.0] - 2026-09-20

A relay and its clients must run the same version. This release changes the
relay announcement and the key exchange, so a mixed network does not work.

### Changed

- **Breaking:** the X25519 key of an identity now uses the standard
  conversion from Ed25519, the same as libsodium. Data that an earlier
  version sealed does not open. This includes DMs, private files, and the
  local sealed cache.
- A sender can now seal to any public key without a published key exchange
  record. `seal:to` computes the recipient key from the Ed25519 public key.
  A DM to a citizen on a different host no longer needs a shared control
  directory.
- Sessions compute the X25519 key of the peer from its Ed25519 public key.

### Removed

- **Breaking:** the key exchange record. `Control.publish_keyex/2` and the
  `x25519_public` field of control plane entries and relay announcements are
  gone. `arc publish` and `arc resolve` print no `keyex` or `x25519` lines.
  Relays and clients of this version reject announcements from earlier
  versions, and the reverse. Upgrade them together.
- The `has_key_exchange` field of the host `resolve` operation.

### Added

- `Arc.Identity.public_key_to_x25519/1` computes the X25519 public key of an
  identity from its Ed25519 public key.
- The exec provider at `providers/exec`. A granted key runs commands on the
  machine of a citizen through the relay. A job continues after the caller
  disconnects, and the provider reports the result to the caller. See
  `docs/exec/SPEC.md`.
- A citizen machine that pauses when it is idle. The caller wakes it, and the
  citizen holds a lease while it works. `providers/exec/citizen/` holds one
  script for each platform.
- `docs/exec/MLD.md`, the machine lifecycle definition. It describes the
  contract for a new platform, and the delegated identity of a per-task
  machine.
- `docker/fly/`, an image that runs a relay and the DM provider on one Fly.io
  machine.

## [0.5.2] - 2026-09-19

### Fixed

- A schema 2 channel can again announce a restart-only release without an
  `install` object if the release sets `eligible` to false. Channels published
  before `install` existed use this form. Since v0.5.0 such channels failed
  verification with `invalid_sources`, and a publisher could not add a new
  sequence to them. v0.5.0 and v0.5.1 still reject these channels.

## [0.5.1] - 2026-09-19

A managed relay must use a restart-only update for this release, because it
changes modules other than `Arc.Net.Relay`.

### Fixed

- One relay client could disconnect other clients. A packet header that was not
  a JSON object, or a header field that was not a string, crashed the
  relay-side connection before the signature check. The crash also stopped the
  acceptor and every connection that it accepted. The relay now drops such a
  packet, and connections no longer link to their acceptor.
- Relay-side connections now close when their relay stops, so clients
  reconnect to a restarted relay.
- A restarted route shard keeps the routes of live clients. A registration for
  a stopped shard now goes to its replacement.
- The relay traps exits and keeps its route shards linked, so the shards stop
  with the relay. A new relay also stops shards that a previous relay left.
- In the MCP HTTP server, a session that stops no longer stops the server or
  the other sessions. Its agent stops, and later requests get 404, so the
  client starts a new session with the same key.
- A second MCP `initialize` with a key that is already active gets 409. Before,
  it stopped the server.
- MCP session servers no longer restart after DELETE, so four fast deletes no
  longer stop the server.
- MCP calls that take too long get an HTTP error, not a closed socket. The
  `send` tool finishes in 20 seconds.
- The MCP server writes a missing JSON-RPC id as `null`, not `"nil"`.
- The MCP acceptor tries again after a temporary accept error, for example when
  file descriptors run out.

## [0.5.0] - 2026-09-18

### Added

- `arc update` without `--socket` updates the local installation through the
  configured relay as the active citizen key: it discovers a `releases`
  provider (or uses `--source`),
  verifies the signed channel against a remembered `--publisher` key with
  replay protection, downloads the newest eligible complete archive in
  bounded chunks, checks its digest and tar members, starts the candidate to
  confirm its version, and swaps it into place, keeping the replaced release
  as `<root>.previous`. `check` reports only; `status` reads local state.
  `--channel`, `--replace-publisher`, `--relay`, `--relay-pubkey` and
  `--format json` are supported.
- Channel documents accept an optional per-release `install` object naming
  the complete installation archive. Verifiers from earlier releases reject
  documents that carry it.
- `Arc.CLI.Update.Publisher` and `scripts/publish-release-channel.exs` sign and
  publish a channel document from an operator keystore key. Publication holds
  a root lock, requires an increasing sequence, keeps published builds
  immutable, checks the size and SHA-256 of every archive and `install`
  archive, and replaces the channel file atomically.
- Channel schema version 2 has the signature domain `ARC-RELEASE-CHANNEL-V2`.
  A version 2 release can have no hot-upgrade sources. It must then set
  `restart_required` and carry an `install` object that repeats its `sha256`
  and `size`, so `arc update` can install it. Releases through v0.4.1 reject
  version 2 documents.
- `scripts/verify-release-channel.exs` verifies a channel and downloads every
  archive through a pinned relay. `docker/local/compose.releases.yaml` adds an
  opt-in local releases provider. See `docs/updates/PUBLISHING.md`.

## [0.4.1] - 2026-09-17

### Added

- `arc join HOST[:PORT]` confirms and remembers a relay fingerprint, selects
  that relay for future commands, and creates an identity on a fresh setup.
  Known relay keys cannot be silently replaced. Explicit flags and environment
  settings retain precedence over saved relay defaults.

## [0.4.0] - 2026-09-17

### Added

- Experimental managed relay boot, a protected local `arc update` interface,
  signed release-channel verification, bounded ARC/local artifact transfer,
  conservative OTP package preflight, and a read-only release provider.
  Checks notify; updates require an operator. Production qualification and
  official signed channels are still required before rollout.
- An isolated application hot-upgrade/downgrade proof that passes encrypted
  traffic across existing relay connections while state migration is paused.
  CI runs the proof. This does not provide a production updater.
- A live-update design covering stable/beta channels and operator-started
  updates, with incompatible releases left pending for a restart decision.

### Changed

- Failed updates retain release identities and the interrupted phase in the
  private recovery journal. Unreadable recovery state blocks retries, and a
  rejected archive digest no longer leaves its temporary download behind.
- Moved the relay accept loop into its own module so it does not retain old
  relay callback code during the hot-upgrade proof.
- `arc status` now queries the configured relay by default, with service-owned
  fields, a key/value table, and `--format json` (`--json` alias). The previous
  combined snapshot JSON changes; use `arc host status` for the local host.
  Older relays need an upgrade for this query.
- Removed Docker inspection and the hardcoded local Compose project from
  `arc status`. Status no longer depends on the deployment platform.

### Fixed

- Background channel checks preserve failed-update status until an operator
  explicitly checks or retries, so a rejected download remains visible.
- Federation test shutdown uses one fixed deadline and optional stack-only
  diagnostics; shutdown also verifies cancellation of an outbound handshake.

## [0.3.2] - 2026-09-16

### Added

- `arc status` shows the selected identity, client relay configuration, live
  local host connections, and the local Compose stack. `--check` performs a
  bounded relay greeting check without registering a citizen; `--json` provides
  machine-readable output.

## [0.3.1] - 2026-09-16

### Added

- Identity selection through `ARC_KEY`, then `arc.key` in the current working
  directory, then `~/.config/arc/default.key`. Selector files contain existing
  identity names; invalid explicit selections fail without falling through.
- A local Docker Compose stack with a relay, journal, sealed DMs, and Agora,
  separate persistent identities and data volumes, and a public connection
  settings helper for installed clients.

### Changed

- `arc keys show` and `arc keys ls` report the effective identity and its source.
  `arc keys use` saves the global default to `default.key`. Existing `default_key`
  files remain readable until a new default is saved.

### Fixed

- Identity-selection errors now identify invalid or unreadable selector files.
  Failed legacy-selector cleanup reports partial success without crashing.
- Local provider containers retain their identities when upgrading the default
  selector format and use ARC's bundled runtime for their provider programs.

## [0.3.0] - 2026-09-16

### Added

- Relay-backed discovery with signed provider announcements, approved relay
  federation partners, opt-in onward traffic, and temporary cached catalogs.
- A private file-storage provider and client-side encryption, plus Agora's
  signed public discussions and local browser interface using the owner's ARC
  identity. Provider runtimes remain separately configured programs.
- `arc request <scheme+arc://provider-key/resource>` for bounded, opaque
  request/reply traffic through a pinned relay, or explicit local delivery.
  The SQLite provider supplies named databases and per-citizen grants.
- Owner-approved direct request/reply routes using mutual TLS, exact peer and
  resource policies, and finite permissions renewed through the relay. Either
  endpoint can accept the connection; both owners can opt into TCP hole punching.
- A reproducible cloud traversal lab with recorded successful direct routes,
  relay fallback, permission expiry, and interrupted-write behavior. See
  [verification results](docs/transport/VERIFICATION-2026-09-16.md). Home and
  mobile networks remain unverified.

### Changed

- Relay-backed citizens use their chosen relay even for nearby peers. Failed
  direct upgrades retain relay delivery. Application operations with unknown
  outcomes are never automatically replayed.
- The project website now uses full-screen parchment illustrations with
  accessible, restrained artwork motion.

### Fixed

- Strict lint and type-analysis findings across relay, provider, and local
  browser handling while preserving consent and message-delivery boundaries.

## [0.2.1] - 2026-09-15

### Added

- Installable releases: a mix release with the bundled Erlang runtime, one
  tarball per platform (linux-x86_64, linux-aarch64, darwin-aarch64), and a
  relay image on ghcr.io. A `vX.Y.Z` tag builds and publishes them. See
  `docs/DEPLOY.md`.

### Fixed

- `arc relay` no longer crashes in a release for lack of `:crypto`, and no
  longer prints an EXIT trace on SIGTERM.

- `arc dm fetch` rejects a blob name that could leave the message's blob
  directory. Attachment names never start with a dot.
- A `send` that fails part way, receipts included, removes every copy that
  landed and rebuilds each counter from disk.
- The mailbox byte counter rebuilds itself from a walk when the `usage`
  file is missing or malformed, never goes negative, and subtracts only
  bytes that actually left the disk.
- `retract` completes when a recipient already purged their copy, and emits
  `dm.retracted`.
- `thread` takes a conversation key as `conversations` prints it. A sender
  who lists themselves in `--to` keeps a plain one-to-one key.
- Events carry `to`, the conversation key the recipient can open.

## [0.2.0] - 2026-09-14

### Added

- `Arc.Identity.SealedBox`: seal a body to an X25519 public key with an
  ephemeral key, HKDF-SHA256, and ChaCha20-Poly1305. No session needed.
- `Arc.Identity.HKDF`: one RFC 5869 implementation for every derivation.
- Template filters `{{key|pubkey}}` and `{{key|seal:to}}`, a `seal_to`
  option on stdin inputs, and an `open` output filter that decrypts
  `sealed-v1:` tokens in a reply before the CLI prints it.
- `arc resolve` prints the peer's X25519 key. `arc publish` publishes it.
- `providers/dm`: sealed direct messages with a persistent inbox, receipts,
  threads, and a block list. Spec in `docs/dm/SPEC.md`.
- `arc version`.
- This changelog.

### Changed

- Session v2. The initiator derives the session key from a per-session
  ephemeral X25519 key, carried in the signed packet header as `ek`. This
  gives forward secrecy against compromise of the initiator's long-term
  key. The whitepaper now states exactly that.
- The replay guard evicts entries older than twice the clock skew window.

### Fixed

- A second `arc publish` no longer clears a published X25519 key.
- The test suite no longer writes to the real `~/.config/arc`.

### Deprecated

- Session v1 packets, those without `ek`, are still accepted and logged.
  The next release removes them.

## [0.1.0] - 2026-09-11

Initial release: identities, control plane, relay, exec providers, the
journal provider, MCP bridge.
