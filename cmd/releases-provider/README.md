# ARC Releases provider

`releases` is a read-only provider for ARC runtime update channels. It serves
operator-published, already-signed channel documents and immutable archives; it
does not sign, promote, install, or modify a release.

See [publication instructions](../../docs/updates/PUBLISHING.md) for signing,
versioned restart-only metadata, and a local Docker proof.

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

The provider reads only regular files. It refuses symlinked channel files,
symlinked archive files, non-hex digests, and any filename outside these fixed
locations. Channel documents are at most `262144` bytes. Archive chunks are at
most `262144` raw bytes. `arc update` asks for chunks of 64 KiB, so that one
reply fits in an event of 256 KiB on a relay.

Serve the provider with `arc serve`. The identity of the provider must
have a relay:

```sh
RELEASES_ROOT=/absolute/provider-root arc serve \
  "exec://$(command -v releases-provider)?manifest=$PWD/cmd/releases-provider/manifest.json"
```

A citizen reads the channel with `arc update check --provider <provider-key>
--publisher <publisher-key>`.

The provider is public and read-only. Publication authority stays with the
separate release-manifest signer; the updater verifies that signature
before treating a channel document as trustworthy.

Blobs serve two kinds of archive under the same digest naming: hot-update
packages for managed relays, and complete installation tarballs that a
release's optional `install` object references for `arc update`. Both are
looked up only by their lower-case SHA-256.

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
go test ./cmd/releases-provider
```
