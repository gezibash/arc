# arc

![arc — A place to be.](docs/assets/arc-header.png)

**A network for humans, agents, programs, and whatever comes next.**

You should be able to carry your identity with you. You should be able to
speak privately and prove who sent a message, without depending
on an account a platform can take away.

ARC starts with a keypair you generate yourself. Your public key is your
address. Keep the key, and you can remain the same participant across
changes of software or host. People and programs use the same foundation.

ARC moves signed Nostr events. A private event is sealed to its recipient,
and a relay, a USB stick or another machine carries it without reading it.
See [the delivery layer](docs/delivery/SPEC.md).

`arc` is the one program. It keeps your identities, talks to relays, calls
apps, serves their interfaces, and runs a relay.

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
- **From source.** See [Development](#development).

Check it works:

```bash
arc version
```

A machine on v0.9.0 or older must install v0.10.0 with the script. The
`arc update` of those versions reads only the older stack.

## Quick start

New to ARC? Follow the [step-by-step getting-started guide](docs/GETTING-STARTED.md).
For assistant-guided setup, use the [arc-onboarding skill](.claude/skills/arc-onboarding/SKILL.md).

Make an identity, and add a relay:

```bash
arc keys gen
arc relay add wss://<relay>
arc whoami
```

To reach citizens that share no relay with you, name an indexer relay. `arc`
publishes its relay lists there, and looks up the relay lists of others
there. For example:

```bash
arc relay add wss://purplepag.es --index
```

`keys gen` prints the name of the identity, then its public key. The first
identity is the default. `arc keys use <name>` changes the default, and
`--key <name>` or `ARC_KEY` picks another identity for one command.

Each identity has its own directory, `~/.config/arc/citizens/<name>`,
with its key, store, relays and installs. `--home` or `ARC_HOME` names
another home. Back up the key files.

`arc keys gen --encrypt` seals a key with a passphrase, as NIP-49 defines.
`arc` reads the passphrase from `ARC_PASSPHRASE`, or asks on the terminal.
`arc keys add <bunker-uri>` uses a key that a NIP-46 signer holds.

## Messages

```bash
arc message send <public key> "hello"
arc sync
arc message inbox
```

A message is a NIP-17 direct message, so a NIP-17 client opens it. The
message waits in the outbox until the recipient acknowledges it. `arc sync`
reconciles this machine with its relays. `arc sync --dir <path>` syncs with
a directory instead: a USB stick, a shared folder, or a disk that you carry.

## Apps

An app adds commands through a signed interface manifest. Journal runs local
data commands. SQLite adds client commands and has a separate service program.
Find an app, review its permissions, and install its commands:

```bash
arc discover exec
arc install <author-or-service> <app>
arc exec run uname -a
```

`arc install` shows the app's permissions and records your consent. It installs
commands, without downloading a program or starting a service. `arc help <name>`
lists its commands. `arc apps list`, `info <name>` and `remove <name>` manage
installs. If a new interface asks for more permissions, ARC requires consent
again. See [the app model and layout](apps/README.md).

`arc call` sends one request. An address names the app interface, the service identity
and the resource:

```bash
arc call 'sqlite+arc://<service>/main' '{"sql":"select 1 as n"}'
arc call 'exec+arc://npub1.../' '{"argv":["uname","-a"]}'
```

The service identity is a key, an npub, an installed name, or a domain for NIP-05.
`arc call` shows the reply as the manifest of the service says: exec shows the
output and exits with the code of the command, and sqlite shows a table.
`--raw` writes the reply as it came. See
[addresses](docs/interface/SPEC.md#141-addresses). With a relay, the call
is live. With `--later`, or with no relay, it travels like a message, and
`arc call results` shows the reply.

Before a live call, `arc` runs the wake hook of the service identity from
`~/.config/arc/wake.toml`. Without a hook, the service needs a current
announcement. See [the Exec app](apps/exec/README.md).

`arc lists add <command> <name> <citizen>...` saves a set of citizens. Where
the command takes a key, the name of the list runs it once for each member.

## Run an app service

```bash
arc serve "exec://$(command -v arc-exec)?manifest=$PWD/apps/exec/manifest.json"
```

`arc serve` runs a program, announces its service, answers live calls through each relay,
and answers carried calls on each sync. It signs the announcement again
every 2 minutes. `arc apps init` creates an app with a starter service program.
Apps with an Arcfile can be run with `arc serve <app-directory>`.

Core also supports live server streaming and duplex sessions. A service declares
its interaction modes; `arc session <address>` uses the same session machinery
as calls between app services. See the [session protocol](docs/sessions/SPEC.md).
The bundled [SQLite](apps/sqlite/README.md#live-sql-sessions),
[Exec](apps/exec/README.md#streaming-processes-and-terminals),
[HTTP](apps/http/README.md), and
[Releases](apps/releases/README.md#streaming-archives) apps implement
these interactions. Use `--exec`, `--tty`, `--http`, or `--websocket` on
`arc session` to select their CLI I/O mappings. Existing request/reply commands
remain available. See the [app service session guide](docs/sessions/PROVIDERS.md).


## Run a relay

```bash
arc relay serve --listen 127.0.0.1:7447
```

The relay is a khatru relay. It serves NIP-42 authentication, NIP-77 sync,
and sealed data only to its author. Flags turn on write limits: an event
size cap, authentication or proof of work for gift wraps, a rate for each IP
address, and a store cap. `--group <id>` hosts a NIP-29 group. See
[Deploy](docs/DEPLOY.md) for how to run a relay.

## Update

```bash
arc update check --provider <provider> --publisher <publisher>
arc update apply --provider <provider> --publisher <publisher>
```

`arc update` reads the signed release channel from a releases provider,
checks the signature of the publisher, and replaces this program. The old
program stays as `<program>.previous`. `ARC_RELEASES` and
`ARC_RELEASE_PUBLISHER` can name the provider and the publisher. No official
channel exists yet. See [updating an installation](docs/updates/OPERATIONS.md)
and [publishing a channel](docs/updates/PUBLISHING.md).

## Upgrade from v0.10.0

From v0.11.0, the home of `arc` is `~/.config/arc`. v0.10.0 kept it in
`~/.config/arc/next`, beside the files of the older stack. Stop every `arc`
command, then move the home one time:

```bash
mv ~/.config/arc ~/.config/arc-old
mv ~/.config/arc-old/next ~/.config/arc
```

`~/.config/arc-old` then holds only the files of the older stack. If a
start script of the exec app names `ARC_HOME`, change it.

## Development

ARC is written in Go. [mise](https://mise.jdx.dev) installs the toolchain
from `mise.toml`.

```bash
mise install
mise run build       # every command into bin/
mise run check       # lint and test
mise run specs       # the specs, their gates, owners and kinds
mise run delivery    # the delivery layer, end to end
mise run interface   # the capability interface, end to end
```

`mise run build` writes `bin/arc`, `bin/arc-exec`, `bin/arc-sqlite`,
`bin/arc-http` and `bin/arc-releases`. App code and manifests live in `apps/`.
Read [AGENTS.md](AGENTS.md) before a change: it holds the package rules and
the spec rules.

## Docs

Guides:

- [Getting started](docs/GETTING-STARTED.md): a first identity, relay,
  message and app call.
- [Apps, programs, services and sessions](apps/README.md).
- [Deploy](docs/DEPLOY.md): releases, relays, Docker.
- [Architecture and package boundaries](docs/ARCHITECTURE.md).

Specs:

- [Delivery layer](docs/delivery/SPEC.md): events, transports, sync, calls,
  and the switchover.
- [App interface](docs/interface/SPEC.md): manifests, commands, and data that
  a citizen keeps for itself.
- [Sessions](docs/sessions/SPEC.md): streaming and duplex interactions.
- [HTTP over ARC](docs/http/SPEC.md).
- [Exec](docs/exec/SPEC.md): remote commands and wakeable citizens.
- [Updates](docs/updates/SPEC.md): signed release channels.
- [How to write a spec](docs/SPEC-TEMPLATE.md): the gates that each spec
  answers, the [event kinds](docs/KINDS.md), and the
  [proposals](docs/proposals/).

Other:

- [Whitepaper](docs/WHITEPAPER.md): protocol design.
- [Changelog](CHANGELOG.md).
