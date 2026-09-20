# Updating an ARC installation

`arc update` replaces the program on this machine with one that a publisher
signed. It reads the channel through the configured relay, from a citizen
that serves releases.

ARC does not hot-load code. A running service keeps the program it started
with until you restart it.

For the policy behind these commands, see [SPEC.md](SPEC.md).

## The commands

```sh
arc update                       # read the channel, and say what it would do
arc update check                 # the same, said plainly
arc update apply                 # download, verify, and replace the program
```

Each command takes:

| Flag | Meaning |
| --- | --- |
| `--provider` | The citizen that serves the releases, by public key |
| `--publisher` | The public key that signs the channel |
| `--channel` | The channel to read. The default is `stable` |
| `--program` | The program to replace. The default is this one |

The relay comes from `arc join`, from `ARC_RELAY`, or from `--relay`. The
identity comes from `arc keys use`, from `ARC_KEY`, or from `--key`.

## What one run does

1. Connects to the relay as the active citizen, the way `arc call` does.
2. Asks the provider for the channel document.
3. Verifies the document: the publisher signed it, the signature covers the
   canonical bytes under a domain that names the schema, the channel is the
   one that was asked for, the document has not expired, and its sequence
   does not go backwards.
4. Selects the newest release for this operating system and CPU that carries
   an archive and is eligible. A lower version is never installed.
5. Reports what it found. `apply` continues.
6. Downloads the archive through the relay in bounded chunks, and checks its
   length and its SHA-256.
7. Reads one program out of the archive. A member that is not a regular file,
   or whose path escapes its directory, is refused.
8. Writes the candidate beside the target, runs it once to prove that it
   starts and reports the expected version, and only then renames it over the
   target. The old program becomes `<name>.previous`.

## What one machine remembers

`~/.config/arc/update/<publisher>/<channel>.json` holds the highest sequence
that this machine accepted, and the digest of that document.

A publisher, a provider, or anyone between them could otherwise serve an
older document and hold the citizen on an old release. The signature of an
old document still verifies, because it was real once. The sequence is what
refuses it.

Each publisher and each channel holds its own record. A record that cannot be
read is an error, not an empty record: the update stops, and the operator
looks. To start again, remove the file.

## Publishing an archive

A release archive is a tarball that the release workflow builds:
`arc-<version>-<os>-<arch>.tar.gz`, with every member under `arc/`. To offer
one through the [release provider](../../cmd/releases-provider/README.md),
store it as `blobs/<sha256>.tar.gz` and name it in the signed channel
document:

```json
{
  "version": "0.7.0",
  "build": "BUILD_ID",
  "runtime": "go1.27.1",
  "platform": {"os": "linux", "arch": "amd64"},
  "size": 41234567,
  "sha256": "ARCHIVE_SHA256",
  "sources": [],
  "restart_required": true,
  "withdrawn": false,
  "eligible": true,
  "install": {"size": 41234567, "sha256": "ARCHIVE_SHA256"}
}
```

A release without `install` is reported as the latest of the channel, and
cannot be installed.

Signing needs the publisher key. `release.Sign` in the `release` package
signs a document. **There is no command that publishes a channel.** The
Elixir tool that did this went with the Elixir tree. Write a Go one before
you publish.

## After an update

Restart each service that runs the old program. `arc status` reports the
relay that answers now, not the program on disk. To see the program, run
`arc version`.

If the new program does not start, the old one is still there as
`<name>.previous`. Move it back.
