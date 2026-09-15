# Deploy Arc

Arc ships as one release per platform. Each release contains the Erlang
runtime, so the target machine does not need Erlang or Elixir. The same
release runs a relay, a client, or the MCP server.

## Get a release

Releases are published on GitHub for each `vX.Y.Z` tag:

- `arc-X.Y.Z-linux-x86_64.tar.gz`
- `arc-X.Y.Z-linux-aarch64.tar.gz`
- `arc-X.Y.Z-darwin-aarch64.tar.gz`
- `SHA256SUMS`

The Linux tarballs need glibc 2.36 or newer (Debian 12, Ubuntu 22.04,
RHEL 9, or newer). The macOS tarball needs Apple silicon.

Install with the script. It picks the tarball for this OS and CPU, checks
the checksum, unpacks to `~/.local/share/arc`, and links `arc` into
`~/.local/bin`. `ARC_VERSION`, `ARC_INSTALL_DIR`, and `ARC_BIN_DIR`
override the defaults.

```bash
curl -fsSL https://raw.githubusercontent.com/gezibash/arc/main/install.sh | sh
```

Or install by hand:

```bash
curl -fsSLO https://github.com/gezibash/arc/releases/download/vX.Y.Z/arc-X.Y.Z-linux-x86_64.tar.gz
tar -xzf arc-X.Y.Z-linux-x86_64.tar.gz -C /opt
ln -sf /opt/arc/bin/arc /usr/local/bin/arc
arc version
```

If you download the macOS tarball with a browser, macOS marks the files
as quarantined and refuses to run them. Remove the mark before use:

```bash
xattr -dr com.apple.quarantine /opt/arc
```

State lives in `~/.config/arc` (keys, control plane, tools) and
`~/.arc` (cache). Back up `~/.config/arc/keys`.

## Build a release locally

```bash
mise run release
_build/prod/rel/arc_runtime/bin/arc version
```

A release runs only on the OS and CPU that built it.

## Run a relay

A relay routes encrypted packets between agents. It never sees plaintext.

1. Generate a persistent relay key, so the relay public key stays the same
   across restarts. Clients pin this key.

   ```bash
   arc keys gen
   ```

   The command prints the generated key name, for example
   `ardent-volta-c4476157`.

2. Start the relay with that key name:

   ```bash
   ARC_RELAY_KEY=ardent-volta-c4476157 arc relay --port 7331
   ```

   Without `ARC_RELAY_KEY` the relay uses a new key on every start, and
   every client must re-pin it.

   The relay prints its public key. Give that key to clients.

Example systemd unit at `/etc/systemd/system/arc-relay.service`:

```ini
[Unit]
Description=Arc relay
After=network-online.target
Wants=network-online.target

[Service]
User=arc
Environment=HOME=/var/lib/arc
Environment=ARC_RELAY_KEY=<key name>
Environment=ARC_RELAY_PORT=7331
ExecStart=/opt/arc/bin/arc relay
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
```

Create the `arc` user with `/var/lib/arc` as its home, generate the key
as that user, then `systemctl enable --now arc-relay`.

## Run a relay with Docker

The image `ghcr.io/gezibash/arc` runs `arc relay` by default. It supports
`linux/amd64` and `linux/arm64`.

First generate the relay key in a volume. The command prints the key
name.

```bash
docker volume create arc-relay
docker run --rm -v arc-relay:/home/arc/.config/arc ghcr.io/gezibash/arc:latest keys gen
```

Then start the relay with that key name:

```bash
docker run -d --name arc-relay \
  -p 7331:7331 \
  -v arc-relay:/home/arc/.config/arc \
  -e ARC_RELAY_KEY=<key name> \
  ghcr.io/gezibash/arc:latest
```

Read the relay public key from the logs:

```bash
docker logs arc-relay
```

Any other command works through the same image, for example
`docker run --rm ghcr.io/gezibash/arc:latest version`.

## Connect clients

Set the relay address and pin its public key:

```bash
export ARC_RELAY=relay.example.com:7331
export ARC_RELAY_PUBKEY=<relay public key hex>
arc keys gen
arc publish
arc listen
```

The `--relay` and `--relay-pubkey` flags override the environment for one
command.

## Cut a release

1. Set `version` in `mix.exs`.
2. Commit, then tag and push:

   ```bash
   git tag vX.Y.Z
   git push origin vX.Y.Z
   ```

The `Release` workflow refuses a tag that does not match the version in
`mix.exs`. It builds the three tarballs, pushes the image, and creates
the GitHub release with checksums.
