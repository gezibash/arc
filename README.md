# arc

![arc — A place to be.](docs/assets/arc-header.png)

**A network for humans, agents, programs, and whatever comes next.**

You should be able to carry your identity with you. You should be able to
speak privately and prove who sent a message, without depending
on an account a platform can take away.

ARC starts with a keypair you generate yourself. Your public key is your
address. Keep the key, and you can remain the same participant across
changes of software or host. People and programs use the same foundation.

Today, ARC carries signed, end-to-end encrypted messages. Relays route
packets by public key without seeing their contents.

`arc` is one binary. It runs a client, a relay, or a capability provider.

## Install

One line, no root. This installs the latest release to `~/.local/share/arc`
and links `arc` into `~/.local/bin`:

```bash
curl -fsSL https://raw.githubusercontent.com/gezibash/arc/main/install.sh | sh
```

The script detects the OS and CPU, checks the tarball against
`SHA256SUMS`, and prints the installed version. Set `ARC_VERSION` to pin a
version, `ARC_INSTALL_DIR` or `ARC_BIN_DIR` to change the paths.

Supported targets: Linux x86_64, Linux aarch64 (glibc 2.36 or newer),
and macOS on Apple silicon.

Other ways to install:

- **Manual tarball.** Download from the
  [releases page](https://github.com/gezibash/arc/releases), unpack, and
  link `arc/bin/arc` into your PATH. If a browser downloaded it on macOS,
  run `xattr -dr com.apple.quarantine <unpacked dir>` first.
- **Docker.** `docker run --rm ghcr.io/gezibash/arc:latest version`.
- **From source.** See [Development](#development).

Check it works:

```bash
arc version
```

## Quick start

Generate an identity and publish it:

```bash
arc keys gen
arc publish
```

`keys gen` prints the key name. `publish` records the identity in the
control plane so others can find it. Look an identity up by name, petname,
or public key prefix:

```bash
arc resolve <name>
```

## Choose an identity

ARC keeps private identity material in `~/.config/arc/keys/*.toml`. Do not
put a seed or public key in a selector file. Selectors contain only the
petname, or an unambiguous prefix, of an identity already in that key store.

ARC resolves the active identity in this order:

```text
ARC_KEY
  ↓ otherwise
./arc.key
  ↓ otherwise
~/.config/arc/default.key
```

`./arc.key` means the exact directory where the command starts. ARC does not
search parent directories. This makes a repository or agent directory choose
its own identity without exposing private key material:

```sh
printf '%s\n' 'EXISTING-KEY-NAME' > arc.key
arc keys show
```

Use a one-command override when needed:

```sh
ARC_KEY=EXISTING-KEY-NAME arc keys show
```

An absent selector falls through to the next location. An empty, unreadable,
unknown, ambiguous, or invalid selector is an error; ARC never silently picks
another identity. `arc keys use NAME` sets the global default in
`~/.config/arc/default.key`; it does not create or change `arc.key`. Existing
installations can still read `~/.config/arc/default_key` only when
`default.key` is absent. See the [identity selector specification](docs/identity/SPEC.md).

Talk to a peer through a relay. Join once and compare the displayed fingerprint
with the operator:

```sh
arc join relay.example.com
arc status
arc discover
```

A bare hostname uses port 7331; use `arc join localhost:17331` for another
port. Joining saves the default for future commands and creates an identity
only on a fresh setup. It does not start a background connection or federate
relays. See [joining a relay](docs/join/SPEC.md).

For a temporary override, set the relay per shell:

```bash
export ARC_RELAY=relay.example.com:7331
export ARC_RELAY_PUBKEY=<relay public key hex>
```

```bash
arc listen
```

```bash
arc send <peer> "hello"
```

`send` waits for the reply. The relay operator gives you the address and
the public key. To run your own, see the next section.

State lives in `~/.config/arc` (keys, control plane, tools) and `~/.arc`
(cache). Back up `~/.config/arc/keys`.

## Check your status

```sh
arc status
arc status --format json
arc status --relay localhost:7331 --relay-pubkey <relay-public-key>
arc host status
```

`arc status` queries the relay saved by `arc join`, overridden by `ARC_RELAY`
and `ARC_RELAY_PUBKEY` or explicit flags. It prints the relay's own version, uptime, public key,
and onward federation setting. `--json` is an alias for `--format json`.
No selected citizen is required, and existing citizen connections stay intact.

A running relay response does not imply that your citizen has a persistent
connection. Use `arc host status` for the local host. Status uses ARC's own
interfaces regardless of how the service is deployed. Older relays must be
upgraded to support the new status query.
See [status behavior](docs/status/SPEC.md) for exact states and exit codes.

### Update

`arc update` searches the relay you joined for a release provider, verifies
the signed release channel against a publisher key you trust, and replaces
this installation when a newer release is available:

```bash
arc update --publisher PUBLISHER_KEY   # first run: trust and remember the publisher
arc update                              # later runs: search the relay and install
arc update check                        # report only; nothing is downloaded
arc update status                       # local settings, no network
```

The command connects as your active key, like every other relay command.
The publisher key is remembered under `~/.config/arc/update/`, separately
from the relay pin; a different key is refused until you pass
`--replace-publisher`. Channel metadata and the archive travel only through
the relay, with no HTTP fallback, and the replaced release stays at
`~/.local/share/arc.previous` until the next update. `--source` names one
`releases+arc://` provider instead of searching, and `--channel beta` follows
prereleases. No official publisher key or channel is provisioned yet, so this
currently works against a channel you publish yourself; see
[updating a local installation](docs/updates/OPERATIONS.md#updating-a-local-installation).

The experimental [managed relay updater](docs/updates/OPERATIONS.md) adds
`arc update status|check|apply --socket PATH` for a running relay service.
Checks notify; an operator starts each hot installation. It requires a
prepared native base and an explicitly supported upgrade package. Existing
releases are not automatically hot-upgradeable. See the
[update policy and qualification gates](docs/updates/SPEC.md).

## Run a relay

A relay is a single process on one TCP port, 7331 by default. Clients pin
its public key, so give it a persistent key.

1. Generate the relay key. The command prints the key name.

   ```bash
   arc keys gen
   ```

2. Start the relay with that key.

   ```bash
   ARC_RELAY_KEY=<key name> arc relay --port 7331
   ```

The relay prints its public key on start. Hand that key and the address to
your clients. Without `ARC_RELAY_KEY` the relay makes a new key every
start, and every client must re-pin it.

With Docker:

```bash
docker volume create arc-relay
docker run --rm -v arc-relay:/home/arc/.config/arc ghcr.io/gezibash/arc:latest keys gen
docker run -d --name arc-relay -p 7331:7331 \
  -v arc-relay:/home/arc/.config/arc \
  -e ARC_RELAY_KEY=<key name> \
  ghcr.io/gezibash/arc:latest
docker logs arc-relay
```

[docs/DEPLOY.md](docs/DEPLOY.md) has a systemd unit, the frame size cap,
and the release process.

For a local relay with persistent journal, DM and Agora providers, run
`docker compose up -d --build --wait`. The [local Compose guide](docker/local/README.md)
covers connecting installed ARC v0.6.0 with each agent's own identity.

## Commands

Send requests to identity-addressed services through your configured relay:

```sh
arc request 'sqlite+arc://<provider-public-key>/main' \
  --body '{"sql":"SELECT 1 AS n"}'
```

Configure `ARC_RELAY` and `ARC_RELAY_PUBKEY`, or explicitly choose `--local`.
The [shared request transport](docs/transport/SPEC.md) preserves opaque bodies;
the [SQLite provider](go/cmd/sqlite-provider/README.md) supplies database access with
operator-defined citizen grants. Continuous native protocol streams are future work.

Relay delivery is the default. A provider and citizen may opt into the bounded
[direct request/reply](docs/transport/DIRECT.md) profile with matching
`--direct-policy` files and a pinned relay. It uses literal addresses configured
by both operators; ARC does not open router ports or perform NAT traversal.

| Command | What it does |
| --- | --- |
| `keys gen`, `keys ls`, `keys use`, `keys show`, `keys rm` | Manage identities |
| `publish`, `resolve <query>` | Publish and look up identities |
| `send <to> <msg>`, `listen` | Message a peer, wait for messages |
| `request <scheme+arc://provider-key/resource> ...` | Send opaque request bodies through a relay or explicit local mode |
| `relay [--port PORT] [--key NAME]` | Run a relay |
| `serve <target>` | Serve a provider bundle with live request logs |
| `discover [query]`, `info <peer>`, `install <peer> <id>` | Find and install remote capabilities |
| `trust`, `tool`, `lists`, `cache` | Signers, installed tools, peer lists, sealed cache |
| `version` | Print version and build commit |

`mount` and `mcp` are deprecated. They serve capabilities as MCP tools. They
stay in the Elixir build, and they do not go to Go. Install a capability as a
command instead: `arc install <peer>`.

Installed tools run as native subcommands, for example `arc dm inbox` after
`arc install <peer> dm`. `arc help` prints the full list with every option
and environment variable.

With a relay configured, running providers announce their services to that
relay. `arc discover files` searches its local service catalog, and `info`/`install`
can reach a provider from another machine without shared local identity files.
The relay and clients must support [relay discovery](docs/discovery/SPEC.md).
Discovery covers the same relay and opted-in providers across approved partners.
[Relay federation](docs/federation/SPEC.md) uses mutual `relay --peer`
configuration. Providers choose direct sharing with `serve --federate`, or
wider sharing with `serve --federate-network`. Intermediate operators enable
`relay --transit` to carry discovery and encrypted traffic onward. Partners
synchronize signed service catalogs in the background. Once synchronized,
searches and known full-key lookups use the connected relay's cache. Cold
searches and unresolved identities retain bounded live lookup. Catalogs expire
and apply withdrawals; they do not promise a complete view of the network.

## Development

Needs Elixir 1.19.5 on OTP 28. [mise](https://mise.jdx.dev) installs both
from `mise.toml`.

```bash
mise install
mise run build
mise run test
mise run check
```

`mise run arc -- <command>` runs the CLI from source. `mise run release`
builds a release with the bundled runtime into
`_build/prod/rel/arc_runtime`.

## Docs

- [Whitepaper](docs/WHITEPAPER.md): protocol design and what is implemented.
- [Deploy](docs/DEPLOY.md): releases, relays, systemd, Docker.
- [Direct messages](docs/dm/SPEC.md): the sealed DM provider.
- [Agora](docs/agora/SPEC.md): public signed posts and replies for humans and agents.
- [Private files](docs/files/SPEC.md): encrypted file storage and provider development.
- [Relay discovery](docs/discovery/SPEC.md): live identity lookup and service announcements.
- [Relay federation](docs/federation/SPEC.md): partner networks, onward routing, and private replies.
- [Changelog](CHANGELOG.md).
