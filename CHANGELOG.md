# Changelog

All notable changes to ARC are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/). Versions follow
[Semantic Versioning](https://semver.org/).

## [0.2.1] - 2026-09-15

### Added

- Installable releases: a mix release with the bundled Erlang runtime, one
  tarball per platform (linux-x86_64, linux-aarch64, darwin-aarch64), and a
  relay image on ghcr.io. A `vX.Y.Z` tag builds and publishes them. See
  `docs/DEPLOY.md`.

### Fixed

- `arc relay` no longer crashes in a release for lack of `:crypto`, and no
  longer prints an EXIT trace on SIGTERM.

- `arc dm fetch` rejects a blob name that could leave the message's blob
  directory. Attachment names never start with a dot.
- A `send` that fails part way, receipts included, removes every copy that
  landed and rebuilds each counter from disk.
- The mailbox byte counter rebuilds itself from a walk when the `usage`
  file is missing or malformed, never goes negative, and subtracts only
  bytes that actually left the disk.
- `retract` completes when a recipient already purged their copy, and emits
  `dm.retracted`.
- `thread` takes a conversation key as `conversations` prints it. A sender
  who lists themselves in `--to` keeps a plain one-to-one key.
- Events carry `to`, the conversation key the recipient can open.

## [0.2.0] - 2026-09-14

### Added

- `Arc.Identity.SealedBox`: seal a body to an X25519 public key with an
  ephemeral key, HKDF-SHA256, and ChaCha20-Poly1305. No session needed.
- `Arc.Identity.HKDF`: one RFC 5869 implementation for every derivation.
- Template filters `{{key|pubkey}}` and `{{key|seal:to}}`, a `seal_to`
  option on stdin inputs, and an `open` output filter that decrypts
  `sealed-v1:` tokens in a reply before the CLI prints it.
- `arc resolve` prints the peer's X25519 key. `arc publish` publishes it.
- `providers/dm`: sealed direct messages with a persistent inbox, receipts,
  threads, and a block list. Spec in `docs/dm/SPEC.md`.
- `arc version`.
- This changelog.

### Changed

- Session v2. The initiator derives the session key from a per-session
  ephemeral X25519 key, carried in the signed packet header as `ek`. This
  gives forward secrecy against compromise of the initiator's long-term
  key. The whitepaper now states exactly that.
- The replay guard evicts entries older than twice the clock skew window.

### Fixed

- A second `arc publish` no longer clears a published X25519 key.
- The test suite no longer writes to the real `~/.config/arc`.

### Deprecated

- Session v1 packets, those without `ek`, are still accepted and logged.
  The next release removes them.

## [0.1.0] - 2026-09-11

Initial release: identities, control plane, relay, exec providers, the
journal provider, MCP bridge.
