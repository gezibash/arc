# Changelog

All notable changes to ARC are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/). Versions follow
[Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- A provider program can call a capability that its citizen installed. The
  program writes a `call` line. `arc serve` makes the call as the citizen,
  and writes a `result` line. The program never holds the key. A call to a
  capability that the citizen did not install fails with `not_installed`.
  In Go, a handler gets a `provider.Caller`. See docs/interface/SPEC.md,
  section 14.2.
- HTTP over ARC. `provider.HTTP` serves a Go `http.Handler` as a provider,
  and opens no port. The handler reads the key of the caller from the header
  field `Arc-Caller`. See docs/http/SPEC.md.
- `http-provider` serves an HTTP server of any language over ARC. It starts
  the server, and forwards each call to it. The server calls other
  capabilities through a local endpoint that takes only its token. See
  docs/http/SPEC.md, section 10.
- The examples `examples/notes` and `examples/notes-server`: one HTTP
  service that keeps its notes in SQLite over ARC. The first runs in the
  provider program. The second is a plain HTTP server with no ARC library,
  behind `http-provider`. `mise run compose` proves both end to end.

### Removed

- **Breaking:** the `Events` and `WantsEvents` interfaces of the `provider`
  package, and `Emit`. `arc serve` dropped each event line of a provider, so
  an event never reached its citizen. If a provider has a `SetEvents` method,
  remove it.

### Fixed

- `arc serve <bundle directory>` gives the program each argument of
  `runtime.args` in the Arcfile. Before, the program got the JSON text of
  the list, split at each space. For example, `args = ["-u", "server.py"]`
  gave one argument: `["-u","server.py"]`. The `args` parameter of an
  `exec://` address can now be a JSON list, and an argument in the list can
  contain a space. An address that gives the arguments with spaces between
  them, for example `args=-v+--x`, still works.

## [0.15.4] - 2026-09-23

### Fixed

- The binaries in the image and in the Fly.io relay image report a clean
  build. Before, `arc version` printed "commit <sha> (modified)" for them,
  because the build left out tracked files and git saw them as deleted.

## [0.15.3] - 2026-09-23

### Fixed

- The linux-aarch64 and darwin-aarch64 binaries of a release report a clean
  build. Before, `arc version` printed "commit <sha> (modified)" for them,
  because the release build saw its own tarballs in `dist/` as changes.

## [0.15.2] - 2026-09-23

### Fixed

- `arc serve` prints "serves" only when at least one relay has the watch for
  live calls. Before, a relay that refused its first watch also counted, so
  with no relay up, "serves" came and every live call failed with "mute: no
  one was listening for this". `arc serve` logs each relay that refused its
  first watch, tries it again every 3 seconds, and sends the announcement
  again. With no relay up, `arc serve` waits for one.
- `arc serve` sends the relay lists of NIP-65 and NIP-17 to a relay or an
  indexer that comes back after start-up. Before, a relay or an indexer that
  was down at start-up never got them. Then a caller that shares no relay
  with the provider could not find the provider. Each sync now sends the
  lists to the relays. Each sync also sends them to an indexer that did not
  get them at start-up, until that indexer has them.

## [0.15.1] - 2026-09-23

### Fixed

- `arc serve` prints "serves" only after each relay has taken the watch for
  live calls. Before, a first call just after that line could fail with
  "mute: no one was listening for this". A watch of a relay now waits for the
  end of stored events, for up to 15 seconds.

## [0.15.0] - 2026-09-23

A live call finds the relays of the provider through its NIP-65 relay list,
and an indexer relay can hold those lists. `arc call` uses the newest
announcement of the provider. No manifest changes, so machines can update in
any order.

### Added

- A citizen publishes its NIP-65 relay list, kind 10002, beside its other
  relay lists. `arc relay add`, `arc relay rm` and `arc serve` publish it.
- A live call also goes to the read relays of the provider's NIP-65 list, so
  a caller and a provider that share no relay can call. See
  `docs/delivery/SPEC.md`, section 11.4.
- `arc relay add <url> --index` names an indexer relay. `arc` publishes its
  NIP-65 and NIP-17 relay lists there, and looks up the lists of others
  there, for live calls and for direct messages. `arc` names no indexer by
  default. See `docs/delivery/SPEC.md`, section 7.2.

### Fixed

- `arc call` asks the relays for the newest announcement of the provider
  before each call, as an installed command does. Before, it used the
  announcement that this machine held, so a new manifest, for example the
  `service.output` of 0.14.0, took effect only after `arc sync`.

## [0.14.0] - 2026-09-23

`arc call` shows a reply as the manifest of the service says. Update each
caller to 0.14.0 before a provider of exec or sqlite serves the new manifest:
a caller of 0.13.0 refuses it.

### Added

- The service of a manifest can have `output`: the pipeline that shows the
  reply of `arc call`. `arc call` then shows the reply as the service says,
  and `--raw` writes it as it came. See `docs/interface/SPEC.md`, section 4.3.
- `arc call` of exec shows the output of the command, and exits with its
  code, or 75 while a job runs. `arc call` of sqlite shows a table.

### Changed

- **Breaking:** the manifests of exec and sqlite use `service.output`. A
  caller of 0.13.0 refuses a manifest with a field that it does not know.
  Update each caller before a provider serves the new manifest.

## [0.13.0] - 2026-09-23

### Added

- `arc call` takes an address again: `<scheme>+arc://<provider>/<path>`, as
  the older stack did. The scheme names the capability, the path is the path
  of the request, and the provider can now be a key, an npub, an installed
  name, or a domain for NIP-05. See `docs/interface/SPEC.md`, section 14.1.

## [0.12.1] - 2026-09-23

### Fixed

- A command whose output renders to nothing shows nothing. Before, it showed
  an empty line, for example `arc exec run true`.

## [0.12.0] - 2026-09-23

`arc exec run` exits with the exit code of the command again. Update each
caller to 0.12.0 before a provider of `exec` serves the new manifest: a
caller of 0.11.0 refuses it.

### Added

- The output of a manifest command can have `exit`: rules that set the exit
  status of the command from its first record. See `docs/interface/SPEC.md`,
  section 9.4.

### Changed

- **Breaking:** the manifest of `exec` uses `exit`. A caller of 0.11.0
  refuses a manifest with a field that it does not know. Update each caller
  before a provider serves the new manifest.

### Fixed

- `arc exec run` exits with the exit code of the command, as `arc-exec`
  did. `arc exec status` exits 75 while the job runs, and 1 when the job is
  lost. Before, both exited 0.

## [0.11.0] - 2026-09-23

The older stack is gone, and `arc` is the only program of ARC. This is step 5
of the switchover in `docs/delivery/SPEC.md`, section 15.1. The home of `arc`
moves to `~/.config/arc`: see "Upgrade from v0.10.0" in the README.

### Changed

- **Breaking:** the home of `arc` is `~/.config/arc`, where it was
  `~/.config/arc/next`. Move it one time, as the README says.
- The image `ghcr.io/gezibash/arc` runs `arc relay serve` on port 7447. It
  ran `arc-relay` on port 7331.
- The petnames of keys come from `delivery/keys`. The names stay the same.

### Removed

- **Breaking:** `arc-legacy` and `arc-relay`, and the packages of the older
  stack: `identity`, `sealedbox`, `session`, `packet`, `frame`,
  `internal/wire`, `announce`, `relay`, `client`, `direct`, `control`,
  `citizen`, `toolbox` and `relays`.
- The DM, journal, Agora and files providers. Their manifests in
  `manifests/` replace them, with no provider.
- `arc-exec`, the local Compose stack, `mise run cli`, and the docs of the
  older stack. Links to those docs point to the tag v0.10.0.
- A binary, `arcn`, that v0.10.0 committed by mistake.

## [0.10.0] - 2026-09-22

`arc` now runs the delivery layer on Nostr events, and the older program is
`arc-legacy`. This is step 4 of the switchover in `docs/delivery/SPEC.md`,
section 15.1. `arc` of 0.9.0 cannot update to this release: install it with
`install.sh`. Move each identity by hand: `arc keys gen` makes a new one, and
the older keys stay with `arc-legacy`.

### Added

- Before a live call, `arc` runs the wake hook of the provider from
  `<home>/wake.toml`. Without a hook, the provider needs an announcement
  that is at most 5 minutes old, or the call stops with `peer_offline`.
  `arc serve` signs its announcement again every 2 minutes.
- `arc lists add`, `rm` and `ls` save sets of citizens for one installed
  command. Where the command takes a key, the name of a list runs it once for
  each member.
- `arc update`, `update check` and `update apply` read a signed release
  channel from a releases provider with live calls, and replace the program.
- `arc release sign` checks the archives of a channel, signs it, and writes
  it for the releases provider.

### Changed

- **Breaking:** `arc` is the program of the delivery layer, built from
  `cmd/arc`. Up to 0.9.0 it was `arcn`. It reads `ARC_HOME`, `ARC_KEY` and
  `ARC_PASSPHRASE`, where `arcn` read `ARCN_HOME`, `ARCN_KEY` and
  `ARCN_PASSPHRASE`. Its home stays `~/.config/arc/next` until 0.11.0.
- **Breaking:** the older program is `arc-legacy`, built from
  `cmd/arc-legacy`. It writes a deprecation notice to standard error on each
  run, and 0.11.0 removes it. `arc-legacy` and `arc-relay` read the identity
  from `ARC_LEGACY_KEY`, where they read `ARC_KEY`. `install.sh` links only
  `arc`.
- The local Compose stack runs `arc-legacy`.

- **Breaking:** a release channel has schema 3. Its publisher is a Nostr key,
  and its signature is BIP-340 Schnorr. This build does not read schemas 1
  and 2, which used Ed25519. A machine on 0.9.0 installs the next release
  with `install.sh`.
- `release.ChunkBytes` is 64 KiB, so that one reply fits in an event of
  256 KiB on a relay.
- The citizen scripts of the exec provider run `arc serve`. `citizen/init`
  takes `--relay` as a Nostr relay URL, and no longer takes
  `--relay-pubkey`.

### Fixed

- `arc -v` prints the version, as `arc --version` does.
- Two commands of one identity run at once. A command holds the files of
  the identity only while it uses them, and lets them go 200 ms later.
  Before, a second command failed with `store: timeout` while `tail` or
  `serve` ran.

- `arc --version` prints the version. Before, it failed, and `arc update`
  could not check a new program.
- Two equal live calls in one second are both answered. Each request rumor
  carries a `nonce` tag. Before, the provider refused the second call as a
  replay, and the caller waited until its timeout.

### Removed

- `docker/fly/`, the image of the older relay and the DM provider on Fly.io.
  The Fly.io app `arc-relay-gezim` is stopped. The Nostr relay,
  `docker/fly-nostr/`, replaces it.

## [0.9.0] - 2026-09-22

`arcn` gets the identity, tool and info commands of `arc`, and a relay that
limits writes. The Fly.io app `arc-nostr-gezim` runs that relay. Update
`arcn` before you use that relay: an `arcn` of 0.8.0 cannot make a live call
through it, or send mail to an inbox on it.

### Added

- `arcn keys gen`, `add`, `list`, `use`, `remove`, `encrypt` and `bunker`
  manage identities. Each identity has its own directory,
  `<home>/citizens/<name>`. `--key <name>` and `ARCN_KEY` pick an identity
  for one command. `arcn whoami` shows the identity.
- `arcn tool list`, `info`, `remove`, `resolve`, `apps init` and `version`
  come from `arc`.
- `arcn relay serve` takes write limits. Each limit is off when its flag is
  absent. `--max-event-bytes` caps the size of one event. `--wrap-auth` takes
  a gift wrap only after NIP-42 authentication, and `--wrap-pow` lets a gift
  wrap with NIP-13 work in without it. `--rate` and `--burst` cap the events
  of one IP address, and `--ip-header` names the header that holds that
  address. `--max-store-mb` caps the store.
- `docker/fly-nostr/` runs `arcn relay serve` as the Fly.io app
  `arc-nostr-gezim`, with the write limits on. `mise run check-relay -- <url>`
  checks one running relay.
- A capability can carry bytes. With `encoding: base64` in `request_body` or
  `response_body`, the citizen passes that body to the runtime as base64,
  and reads it back the same way. No byte changes on the way.
- `arc serve` and `arc listen` take `--federate` or `--federate-network`.
  With `--federate`, the direct partners of the relay find the citizen. With
  `--federate-network`, the wider network finds it where transit allows. The
  two flags are mutually exclusive.
- `arc listen` announces the citizen when it connects, and again every 150
  seconds. `arc resolve` and `arc send` find a listener by name.
- A relay with partners asks them live when its catalog is cold, or when it
  does not hold a name or a public key. The lookup carries an id, a path and a
  budget of 64. Each relay runs at most 32 lookups and asks at most 16
  partners for each.

### Changed

- **Breaking:** `arcn key` is gone. `arcn keys add` replaces `arcn key use`,
  and `arcn whoami` replaces `arcn key show`. An `arcn` home keeps each
  identity in `<home>/citizens/<name>`.
- **Breaking:** `arc join` takes the pin with `--relay-pubkey`, like the
  other commands. `--pubkey` is gone.
- `arc join` shows the key and the petname of a new relay, and pins the key
  only after you type `yes`. It asks the relay with a temporary identity, so
  it never replaces the route of a running citizen. With no key and no
  selector, it makes the first identity of the machine. It warns when
  `ARC_RELAY` or `ARC_RELAY_PUBKEY` hides the relay that it saved.
- `arc status` asks the relay with a temporary identity. It needs no key,
  reads no selector, and never replaces the route of a running citizen.
- `arc-relay --generate` makes a key only when the store holds none, and
  makes it the default, so the next start finds the same key. With
  `--key NAME`, it refuses a name that the store does not hold.
- `arc call` writes the response body byte for byte. It adds a newline only
  when standard output is a terminal.
- A direct policy that does not match leaves `arc call` on the relay, with a
  note on standard error. Before, the call failed.
- A provider has 5 seconds to end after its input closes. Only then does the
  citizen kill it.
- A lookup by name or prefix that did not reach every partner fails with
  `federation_unavailable`. Before, it returned its entries with
  `partial: true`.

### Fixed

- The status of a relay that runs with `--transit` says
  `"federation_transit": true`. Before, it always said `false`.
- An empty `arc.key` or `default.key` fails the command. Before, `arc` went
  on to the next selector.
- A selector that names no key fails with the selector and the name, for
  example `ARC_KEY names "nope"`. Before, the error said `no identity`.
- `arc` reads the legacy `~/.config/arc/default_key` when `default.key` is
  absent, as 0.6.0 did. `arc keys use` removes it after it writes
  `default.key`.
- `arc whoami --key NAME` says `chosen by --key`.
- An unknown subcommand, such as `arc keys show`, fails with status 1.
  Before, `arc` printed the help and exited with status 0.
- Each error prints once, after `arc:`.
- Two joins at the same time keep both relays. A lock on
  `relays.json.lock` covers each read and write of `relays.json`.
- `arc serve` says why it stopped: `the relay connection ended`, or
  `the provider stopped`. Before, it said `signal: killed`. A stop with
  Ctrl-C exits with status 0.
- When the relay connection of `arc serve` ends, a direct route that stands
  serves until its lease ends.
- The citizen refuses a request id that already waits, and a request over
  256 that wait.
- `arc call` refuses an address path with a percent escape, a dot segment,
  or `/info`. It refuses a capability of another id or another mode than
  `request_reply`, and a body that is not UTF-8 for a text capability. The
  citizen refuses such a body too.
- The Agora board holds its directory with `flock`, so it starts again after
  a restart. Before, a restart could leave it at `storage_locked` for good.
- `Relay.Close` no longer waits for a client that joined while it closed.
- A direct route renews its lease over the relay while half of the lease
  remains. Before, a route never renewed its lease.
- When the lease of a direct route ends, both sides close the carrier, and the
  conversation returns to the relay. Before, both sides kept the carrier open
  after the lease ended.
- A live call of `arcn` answers the NIP-42 challenge of a relay, and sends
  again. Mail of `arcn` answers the challenge of the inbox relay of a
  recipient with a one-time key.
- `arcn` keeps the request of a relay for authentication when a query
  closes. Before, a read of a sealed part could fail at random.
- When no relay answers, `arcn` says so. Before, it said that an event was
  missing, and gave the wrong advice.

### Known issues

- `arc` refuses the `private_file`, `sealed_file` and `agora` inputs, so
  `arc files` and `arc agora` fail.

## [0.8.0] - 2026-09-21

`arc` wakes a citizen whose machine pauses, and it says at once when a
citizen is not there. This completes phase 2 of `docs/exec/SPEC.md`. Only the
caller needs this version. A citizen on 0.7.0 works with it.

### Added

- `arc` wakes a citizen whose machine pauses. Before a request to the
  citizen, it runs the wake hook of the citizen from `wake.toml` in the
  directory of ARC. After an answer, it skips the hook for 30 seconds.
- A hook that exits with a status other than 0 stops the request with
  `wake_failed`. A hook that does not end in 30 seconds, or before the
  deadline of the request, stops it with `wake_timeout`.
- A hook that this `arc` cannot run, for example of a kind other than
  `command`, fails only the requests to its citizen. A `wake.toml` that is
  not valid fails every request, and no other command.
- A request to a citizen without a wake hook needs a current announcement of
  the citizen on the relay. Without one, the request stops at once with
  `peer_offline`. Before, it waited for the timeout.
- `arc resolve` shows the presence state of each citizen: `online`,
  `asleep` (no announcement, and a wake hook), or `offline`. It finds a
  citizen that sleeps by the name or the key of its wake hook.
- The `wake` package runs the wake flow. The `client` package takes a waker
  as `Options.Waker`, so every request of a library user follows the flow,
  and `Peers.Online` asks the relay about one citizen.

### Changed

- `arc-exec` leaves the wake to `arc`. It no longer reads `wake.toml` or
  `ARC_WAKE_CONFIG`, and it needs Python 3.9 or newer instead of 3.11.

### Removed

- The traversal lab, its results, and the tests and tools that needed the
  Elixir implementation. The results stay at the `v0.5.2` tag.

### Fixed

- The relay of the Fly.io image reports its release version. Build the image
  with `--build-arg VERSION=X.Y.Z`. Before, the relay reported `dev`.

### Known issues

- `arc` refuses the `private_file`, `sealed_file` and `agora` inputs, so
  `arc files` and `arc agora` fail.

## [0.7.0] - 2026-09-21

ARC is a Go module. A release holds one static binary for each command, and a
citizen machine needs no language runtime. `arc` runs the relay protocol of
earlier versions. `arcn` runs the delivery layer on Nostr events.

### Changed

- **Breaking:** Go replaces the Elixir implementation. The release tarballs
  and the image hold Go binaries.
- **Breaking:** the relay is the binary `arc-relay`. Start it with
  `arc-relay --key NAME --address :7331`. The image runs `arc-relay` by
  default. To run another command in the image, name it, for example
  `docker run IMAGE arc keys gen`.
- **Breaking:** `arc call ADDRESS [BODY]` replaces
  `arc request ADDRESS --body BODY`. `--timeout` counts seconds, and the
  default is 30. The body comes from the second argument or from standard
  input. `--input`, `--output` and `--local` are gone.
- **Breaking:** `arc status` takes `--json` only. `--format` and `--check`
  are gone. An error goes to standard error, and the JSON has no `address`
  field.
- **Breaking:** `arc install` takes `--yes` in place of `--trust`.
- **Breaking:** `arc update` reads the channel from the provider that
  `--provider` or `ARC_RELEASES` names. It does not search the relay.
  `arc update status`, `--source` and `--replace-publisher` are gone.
- `arc send` returns when the message leaves for the relay. `--wait` waits
  for an answer.
- `arc keys list` marks the global default with `*`, not the active
  identity.
- **Breaking:** `arc whoami` replaces `arc keys show`. `arc keys gen` prints
  the name and the public key on two lines. `arc keys list` and
  `arc keys remove` replace `arc keys ls` and `arc keys rm`.
- The exec citizen runs the `exec-provider` binary of a release, so the
  machine needs no Go toolchain. `citizen/init` finds the binary next to `arc`
  and writes `EXEC_PROVIDER` to `citizen.env`. `arc-exec` sends each request
  with `arc call`.
- `citizen/citizen-up` looks for its own announcement in up to 50 exec
  citizens. Before, it looked in 10.

### Added

- The delivery layer, in `delivery/` and the `arcn` command. Every datum is a
  signed Nostr event. A relay or a directory, such as a USB stick, carries the
  events. See `docs/delivery/SPEC.md`.
- Version 1 of the capability interface. A capability is a JSON manifest of
  primitives. Seven providers use it: exec, sqlite, releases, journal, dm,
  agora and files. See `docs/interface/SPEC.md`.

### Removed

- **Breaking:** the MCP server, `arc mcp` and `arc mount`.
- **Breaking:** the hot upgrade of a running relay. To update a relay, replace
  the binary and restart the relay.
- **Breaking:** `arc host`.
- **Breaking:** `arc tool update` and `arc apps open`.
- The TCP hole punch of direct connections.

### Fixed

- Two races made the direct route and federation tests fail under load. A
  control message now always travels on the relay. A lock for each federation
  link covers the encryption and the write. A relay drops the link of a
  partner connection that ended.
- The image, the Fly.io entrypoint, the local Compose stack and the exec
  citizen scripts called `arc relay`, `arc keys show` or `arc request`. The
  Go `arc` does not have these commands. The local relay health check used
  the Erlang runtime.
- The image reports its release version. Before, it reported `dev`.

### Known issues

- `arc` refuses the `private_file`, `sealed_file` and `agora` inputs, so
  `arc files` and `arc agora` fail.

## [0.6.0] - 2026-09-20

No release was published for this version.

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
  [verification results](https://github.com/gezibash/arc/blob/v0.5.2/docs/transport/VERIFICATION-2026-09-16.md). Home and
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
