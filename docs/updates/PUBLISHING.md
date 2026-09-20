# Publishing signed channels

The publisher, the provider and the relay have separate identities. The
publisher signs metadata on the operator's machine. The provider receives
public metadata and archives only. A citizen pins the publisher key
separately from its provider and relay pins. Never mount the signing key
store into the provider.

This is an operator-driven path, not an official public channel.

## The tools do not exist yet

Two Go commands are missing. The Elixir tools that did this work went with
the Elixir tree, and nothing replaced them:

- **A publisher.** It must take an unsigned manifest and a key, verify the
  size and hash of every archive, sign the document with `release.Sign`, and
  replace the channel file under a lock. `release.Sign` exists. The command
  does not.
- **A verifier.** It must read the channel through a relay as a throwaway
  citizen, verify publisher authority, download each archive, and check its
  signed size and digest without installing anything. `release.Verify`,
  `release.FetchChannel` and `release.Download` exist. The command does not.

The rest of this document is the policy that those commands must follow, and
the shape of the document they must produce.

## Prepare a publication

Generate a dedicated identity with `arc keys gen`. Keep its printed name and
public key. Do not change the active citizen identity. Keep the private key
in the host key store, and name it explicitly when signing.

Create `channels/` and `blobs/` under a provider root that the operator owns.
Download the release archives and check their published checksums. Place each
archive at `blobs/<sha256>.tar.gz`. Keep every published blob. Never change
one in place.

The unsigned document holds `schema_version`, `channel`, `publisher`,
`sequence`, `expires_at` and `releases`. Each release records its version, an
immutable build identifier, the Go toolchain that built it, the platform, the
size and SHA-256 of its archive, and its `install` object:

- A release that carries an archive sets `restart_required: true`,
  `sources: []`, and `eligible: true`. Its `install` object repeats the
  `sha256` and `size` of the release.
- A release with no `install` object must set `eligible: false`. `arc update`
  reports such a release, and cannot install it.
- Schema `2` signs under the domain `ARC-RELEASE-CHANNEL-V2` followed by one
  zero byte. Schema `1` keeps its own bytes. Clients through v0.4.1 refuse
  schema `2`, so those need one bootstrap update first.

## Rules that publication must keep

Verify the size and hash of every archive before replacing the channel file.
Refuse an expired document, a different publisher, a sequence that does not
increase, a build that changed or disappeared, a symbolic link, and a
publication already in progress. Withdraw an old build with
`withdrawn: true`. Never remove it.

Renew before `expires_at` by signing the same releases with a higher sequence
and a new expiry. Expiry stops acceptance. Nothing renews by itself, and
nothing signs a release by itself.

The provider root must not be writable by others. A local demonstration
channel is not a public trust root, and must never quietly become the
official publisher.

## Run the local provider

The standard stack stays as it is. Build the provider image, then start the
release provider against the existing relay:

```sh
export ARC_LOCAL_PORT=17331
export ARC_RELEASES_ROOT=/absolute/provider-root
export ARC_LOCAL_IMAGE=arc-local-release-provider:0.7.0
docker compose build relay
docker compose -f compose.yaml -f docker/local/compose.releases.yaml \
  --profile releases up -d --no-deps --wait releases
```

The release directory is mounted read-only. The provider identity lives in
its own named volume. Find the provider with `arc discover`. It publishes the
scheme `releases` and the resource `/releases`.

## A public channel needs more

For a public channel, provide a reachable provider and relay, distribute the
verified publisher pin in the client bootstrap, and establish release
approval and expiry renewal. A local proof does none of that.
