# ARC Releases provider

`releases` is a read-only provider for ARC runtime update channels. It serves
operator-published, already-signed channel documents and immutable archives; it
does not sign, promote, install, or modify a release.

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
most `262144` raw bytes.

Serve the bundle with the normal ARC provider flow:

```sh
bin/arc serve providers/releases --relay relay.example:7331 --relay-pubkey <relay-key>
```

The provider is public and read-only. Publication authority stays with the
separate release-manifest signer; the updater verifies that signature
before treating a channel document as trustworthy.

Blobs serve two kinds of archive under the same digest naming: hot-update
packages for managed relays, and complete installation tarballs that a
release's optional `install` object references for `arc update`. Both are
looked up only by their lower-case SHA-256.

## Request protocol

The capability address has the form:

```text
releases+arc://<provider-public-key>/releases
```

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

## Local source and staging client

`Arc.CLI.Update.Source` consumes either a relay-backed source:

```elixir
{:arc, citizen_agent, "releases+arc://<provider-key>/releases"}
```

or an explicitly selected offline directory:

```elixir
{:local, "/absolute/release-root"}
```

`channel/3` bounds metadata to `262144` bytes. `stage_archive/5` consumes a
known lower-case digest and byte length, writes chunks into a caller-prepared
staging directory, checks the final SHA-256, and creates the final filename only
when verification succeeds. Existing destinations are never overwritten. ARC
sources use `Arc.Data.Protocol` and therefore the citizen agent's configured
relay route; this helper has no HTTP transport, direct fallback, or implicit
local fallback.

## Tests

```sh
cd providers/releases
mise exec -- mix test

cd /Users/zim/Work/arc
mise exec -- mix test apps/arc_cli/test/arc/update_source_test.exs
mise exec -- mix test apps/arc_cli/test/arc/update_source_relay_test.exs
```
