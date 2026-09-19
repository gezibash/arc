# Publishing signed channels

The publisher, provider and relay have separate identities. The publisher signs
metadata on the operator's machine. The provider receives only public metadata
and archives. Citizens pin the publisher public key independently of their
provider and relay pins. Never mount the signing keystore into the provider.

This is an operator-driven publication path, not an official public channel.
`arc update` installs a full release archive that a release names in its
`install` object. See
[publishing a complete archive](OPERATIONS.md#publishing-a-complete-archive).
`arc update apply` still applies only qualified hot edges on a prepared managed
relay. Full release archives are never passed to the hot-upgrade engine.

## Prepare a publication

Generate a dedicated identity with `arc keys gen`; retain its printed name and
public key. Do not change the active citizen identity. Keep the private key in
the existing host keystore. The publisher script requires `--key` explicitly.

Create `channels/` and `blobs/` under an operator-owned provider root. Download
release archives and verify their published checksums. Place each archive at
`blobs/<sha256>.tar.gz`. Retain published blobs; never modify them in place.

Prepare a complete unsigned manifest following `Arc.CLI.Update.Manifest`:

- `schema_version`, `channel`, `publisher`, `sequence`, `expires_at`, `releases`.
- Each release records its actual version, immutable build identifier, ERTS
  runtime from `releases/start_erl.data`, platform, archive size and SHA-256.
- A release with only a full release archive uses schema `2`,
  `restart_required: true`, and `sources: []`. Its `install` object must repeat
  the release `sha256` and `size`. Set `eligible: true` to let `arc update`
  install it. Do not invent live-upgrade plan hashes.
- If such a release has no `install` object, it must set `eligible: false`.
  Channels published before `install` existed use this form. `arc update`
  reports the release but cannot install it.
- A release with a hot package and a full release archive names the full
  archive in `install`. Place both archives in `blobs/`.
- Version `1` remains supported without changing its signing bytes. Version `2`
  has the separate signature domain `ARC-RELEASE-CHANNEL-V2` plus a zero byte.
  Released clients through v0.4.1 reject version `2`; a bootstrap update is needed.
- Prepared live-upgrade artifacts still require the exact authored source plans.

Publish from the repository:

```sh
mise exec -- mix run scripts/publish-release-channel.exs \
  --root /absolute/provider-root \
  --manifest /absolute/unsigned.json --key PUBLISHER_KEY_NAME
```

Publication verifies the size and hash of every archive, including each
`install` archive, before replacing the channel file. It refuses an expired document, different publisher, non-increasing
sequence, changed or removed build, symlinked leaf, or an existing publication
lock. Withdraw an old build with `withdrawn: true` instead of removing it.
A crash may leave `.publish.lock`; confirm no publisher is running before
removing that exact lock. A failure after rename can leave a published channel;
inspect its sequence before retrying. The root must not be writable by others.

Renew before `expires_at` by signing the same releases with an increased
sequence and a new expiry. Expiry stops acceptance; there is no automatic
renewal or automatic release signing. A local demonstration channel is not a
public trust root, and must not silently become the official publisher.

## Run the optional local provider

The standard stack stays unchanged. Build a separate provider image, then start
only the release provider against the existing relay:

```sh
export ARC_LOCAL_PORT=17331
export ARC_RELEASES_ROOT=/absolute/provider-root
export ARC_LOCAL_IMAGE=arc-local-release-provider:0.5.1
docker compose build relay
docker compose -f compose.yaml -f docker/local/compose.releases.yaml \
  --profile releases up -d --no-deps --wait releases
```

The release directory is mounted read-only. Provider identity lives in its own
named volume. Discover the provider using `arc discover`; it publishes scheme
`releases` and resource `/releases`.

## Verify through the relay

This uses an ephemeral citizen, verifies publisher authority, and downloads each
archive through ARC before checking its signed size and digest. It does not
install or restart anything. The output directory must not exist. This is a
first-contact publication proof; managed services additionally retain their
highest accepted sequence to reject replay across checks.

```sh
mise exec -- mix run scripts/verify-release-channel.exs \
  --relay 127.0.0.1:17331 --relay-pubkey RELAY_PUBLIC_KEY \
  --provider PROVIDER_PUBLIC_KEY --publisher PUBLISHER_PUBLIC_KEY \
  --channel stable --output /absolute/new-verification-directory
```

For a public channel, separately provision a reachable provider and relay,
distribute the verified publisher pin in the client bootstrap, and establish
release approval and expiry renewal. The current local proof does not do that.
