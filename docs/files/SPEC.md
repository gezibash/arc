# Private files: an ARC storage provider

## 1. Purpose and current scope

The `providers/files` bundle stores immutable encrypted files for ARC citizens.
The trusted ARC client encrypts both the basename and the raw file bytes to
the active citizen before invoking the provider. The provider stores signed
encrypted envelopes and never needs the citizen's secret key.

This is a storage service, not an execution environment. A workload is code
plus its permitted inputs and intended outputs, submitted to a compute
provider. Future workloads can use private storage as an input/output service,
but this initial provider has no sharing, compute grants, guest verification,
or automatiCommc key release to workloads.

It supports files up to 4 MiB, an operator-enforced 64 MiB stored-byte budget
per owner, and paginated listings. Every upload creates a new immutable
object; there is no overwrite, delete, or mutable "latest" pointer. Keep the
returned file ID to pin an exact object.

## 2. Running it from this checkout

The new local encryption and file-output handling requires CLI interface
version 3. Existing released clients that only understand version 2 must be
updated; use the source commands below while developing. Older clients reject
the manifest version before trying to render private-file inputs.

Start the provider with a dedicated identity using the existing ARC key
workflow. `<provider-key-name>` and `<provider-public-key>` below are values
from that identity, not literal names to copy.

```bash
mise run arc -- keys gen
ARC_KEY=<provider-key-name> mise run arc -- serve providers/files
```

`serve` reads the bundle's `Arcfile` and runs `run.sh`. The script builds the
standalone Elixir escript when needed. Use a separate terminal with the
citizen's own active identity to install and use the service:

```bash
mise run arc -- install <provider-public-key> primary
mise run arc -- files put ./report.pdf
mise run arc -- files list
mise run arc -- files get <returned-file-id> --output ./restored-report.pdf
```

Installation uses the existing provider trust prompt. These commands do not
require a new ARC command implementation for every provider: the manifest
defines the installed `files` command group. For machines on different hosts,
configure the same ARC relay connection and pinned relay key on each machine.
Updated clients and relays use [relay discovery](../discovery/SPEC.md); no
shared control directory is needed. Same-host development can use a local
relay or the existing unconfigured local mode.

`FILES_ROOT` selects the provider's storage directory; the default is
`~/.arc/files`. `FILES_QUOTA_BYTES` overrides the per-owner stored-byte budget
with a positive integer; the default is 64 MiB. The destination of `get` is an explicit client-side path.
Existing files and symlinks are refused; no server-supplied filename chooses
where a download is written. Saved files have owner-only permissions.

Private-file commands currently require the local CLI. The shared toolbox
rejects these inputs rather than passing local filenames through to a provider.
Mounted tools do not yet support this client's file handling.

## 3. Operations and access

| User command | Behavior |
| --- | --- |
| `files put <path>` | Encrypt the basename and bytes locally, sign their binding, upload the envelope, and verify the acknowledged ID |
| `files list [--after <id>]` | Retrieve up to 50 signed headers; verify them and decrypt names locally; show the continuation cursor when supplied |
| `files get <id> --output <path>` | Retrieve an owner-scoped object, verify signature and requested ID, check the encrypted body hash, decrypt locally, and write exact bytes |

The authenticated caller supplied by ARC determines the storage owner. An
owner field supplied inside a request cannot select another owner's directory.
Knowing an object ID does not grant access to another citizen's objects.

The provider serves requests through the existing newline-delimited JSON
runtime protocol. It rejects application streams. Requests, responses, and
error messages must never include plaintext filenames or file bytes. An
operator with control of the storage host can read its stored ciphertext and
traffic metadata, but does not hold the decryption key.

## 4. Encrypted envelope and object identity

The current envelope is a JSON object with these fields:

| Field | Meaning |
| --- | --- |
| `version` | Integer `1` |
| `owner` | Lowercase hex ARC public key |
| `name` | `sealed-v1:` token containing the basename, encrypted to the owner |
| `body` | `sealed-v1:` token containing the raw bytes, encrypted to the owner |
| `body_hash` | Lowercase SHA-256 hex digest of the ASCII `body` token |
| `signature` | Lowercase hex Ed25519 signature over the message below |
| `id` | Lowercase SHA-256 hex digest of the message plus signature below |

The signature message is the exact ASCII concatenation below, with no final
newline. Its fixed prefix separates this signature from other ARC messages.

```text
arc-private-file-v1\n<owner>\n<name-token>\n<body_hash>
```

The ID hashes that message followed by a newline and the lowercase signature
text. It does not depend on JSON whitespace or object field ordering. The
sealed tokens contain fresh random encryption material, so uploading the same
file again normally creates a different ID. Plaintext hashes and filenames
are not exposed to the provider.

The provider validates owner, token format, length limits, body hash, signature,
and ID before accepting an object. A get response is checked again by the
client against the requested ID and active identity. This detects substitution
and corruption. The signature also lets the client verify listing headers
without downloading every body.

## 5. Provider wire requests

These objects are encoded as the ARC runtime event's `message` string. They
are not shell commands. The trusted client constructs them; file and output
paths never enter these requests.

```json
{"op":"put","file":{"version":1,"owner":"...","name":"sealed-v1:...","body":"sealed-v1:...","body_hash":"...","signature":"...","id":"..."}}
{"op":"get","id":"..."}
{"op":"list"}
{"op":"list","after":"..."}
```

Responses are objects returned in the runtime's `reply` field:

```json
{"id":"..."}
{"file":{"version":1,"owner":"...","name":"sealed-v1:...","body":"sealed-v1:...","body_hash":"...","signature":"...","id":"..."}}
{"files":[{"version":1,"owner":"...","name":"sealed-v1:...","body_hash":"...","signature":"...","id":"..."}],"next":null}
```

Ellipses are illustrative placeholders, not valid envelopes. Listings are
sorted by object ID, not upload time. The continuation cursor is the last ID
of the page; absence of another cursor is JSON `null`. Pagination is a view
of currently stored immutable objects, not a signed complete inventory.

## 6. Stored layout and failure behavior

```text
FILES_ROOT/
  objects/
    <authenticated-owner-public-key>/
      <file-id>.json
```

The persisted JSON is the validated canonical encrypted envelope. Repeating
the same envelope is idempotent and does not consume additional quota.
Acceptance occurs after the object is fully written; partial writes must not
appear as successful immutable objects. Storage failures and corrupt objects
are surfaced, not silently removed from a successful listing.

The initial provider is a single runtime writer for a storage root. Operators
must not launch multiple independent writers against the same root: aggregate
quota accounting is not a distributed transaction. Host administrators remain
able to stop the service, delete encrypted objects, or omit objects from a
listing. Backups and replication are not implemented here.

## 7. Privacy boundaries

- Encryption happens in the citizen's trusted ARC client. A protected compute
  host is unnecessary for storing already encrypted files.
- The provider sees owner keys, object IDs, sizes, and access patterns. It can
  infer approximate file size. This is not anonymous storage.
- Signed objects establish authorship and integrity, not completeness of the
  provider's inventory or indefinite availability.
- The citizen's key is the recovery authority. Losing it makes these files
  unreadable; compromise can expose saved files. There is no provider-held
  recovery key or automatic key rotation.
- A provider manifest is executable configuration interpreted by the client.
  Use the reviewed, pinned private-file interface. Trusting an arbitrary
  replacement manifest can change what an installed command sends.
- The general private-environment contract's mutable state controller,
  anti-rollback head tracking, hardware verification, and delegated workload
  access remain future work. This store promises immutable objects by ID.

## 8. Implementation map and checks

| Location | Responsibility |
| --- | --- |
| [Provider bundle](../../providers/files/Arcfile) | Runtime launch recipe |
| [Manifest](../../providers/files/manifest.json) | Installed commands and private-file interface version |
| [Trusted client](../../apps/arc_cli/lib/arc/cli/private_file.ex) | Encryption, signatures, reply verification, safe binary output |
| [CLI integration](../../apps/arc_cli/lib/arc/cli/tools.ex) | Resolve the active identity and invoke the private-file renderer |
| [Interface normalization](../../apps/arc_data/lib/arc/data/interface_manifest.ex) | Preserve versioned private-file command descriptions |
| [Provider tests](../../providers/files/test/files_test.exs) | Storage access, signatures, limits, and protocol errors |
| [Client tests](../../apps/arc_cli/test/arc/private_file_test.exs) | Binary round trips, tamper detection, recipient isolation, and output-path protection |

Run the provider checks from its directory and the ARC integration checks from
the repository root:

```bash
cd providers/files
mix test
```

```bash
mix test apps/arc_cli/test/arc/private_file_test.exs apps/arc_cli/test/arc/cli_private_files_test.exs apps/arc_data/test/arc/data/interface_manifest_test.exs apps/arc_cli/test/arc/cli_tools_test.exs
```

The tests use generated identities and temporary data. A local round trip is
not a production deployment, hardware-protected execution test, or independent
cryptographic audit.
