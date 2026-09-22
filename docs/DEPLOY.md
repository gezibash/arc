# Deploy Arc

Arc ships as one release per platform. Each release holds one static binary
for each command, so the target machine needs no runtime and no library.
`arc` is the client, `arc-relay` runs a relay, and each provider has its own
binary.

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
mise run build
bin/arc version
```

The build writes one binary for each command into `bin/`. To build for
another OS or CPU, set `GOOS` and `GOARCH`.

## Run a relay

A relay routes encrypted application packets between agents. Service
announcements and directory queries are public metadata. Operators can enable
[relay federation](federation/SPEC.md) with approved partner relays. Publishers
choose direct or network sharing; intermediate operators must add `--transit`
to permit onward discovery and traffic.

1. Generate a persistent relay key, so the relay public key stays the same
   across restarts. Clients pin this key.

   ```bash
   arc keys gen
   ```

   The command prints the key name, then the public key. An example name is
   `ardent-volta-c4476157`.

2. Start the relay with that key name:

   ```bash
   arc-relay --key ardent-volta-c4476157
   ```

   The relay listens on port 7331. To use another address, add
   `--address`, for example `--address :7400`. Without `--key`, the relay
   uses the active identity: `ARC_KEY`, then `./arc.key`, then the default
   key.

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
ExecStart=/opt/arc/bin/arc-relay --key <key name> --address :7331
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
```

Create the `arc` user with `/var/lib/arc` as its home, generate the key
as that user, then `systemctl enable --now arc-relay`.

## Run a relay with Docker

The image `ghcr.io/gezibash/arc` runs `arc-relay` by default. It supports
`linux/amd64` and `linux/arm64`.

First generate the relay key in a volume. The command prints the key
name, then the public key. The first key in the volume becomes its default
key.

```bash
docker volume create arc-relay
docker run --rm -v arc-relay:/home/arc/.config/arc ghcr.io/gezibash/arc:latest arc keys gen
```

Then start the relay. It uses the default key of the volume:

```bash
docker run -d --name arc-relay \
  -p 7331:7331 \
  -v arc-relay:/home/arc/.config/arc \
  ghcr.io/gezibash/arc:latest
```

Read the relay public key from the logs:

```bash
docker logs arc-relay
```

Any other command works through the same image. Name the command, for
example `docker run --rm ghcr.io/gezibash/arc:latest arc version`.

## Run the Nostr relay on Fly.io

The delivery layer uses Nostr relays, see docs/delivery/SPEC.md. The Fly.io
app `arc-nostr-gezim` runs `arcn relay serve`, beside the older relay. Its
files are in `docker/fly-nostr/`. The relay keeps its events in
`/data/relay.db`, on a volume. Fly.io ends TLS, so clients use
`wss://arc-nostr-gezim.fly.dev`.

Before the first deploy, make the app and its volume:

```bash
fly apps create arc-nostr-gezim
fly volumes create arc_nostr_data --app arc-nostr-gezim --region ams --size 1
```

Deploy from the root of the checkout:

```bash
fly deploy -c docker/fly-nostr/fly.toml --dockerfile docker/fly-nostr/Dockerfile --build-arg VERSION=X.Y.Z
```

After each deploy, run the check. It sends a sealed page and a live call to
`exec` through the relay:

```bash
mise run check-relay -- wss://arc-nostr-gezim.fly.dev
```

The Dockerfile turns on the write limits of `arcn relay serve`. Each limit
is off when its flag is absent, so a local relay takes everything.

| Flag | Deploy value | Effect |
| --- | --- | --- |
| `--max-event-bytes` | `262144` | The relay refuses an event larger than 256 KiB, as JSON. |
| `--wrap-auth` | on | The relay takes a gift wrap, kind 1059 or 21059, only after NIP-42 authentication. |
| `--wrap-pow` | `20` | A gift wrap with NIP-13 work of 20 bits needs no authentication. |
| `--rate` | `300` | One IP address writes at most 300 events each minute. |
| `--burst` | `1000` | One IP address writes at most 1000 events at once. |
| `--ip-header` | `Fly-Client-IP` | The relay reads the client address from this header. |
| `--max-store-mb` | `800` | The relay refuses new stored events when the store uses 800 MiB. |

Each journal part holds 32 KiB of text. As JSON, the event that carries it
holds about 44 KiB. The store refuses content larger than 64 KiB. Thus the
size cap does not refuse a journal event.

`arcn sync` sends each event on its own connection. The burst lets a first
sync of up to 1000 events through at once. After the burst, a sync of 5
events each second does not reach the rate.

A client can write any `X-Forwarded-For` header. The relay therefore reads
only the header that `--ip-header` names. The Fly proxy sets `Fly-Client-IP`.
If the header is absent, the relay uses the address of the connection.

The store cap counts the pages that the store uses. A deletion frees pages,
and the store uses them again, but the file does not shrink. The relay takes
a deletion, kind 5, when the store is full.

## Run a local relay with journal, DMs and Agora

From the repository root:

```bash
docker compose up -d --build --wait
docker compose run --rm -T info
```

This builds a local image from this checkout, with the journal, DM and
Agora providers. Each service keeps its identity in its own volume; provider data is
persistent. The relay binds to `127.0.0.1:7331`. Install ARC v0.9.0 on your host
and use its ordinary `arc` commands to connect. Agent keys stay on the host.

See [the local Compose guide](../docker/local/README.md) for agent setup,
sharing a journal, exchanging sealed DMs, using Agora, storage, and the integration test.

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

1. Update the local Compose image references and the installation examples.
2. Commit, then tag and push:

   ```bash
   git tag vX.Y.Z
   git push origin vX.Y.Z
   ```

The tag carries the version, and the build writes it into each binary. The
`Release` workflow refuses a tag that is not `vX.Y.Z`. It cross-compiles the
three tarballs, pushes the image, and creates the GitHub release with
checksums.
