# Deploy Arc

Arc ships as one release per platform. Each release holds one static binary
for each command, so the target machine needs no runtime and no library.
`arc` is the program: it is the client, and `arc relay serve` runs a relay.
Each provider has its own binary.

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

`arc relay serve` runs a Nostr relay, built on khatru. It keeps its events
in `<home>/relay.db`, or in the file that `--db` names:

```bash
arc relay serve --listen 0.0.0.0:7447
```

Clients use `ws://<host>:7447`. Put a proxy that ends TLS in front of the
relay, and clients use `wss://`. The write limits are off until their flags
turn them on, see "Run the Nostr relay on Fly.io" below.

Example systemd unit at `/etc/systemd/system/arc-relay.service`:

```ini
[Unit]
Description=Arc relay
After=network-online.target
Wants=network-online.target

[Service]
User=arc
Environment=HOME=/var/lib/arc
ExecStart=/opt/arc/bin/arc relay serve --listen 0.0.0.0:7447
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
```

Create the `arc` user with `/var/lib/arc` as its home, then
`systemctl enable --now arc-relay`. The relay keeps its events in
`/var/lib/arc/.config/arc/relay.db`.

## Run a relay with Docker

The image `ghcr.io/gezibash/arc` runs `arc relay serve` on port 7447 by
default, with no write limits. It supports `linux/amd64` and `linux/arm64`.
Keep the events in a volume:

```bash
docker volume create arc-relay
docker run -d --name arc-relay -p 7447:7447 \
  -v arc-relay:/home/arc/.config/arc \
  ghcr.io/gezibash/arc:latest
```

To turn on write limits, name the command and its flags, as
`docker/fly-nostr/Dockerfile` does. Any other command works through the same
image, for example `docker run --rm ghcr.io/gezibash/arc:latest arc version`.

## Run the Nostr relay on Fly.io

The delivery layer uses Nostr relays, see docs/delivery/SPEC.md. The Fly.io
app `arc-nostr-gezim` runs `arc relay serve`. Its files are in
`docker/fly-nostr/`. The relay keeps its events in
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

The Dockerfile turns on the write limits of `arc relay serve`. Each limit
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

`arc sync` sends each event on its own connection. The burst lets a first
sync of up to 1000 events through at once. After the burst, a sync of 5
events each second does not reach the rate.

A client can write any `X-Forwarded-For` header. The relay therefore reads
only the header that `--ip-header` names. The Fly proxy sets `Fly-Client-IP`.
If the header is absent, the relay uses the address of the connection.

The store cap counts the pages that the store uses. A deletion frees pages,
and the store uses them again, but the file does not shrink. The relay takes
a deletion, kind 5, when the store is full.

## Connect clients

Add the relay to each identity that uses it:

```bash
arc keys gen
arc relay add wss://relay.example.com
arc relay ls
```

`arc relay ls` shows the NIP-11 document of each relay. `arc message send`,
`arc call` and `arc sync` then use the relay.

## Cut a release

1. Move the entries under `[Unreleased]` in `CHANGELOG.md` to the new version,
   and update the installation examples.
2. Commit, then tag and push:

   ```bash
   git tag vX.Y.Z
   git push origin vX.Y.Z
   ```

The tag carries the version, and the build writes it into each binary. The
`Release` workflow refuses a tag that is not `vX.Y.Z`. It cross-compiles the
three tarballs, pushes the image, and creates the GitHub release with
checksums.
