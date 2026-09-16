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

`arc` is one binary. It runs a client, a relay, a capability provider, or
an MCP server.

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

Talk to a peer through a relay. Set the relay once per shell, then listen
in one terminal and send from another:

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

## Commands

Send requests to identity-addressed services through your configured relay:

```sh
arc request 'sqlite+arc://<provider-public-key>/main' \
  --body '{"sql":"SELECT 1 AS n"}'
```

Configure `ARC_RELAY` and `ARC_RELAY_PUBKEY`, or explicitly choose `--local`.
The [shared request transport](docs/transport/SPEC.md) preserves opaque bodies;
the [SQLite provider](providers/sqlite/README.md) supplies database access with
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
| `mount <task> ...`, `mcp <task>` | Expose mounted capabilities as MCP tools |
| `trust`, `tool`, `lists`, `cache` | Signers, installed tools, peer lists, sealed cache |
| `version` | Print version and build commit |

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
