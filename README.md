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
capabilities, serves them, and runs a relay.

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

Make an identity, and add a relay:

```bash
arc keys gen
arc relay add wss://arc-nostr-gezim.fly.dev
arc whoami
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

## Capabilities

A provider announces capabilities as signed events. Find one, trust it, and
call it:

```bash
arc discover exec
arc install <provider> --yes
arc exec run uname -a
```

`arc install` shows what the capability can do, and records your consent. An
installed capability adds its own commands, `arc <name> <command>`. `arc help
<name>` lists them. If a new version of a capability asks for more, `arc`
stops until you install it again.

`arc call` sends one request. An address names the capability, the provider
and the resource:

```bash
arc call 'sqlite+arc://<provider>/main' '{"sql":"select 1 as n"}'
arc call 'exec+arc://npub1.../' '{"argv":["uname","-a"]}'
```

The provider is a key, an npub, an installed name, or a domain for NIP-05.
`arc call` shows the reply as the manifest of the service says: exec shows the
output and exits with the code of the command, and sqlite shows a table.
`--raw` writes the reply as it came. See
[addresses](docs/interface/SPEC.md#141-addresses). With a relay, the call
is live. With `--later`, or with no relay, it travels like a message, and
`arc call results` shows the reply.

Before a live call, `arc` runs the wake hook of the provider from
`~/.config/arc/wake.toml`. Without a hook, the provider needs a current
announcement. See [the exec provider](cmd/exec-provider/README.md).

`arc lists add <command> <name> <citizen>...` saves a set of citizens. Where
the command takes a key, the name of the list runs it once for each member.

## Serve a capability

```bash
arc serve "exec://$(command -v exec-provider)?manifest=$PWD/cmd/exec-provider/manifest.json"
```

`arc serve` announces the capability, answers live calls through each relay,
and answers carried calls on each sync. It signs the announcement again
every 2 minutes. `arc apps init` writes a new provider bundle.

## Run a relay

```bash
arc relay serve --listen 127.0.0.1:7447
```

The relay is a khatru relay. It serves NIP-42 authentication, NIP-77 sync,
and sealed data only to its author. Flags turn on write limits: an event
size cap, authentication or proof of work for gift wraps, a rate for each IP
address, and a store cap. `--group <id>` hosts a NIP-29 group. See
[Deploy](docs/DEPLOY.md) for the public relay on Fly.io.

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
`citizen.env` of the exec provider names `ARC_HOME`, change it, or run
`citizen/init` again.

## Development

ARC is written in Go. [mise](https://mise.jdx.dev) installs the toolchain
from `mise.toml`.

```bash
mise install
mise run build       # every command into bin/
mise run check       # lint and test
mise run delivery    # the delivery layer, end to end
mise run interface   # the capability interface, end to end
```

`mise run build` writes `bin/arc` and one binary for each other command.

## Docs

- [Delivery layer](docs/delivery/SPEC.md): events, transports, sync, calls,
  and the switchover.
- [Capability interface](docs/interface/SPEC.md): manifests, commands, and
  data that a citizen keeps for itself.
- [Updates](docs/updates/SPEC.md): signed release channels.
- [Deploy](docs/DEPLOY.md): releases, relays, Docker, Fly.io.
- [Whitepaper](docs/WHITEPAPER.md): protocol design.
- [Changelog](CHANGELOG.md).
