# Releases app

The [manifest](manifest.json) generates caller commands. `server/` owns
release lookup and archive streaming. `cmd/arc-releases` is the executable
entry point.

`releases` is a read-only service for ARC runtime update channels. It serves
operator-published, already-signed channel documents and immutable archives; it
does not sign, promote, install, or modify a release.

[Updates and release channels](../../docs/updates/SPEC.md) defines the signed
channel document and how `arc update` uses this service. `arc update` keeps
calling the releases provider itself, because it replaces `arc` and verifies
what it installs. [Publish a channel](#publish-a-channel) below describes
signing and versioned restart-only metadata.

## Layout

Set `RELEASES_ROOT` to a pre-existing directory with this exact layout:

```text
RELEASES_ROOT/
  channels/
    stable.json
    beta.json
  blobs/
    <lowercase-sha256>.tar.gz
```

The service reads only regular files. It refuses symlinked channel files,
symlinked archive files, non-hex digests, and any filename outside these fixed
locations. Channel documents are at most `262144` bytes. Archive chunks are at
most `262144` raw bytes. `arc update` asks for chunks of 64 KiB, so that one
reply fits in an event of 256 KiB on a relay.

Serve the service with `arc serve`. The identity of the service must
have a relay:

```sh
RELEASES_ROOT=/absolute/service-root arc serve \
  "exec://$(command -v arc-releases)?manifest=$PWD/apps/releases/manifest.json"
```

A citizen reads the channel with `arc update check --provider <service-key>
--publisher <publisher-key>`.

The service is public and read-only. Publication authority stays with the
separate release-manifest signer; the updater verifies that signature
before treating a channel document as trustworthy.

Blobs serve two kinds of archive under the same digest naming: hot-update
packages for managed relays, and complete installation tarballs that a
release's optional `install` object references for `arc update`. Both are
looked up only by their lower-case SHA-256.

## Publish a channel

The publisher, the provider and the relay have separate identities. The
publisher signs metadata on the operator's machine. The provider receives
public metadata and archives only. A citizen pins the publisher key
separately from its provider and relay pins. Never mount the signing key
store into the provider.

This is an operator-driven path, not an official public channel.

### The tools

- **The publisher** is `arc release sign`. It reads an unsigned channel,
  checks the size and hash of each archive in `<root>/blobs`, and signs the
  channel with the chosen identity. It refuses a channel whose publisher
  differs from the served channel, or whose sequence does not increase. It
  also refuses a channel file that is a link. It then replaces
  `<root>/channels/<channel>.json` whole.
- **The verifier does not exist yet.** It must read the channel through a
  relay as a throwaway citizen, verify publisher authority, download each
  archive, and check its signed size and digest without installing
  anything. `arc update check` does the first part.

### Prepare a publication

Generate a dedicated identity with `arc keys gen`. Keep its printed name
and public key. Do not change the active citizen identity. Keep the secret
key on this machine: a remote signer cannot sign a release yet. Name the
identity with `--key` when you sign:

```sh
arc --key <publisher-name> release sign --root /absolute/provider-root unsigned.json
```

Create `channels/` and `blobs/` under a provider root that the operator owns.
Download the release archives and check their published checksums. Place each
archive at `blobs/<sha256>.tar.gz`. Keep every published blob. Never change
one in place.

A release archive is a tarball that the release workflow builds:
`arc-<version>-<os>-<arch>.tar.gz`, with every member under `arc/`. To offer
one through this service, store it as `blobs/<sha256>.tar.gz` and name it in
the signed channel document, as section 3.1 of the updates spec shows.

The unsigned document holds `schema_version`, `channel`, `publisher`,
`sequence`, `expires_at` and `releases`. Each release records its version, an
immutable build identifier, the Go toolchain that built it, the platform, the
size and SHA-256 of its archive, and its `install` object:

- A release that carries an archive sets `restart_required: true`,
  `sources: []`, and `eligible: true`. Its `install` object repeats the
  `sha256` and `size` of the release.
- A release with no `install` object must set `eligible: false`. `arc update`
  reports such a release, and cannot install it.
- Set `schema_version` to `3`, and `publisher` to the Nostr public key of the
  publisher, as hex. Schema `3` signs under the domain
  `ARC-RELEASE-CHANNEL-V3` followed by one zero byte, with BIP-340 Schnorr.
  A client of v0.9.0 or older reads only schemas `1` and `2`, so it must
  install a newer release with `install.sh` first.

Signing needs the publisher key, a Nostr key. `arc release sign` checks the
archives, signs the document, and writes it for the provider.

### Rules that publication must keep

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

### Run the provider

Serve the provider with `arc serve`, with its own identity. The identity
must have a relay. Never give it the key of the publisher:

```sh
RELEASES_ROOT=/absolute/provider-root arc --key <provider-name> serve \
  "exec://$(command -v arc-releases)?manifest=$PWD/apps/releases/manifest.json"
```

Find the provider with `arc discover releases`. A citizen reads the channel
with `arc update check --provider <provider-key> --publisher <publisher-key>`.

### A public channel needs more

For a public channel, provide a reachable provider and relay, distribute the
verified publisher pin in the client bootstrap, and establish release
approval and expiry renewal. A local proof does none of that.

## Request protocol

A request is a live call to the capability `releases`, with the method `RAW`
and the path `/releases`.

Each ARC request body is UTF-8 JSON. Replies are UTF-8 JSON, except channel
requests, which return the exact bytes of the channel JSON file.

Fetch a channel:

```json
{"op":"channel","channel":"stable"}
```

Fetch an archive chunk:

```json
{"op":"chunk","digest":"sha256:<64 lowercase hex>","offset":0,"length":262144}
```

The chunk reply is:

```json
{"digest":"sha256:<64 lowercase hex>","offset":0,"data":"base64 bytes"}
```

`offset` is exact. An offset equal to the archive length returns an empty data
string. Other bad offsets, lengths, paths, and malformed JSON fail as
`invalid_request`. Missing artifacts fail as `not_found`.

## Tests

From the repository root:

```sh
go test ./apps/releases/server
```


## Streaming archives

`manifest.json` declares `server_stream`. An initial request
`{"op":"archive","digest":"sha256:<hex>"}` streams the raw archive bytes.
The same fixed-path, regular-file and symlink checks used by chunk requests apply.
The stream reads the file size captured when opened. Cancellation releases it.

```sh
arc session --mode server_stream 'releases+arc://<service>/releases' \
  '{"op":"archive","digest":"sha256:<64 lowercase hex>"}' > archive.tar.gz
```

Check the command's exit status before using the output file: a failed session
may leave a partial file. Validate the archive against its signed channel metadata.

After the consumer installs a releases service that declares streaming,
`arc update apply` selects this mode automatically. It checks exact byte count,
SHA-256 and the final core session outcome before unpacking. It retains the
archive in memory for the existing installer. Services without an installed
streaming declaration use the existing chunk path, selected before submission;
a failed stream never silently falls back or resumes. Channel documents remain
ordinary request/reply operations and still require publisher verification.
