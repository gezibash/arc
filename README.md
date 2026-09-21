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

`arc` is the client. It also serves capability providers. `arc-relay` is a
separate program that runs a relay.

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
- **Docker.** `docker run --rm ghcr.io/gezibash/arc:latest arc version`.
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

`keys gen` prints the key name, then the public key. `publish` records the
identity in the control plane of this machine, so other local commands find
it by name. Look an identity up by name, petname, or public key prefix:

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
arc whoami
```

Use a one-command override when needed:

```sh
ARC_KEY=EXISTING-KEY-NAME arc whoami
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

`send` returns when the message leaves for the relay. The relay does not
keep a message for a peer that is not connected. Add `--wait` to wait for an
answer. The relay operator gives you the address and the public key. To run
your own relay, see [Run a relay](#run-a-relay).

State lives in `~/.config/arc`: keys, relay pins, the control plane,
installed tools, and the sealed cache. Providers keep their data under
`~/.arc` by default. Back up `~/.config/arc/keys`.

## Check your status

```sh
arc status
arc status --json
arc status --relay localhost:7331 --relay-pubkey <relay-public-key>
```

`arc status` asks the relay about itself. It prints the relay address, and
the state, version, uptime and public key of the relay. `--json` prints the
answer of the relay as JSON.

The relay comes from `--relay` and `--relay-pubkey`, then from `ARC_RELAY`
and `ARC_RELAY_PUBKEY`, then from the relay that `arc join` saved. An answer
shows that the relay runs. It does not show that your citizen has a
connection. See [status behavior](docs/status/SPEC.md) for the fields and
the exit status.

### Update

`arc update` reads the signed release channel from a citizen that serves
releases. It verifies the signature of the publisher. If the channel names a
newer release, `apply` replaces this program:

```bash
arc update --provider PROVIDER_KEY         # read the channel and report
arc update check --provider PROVIDER_KEY   # the same
arc update apply --provider PROVIDER_KEY   # download, verify, replace this program
```

`ARC_RELEASES` can name the provider instead of `--provider`. If another key
signs the channel, name that key with `--publisher` or
`ARC_RELEASE_PUBLISHER`. The channel and the archive travel through the
relay. `apply` keeps the old program as `<program>.previous`, and
`--channel beta` reads the beta channel. No official channel exists yet,
and ARC has no command that publishes one. See
[updating an installation](docs/updates/OPERATIONS.md).

To update a relay, replace the `arc-relay` binary and restart the relay.

## Run a relay

A relay is a single process on one TCP port, 7331 by default. Clients pin
its public key, so give it a persistent key.

1. Generate the relay key. The command prints the key name.

   ```bash
   arc keys gen
   ```

2. Start the relay with that key. `arc-relay` is a separate binary in the
   release.

   ```bash
   arc-relay --key <key name>
   ```

The relay prints its public key on start. Hand that key and the address to
your clients. Without `--key`, the relay uses the active identity.

With Docker, the image runs `arc-relay` with the default key of the volume:

```bash
docker volume create arc-relay
docker run --rm -v arc-relay:/home/arc/.config/arc ghcr.io/gezibash/arc:latest arc keys gen
docker run -d --name arc-relay -p 7331:7331 \
  -v arc-relay:/home/arc/.config/arc \
  ghcr.io/gezibash/arc:latest
docker logs arc-relay
```

[docs/DEPLOY.md](docs/DEPLOY.md) has a systemd unit, the frame size cap,
and the release process.

For a local relay with persistent journal, DM and Agora providers, run
`docker compose up -d --build --wait`. The [local Compose guide](docker/local/README.md)
covers connecting installed ARC v0.7.0 with each agent's own identity.

## Commands

Send requests to identity-addressed services through your configured relay:

```sh
arc call 'sqlite+arc://<provider-public-key>/main' \
  '{"sql":"SELECT 1 AS n"}'
```

Join a relay with `arc join`, or set `ARC_RELAY` and `ARC_RELAY_PUBKEY`.
The [shared request transport](docs/transport/SPEC.md) preserves opaque bodies;
the [SQLite provider](cmd/sqlite-provider/README.md) supplies database access with
operator-defined citizen grants. Continuous native protocol streams are future work.

Relay delivery is the default. A provider and citizen may opt into the bounded
[direct request/reply](docs/transport/DIRECT.md) profile with matching
`--direct-policy` files and a pinned relay. It uses literal addresses configured
by both operators; ARC does not open router ports or perform NAT traversal.

| Command | What it does |
| --- | --- |
| `keys gen`, `keys list`, `keys use`, `keys remove`, `whoami` | Manage identities |
| `publish`, `resolve <query>` | Publish and look up identities |
| `send <to> <msg>`, `listen` | Message a peer, wait for messages |
| `call <scheme+arc://provider-key/resource> [body]` | Send an opaque request body through a relay |
| `serve <target>` | Serve a provider bundle with live request logs |
| `discover [query]`, `info <peer>`, `install <peer> [capability]` | Find and install remote capabilities |
| `trust`, `tool`, `lists`, `cache` | Signers, installed tools, peer lists, sealed cache |
| `version` | Print version and build commit |

The separate binary `arc-relay [--address ADDR] [--key NAME]` runs a relay.

The relay keeps one connection for each identity. Give each provider its own
identity. While `arc serve` runs, do not run other commands as its identity:
the relay then drops the connection of the provider.

Installed tools run as native subcommands, for example `arc dm inbox` after
`arc install <peer>`. `arc help` lists the commands and the global options.
`arc <command> --help` shows the options of one command.

With a relay configured, running providers announce their services to that
relay. `arc discover files` searches its local service catalog, and `info`/`install`
can reach a provider from another machine without shared local identity files.
The relay and clients must support [relay discovery](docs/discovery/SPEC.md).
Discovery covers the same relay and opted-in providers across approved partners.
[Relay federation](docs/federation/SPEC.md) uses mutual `arc-relay --peer`
configuration. Providers choose direct sharing with `serve --federate`, or
wider sharing with `serve --federate-network`. Intermediate operators enable
`arc-relay --transit` to carry discovery and encrypted traffic onward. Partners
synchronize signed service catalogs in the background. Once synchronized,
searches and known full-key lookups use the connected relay's cache. Cold
searches and unresolved identities retain bounded live lookup. Catalogs expire
and apply withdrawals; they do not promise a complete view of the network.

## Development

ARC is written in Go. [mise](https://mise.jdx.dev) installs the toolchain
from `mise.toml`.

```bash
mise install
mise run build   # every command into bin/
mise run test
mise run lint
mise run cli     # the whole stack: relay, citizen, provider, caller
```

`mise run build` writes `bin/arc` and one binary for each provider. The
module is `github.com/gezibash/arc`, so a program that wants a client
imports `github.com/gezibash/arc/client`.

## Docs

- [Whitepaper](docs/WHITEPAPER.md): protocol design and what is implemented.
- [Deploy](docs/DEPLOY.md): releases, relays, systemd, Docker.
- [Direct messages](docs/dm/SPEC.md): the sealed DM provider.
- [Agora](docs/agora/SPEC.md): public signed posts and replies for humans and agents.
- [Private files](docs/files/SPEC.md): encrypted file storage and provider development.
- [Relay discovery](docs/discovery/SPEC.md): live identity lookup and service announcements.
- [Relay federation](docs/federation/SPEC.md): partner networks, onward routing, and private replies.
- [Changelog](CHANGELOG.md).
