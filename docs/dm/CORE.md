# Core changes for DM

This document lists every change to ARC core that the DM provider needs.
The DM provider spec is `docs/dm/SPEC.md`. Each item names the module, the
current behaviour with a file reference, the required behaviour, and the
tests that prove it. Items are ordered by dependency. C1 to C3 block DM
Phase 1. C4, C6, and C7 ship in the same release. C5 reserves a frame type
for Phase 3.

The whitepaper (`docs/WHITEPAPER.md`) makes two commitments that bind this
work:

- "The storage layer sees only ciphertext." A mailbox that stores plaintext
  breaks this. The sealed box (C1) is therefore a Phase 1 requirement, not
  a later phase.
- "Forward secrecy: compromise of long-term keys does not expose past
  sessions." The session key is derived from long-term keys only. C4 makes
  the code match the claim.

## C1. Sealed box

**Module:** new `Arc.Identity.SealedBox` in `apps/arc_identity`.

**Now:** `Arc.Data.Session.establish/3` (`apps/arc_data/lib/arc/data/session.ex:35-56`)
is the only key agreement. It needs both parties' long-term keys and a live
session. HKDF and ChaCha20-Poly1305 are inlined there. No `seal`, `open`,
or `box` symbol exists anywhere in `apps/` or `providers/`.

**Required:**

```elixir
@spec seal(recipient_x25519_pub :: <<_::256>>, plaintext :: binary()) :: binary()
@spec open(Arc.Identity.t(), sealed :: binary()) :: {:ok, binary()} | {:error, :open_failed}
```

Construction, version 1:

1. Generate an ephemeral X25519 keypair `{ek_pub, ek_priv}`.
2. `shared = ECDH(ek_priv, recipient_x25519_pub)`.
3. `key = HKDF-SHA256(ikm: shared, salt: ek_pub <> recipient_x25519_pub, info: "arc-sealed-v1", len: 32)`.
4. `nonce = 12 zero bytes`. The key is single-use, so a fixed nonce is safe.
5. `ct = ChaCha20-Poly1305(key, nonce, plaintext, aad: ek_pub)`.
6. Output: `<<1, ek_pub::32, ct::binary>>`. Byte 1 is the format version.

`open/2` derives the recipient's X25519 private key with
`Arc.Identity.to_x25519/1`, recomputes `key`, and decrypts. Any failure
returns `{:error, :open_failed}`. The function must not distinguish a wrong
key from a corrupt body.

The HKDF extract and expand steps move to a new `Arc.Identity.HKDF` module
with `derive(ikm, salt, info, len)`. `Arc.Data.Session` must call it instead
of its inline copy. This is the only change to `Session` in C1. The derived
session key must not change: keep salt as 32 zero bytes and info
`"arc-session-v1"` for the session path.

**Tests** (`apps/arc_identity/test/arc/identity/sealed_box_test.exs`,
mirror the style of `session_test.exs`):

- Round trip: `open(id, seal(x_pub(id), msg)) == {:ok, msg}` for empty,
  1 byte, and 1 MiB bodies, and for a body with non-ASCII text.
- Wrong recipient returns `{:error, :open_failed}`.
- One flipped byte anywhere in the sealed blob returns `{:error, :open_failed}`.
- Two seals of the same plaintext produce different output.
- Version byte other than 1 returns `{:error, :open_failed}`.
- `Session` test suite still passes unchanged after the HKDF move.

## C2. Publish and resolve the X25519 key

**Module:** `Arc.CLI.Control` (`apps/arc_cli/lib/arc/cli/control.ex`),
`Arc.Control.Local` (`apps/arc_control/lib/arc/control/local.ex`).

**Now:**

- `arc publish` calls `Control.publish/1` only (`control.ex:10-23`). The
  X25519 key is published only by `Arc.Data.Agent` (`agent.ex:107-119`).
  A citizen who runs `arc publish` and never starts an agent has no keyex.
- `Control.Local.handle_call({:publish, ...})` writes `x25519_public: nil`
  (`local.ex:64-77`). A second `publish` clears a published keyex.
- `arc resolve` prints `keyex: published` or `none` (`control.ex:25-46`).
  The bytes are not printed. No caller can obtain a peer's X25519 key from
  the CLI.

**Required:**

- `arc publish` publishes the identity and the keyex in one command. If
  `publish_keyex` fails, the command fails and prints the error.
- `Control.publish/1` preserves an existing `x25519_public` when the entry
  already exists. Only `revoke` clears it.
- `arc resolve <query>` prints one more line: `x25519:     <64 hex>` when
  the key is published, and `x25519:     none` when it is not.
- `Control.resolve/1` is unchanged. The entry map already carries
  `x25519_public`.

**Tests:**

- `apps/arc_control/test/arc/control_test.exs`: publish, publish_keyex,
  publish again, resolve still returns the keyex.
- `apps/arc_cli/test/arc/cli_test.exs`: `arc publish` output contains
  `keyex: published`; `arc resolve` output contains the hex key.

## C3. Template filters that need the network and the identity

**Module:** `Arc.Data.Toolbox` (`apps/arc_data/lib/arc/data/toolbox.ex`),
`Arc.CLI.Tools` (`apps/arc_cli/lib/arc/cli/tools.ex`),
`Arc.Data.InterfaceManifest` (`apps/arc_data/lib/arc/data/interface_manifest.ex`).

**Now:**

- Filters are `json` and `shell` (`toolbox.ex:243`). The filter function
  receives `(key, values)` and nothing else (`toolbox.ex:302-306`). It
  cannot resolve a name or reach the caller's identity.
- The `stdin` input path in the CLI (`tools.ex:931-946`) appends the body
  after the rendered header. The body never passes through a filter.
- The CLI resolves the provider, not any argument, and only after the input
  is rendered (`tools.ex:618-641`, `capability_invocation.ex:35`).
- `print_reply/1` prints the reply text as is (`tools.ex:495-501`). There
  is no output hook.

**Required:**

1. **Filter context.** `Toolbox.render_template/3` takes a third argument,
   a context map:

   ```elixir
   %{resolve: (String.t() -> {:ok, entry} | {:error, term}), identity: Arc.Identity.t() | nil}
   ```

   `render_template/2` stays and passes an empty context. The CLI builds
   the context from `Arc.Control.resolve/1` and the active key.

2. **`pubkey` filter.** `{{to|pubkey}}` resolves the value with
   `context.resolve` and renders the Ed25519 public key as 64 hex
   characters. A value that is already 64 hex characters passes through.
   Resolution failure renders as a template error `{:error, {:resolve, value, reason}}`
   and the CLI sends nothing.

3. **`seal` filter with an argument.** The placeholder grammar gains an
   optional argument: `{{body|seal:to}}`. The regex at `toolbox.ex:242`
   becomes `~r/\{\{([a-zA-Z0-9_-]+)(?:\|([a-zA-Z0-9_]+)(?::([a-zA-Z0-9_-]+))?)?\}\}/`.
   `seal:to` resolves `values["to"]`, takes `entry.x25519_public`, seals
   the body with `Arc.Identity.SealedBox.seal/2`, and renders
   `sealed-v1:<base64>`. If the entry has no `x25519_public`, the render
   fails with `{:error, {:no_keyex, value}}`.

4. **Sealed stdin body.** The `stdin` input spec gains an optional
   `"seal_to"` key: one arg name, or a list of arg names. The literal
   `"me"` means the caller's own key. `InterfaceManifest.normalize_cli_input/2`
   accepts a string or a list and normalises to a list. When set, the CLI
   seals the stdin body once per entry and appends one `sealed-v1:` token
   per line, in list order, in place of the raw body. The header template
   still renders first. An entry that does not resolve, or has no keyex,
   fails the whole command before anything is sent.

5. **Output filter.** A CLI command spec gains an optional
   `"output": {"filter": "open"}` key. When set, `print_reply/1` scans each
   output line for the token `sealed-v1:<base64>`, opens it with the active
   identity, and prints the plaintext in its place. A token that fails to
   open prints as `[sealed: cannot open]`. Lines without the token print
   unchanged. Stream mode (`tools.ex:711-740`) is not affected.

The `seal` filter and `seal_to` are the only places the CLI touches the
plaintext. The provider sees `sealed-v1:` tokens only.

**Tests:**

- `toolbox_test.exs`: `pubkey` renders hex from a name through a stub
  resolver; `pubkey` passes hex through; `seal:to` output opens with the
  recipient's key; missing keyex fails; unknown filter still fails as
  today; the old two-argument `render_template/2` still passes.
- `interface_manifest_test.exs`: `seal_to` and `output.filter` normalise;
  unknown `output.filter` normalises to `nil`.
- `cli_tools_test.exs`: a stub manifest with `seal_to` sends a
  `sealed-v1:` body; the reply path opens a `sealed-v1:` token; a token
  sealed to a different key prints `[sealed: cannot open]`.

## C4. Forward secrecy in the session

**Module:** `Arc.Data.Session`, `Arc.Data.Packet`, `Arc.Data.Agent`.

**Now:** the session key is
`HKDF(ECDH(my_static_x25519, peer_static_x25519))` (`session.ex:35-56`).
Two peers derive the same key in every session for life. Compromise of
either long-term key exposes every past session. The whitepaper claims the
opposite.

**Required, version 2 of the session:**

1. The initiator generates an ephemeral X25519 keypair per session.
2. The packet header (`packet.ex`, header JSON) gains an `ek` field: the
   initiator's ephemeral public key, 32 bytes base64. The header is signed,
   so `ek` is authenticated.
3. Key: `HKDF(ikm: ECDH(ek_priv, peer_static) , salt: ek_pub, info: "arc-session-v2")`.
   The responder recomputes it from `ek` and its own static key.
4. The responder replies under the same session id and key. There is no
   second ephemeral in v2. Compromise of the initiator's long-term key does
   not expose past sessions. Compromise of the responder's does. The
   whitepaper text must say this until a v3 adds a responder ephemeral.
5. A packet without `ek` is a v1 packet. The agent accepts it for one
   release and logs a deprecation. The release after removes v1.

**Tests:**

- `session_test.exs`: two `establish` calls with the same peers produce
  different keys; a responder that receives `ek` derives the initiator's
  key; a v1 packet still decodes under the compatibility flag.
- `packet_test.exs`: header with `ek` round trips and verifies; a
  tampered `ek` fails signature check.

**Whitepaper:** replace the forward secrecy bullet at
`docs/WHITEPAPER.md:512` with the v2 statement above.

## C5. Push events to a connected client

**Module:** `Arc.Data.Frame`, `Arc.Data.Agent`, `Arc.CLI.Agent`.

**Now:** a handler can emit a frame to any public key through
`{:emit, events, state}` (`agent.ex:389-421`) and `handle_info/2`. A
client in request mode discards every frame except its reply
(`capability_invocation.ex:166-186`). `arc listen` prints unmatched frames
from the in-memory inbox (`cli/agent.ex:377-392`). No frame type means
"event".

**Required:**

- Frame type 10, `event`. Meta carries `{"topic": "<string>"}`. Body is
  free text.
- `arc listen` prints an event frame as `event <topic> from <name>: <body>`
  and passes the body through the `open` output filter when the body is a
  `sealed-v1:` token.
- `Arc.Data.Handler` gets a documented helper `emit_event(to_pk, topic, body)`
  that builds the type 10 frame. The DM provider calls it from `send`.
- Request mode stays as it is. An event that arrives during a request is
  appended to the inbox, not dropped.

This is DM Phase 3. It is listed here because the frame type must be
reserved now so no provider claims type 10 for something else.

**Tests:** `frame_test.exs` encodes and decodes type 10; `agent_test.exs`
emits an event from a handler and the observer receives it.

## C6. Version and changelog

**Now:** every `mix.exs` is `0.1.0`. There is no `CHANGELOG.md`. The CLI has
no `version` command (`cli.ex:61-77`).

**Required:**

- Bump the umbrella and all seven apps to `0.2.0` in one commit.
- Add `CHANGELOG.md` at the repo root. Keep a Changelog format. The `0.2.0`
  entry lists C1 to C7 and the DM provider.
- Add `arc version`. It prints `arc <umbrella version> (<git short sha>)`.
  The escript embeds the sha at build time with a `mix escript.build`
  alias in `apps/arc_cli/mix.exs`. If the sha is unavailable, print
  `unknown`.
- Add `version` to `@bare_commands` and to the help text.

**Tests:** `cli_test.exs` asserts `arc version` output matches
`~r/^arc 0\.2\.0 \(/`.

## C7. Replay guard memory

**Module:** `Arc.Data.Agent`.

**Now:** `replay_guard` keeps one entry per `{src, session_id}` for the
life of the process (`agent.ex:336-358`). A provider that serves many
short sessions grows without bound. `max_ts` is tracked and never used.

**Required:** evict an entry when `now - max_ts > 2 * allowed_clock_skew_ms`.
A packet outside the skew window is already rejected, so an evicted entry
cannot be replayed. Run the sweep on a timer every `allowed_clock_skew_ms`.

**Tests:** `agent_test.exs` inserts an old guard entry, advances the clock
through a test hook, and asserts the entry is gone.

## C8. X25519 key from the Ed25519 public key

**Module:** `Arc.Identity` (`apps/arc_identity/lib/arc/identity.ex`),
`Arc.Data.Toolbox`.

**Problem:** `seal` found the recipient's X25519 key only in the local control
directory. A sender on a different host had no entry for the recipient, so it
could not seal. The old `to_x25519/1` used the Ed25519 seed as the X25519
secret. Only the recipient could compute the matching public key.

**Required:**

- `to_x25519/1` uses the standard conversion. The X25519 secret is the
  clamped first 32 bytes of SHA-512 of the seed. libsodium calls this
  `crypto_sign_ed25519_sk_to_curve25519`.
- `public_key_to_x25519/1` computes the X25519 public key from the Ed25519
  public key with the birational map u = (1 + y) / (1 - y) mod p. libsodium
  calls this `crypto_sign_ed25519_pk_to_curve25519`.
- `public_key_to_x25519/1` rejects a key that maps to a point of small order,
  and a y that is not below the field prime.
- `seal:to` computes the recipient key from the resolved Ed25519 public key.
  It does not read `x25519_public` from the control plane entry. A full hex
  public key needs no entry.
- Sessions compute the X25519 key of the peer from its Ed25519 public key.
- The key exchange record is removed. `Control.publish_keyex/2` and the
  `x25519_public` field of a control plane entry do not exist. A relay
  announcement has no `x25519_public` field. `arc publish` and
  `arc resolve` print no key exchange lines.

**Breaking changes:**

- Data that an earlier version sealed does not open. This includes DMs,
  private files, and the local sealed cache. There is no fallback to the old
  key.
- A relay announcement from an earlier version has an `x25519_public` field.
  This version rejects it. An earlier version rejects an announcement from
  this version. Upgrade relays and clients together.

**Tests:**

- `identity_test.exs`: the libsodium vector from
  `test/default/ed25519_convert`, agreement of the two functions for 200
  random identities, and the rejected inputs.
- `toolbox_test.exs`: `seal:to` works for an entry without `x25519_public`
  and for a bare hex key without a resolver.

## Out of scope

- A second control plane implementation. `Arc.Control.Local` stays the only
  one.
- Storage backends in `arc_storage`. The DM provider stores files under
  `DM_ROOT` like the journal.
- Group messaging.
- A responder ephemeral in the session (v3).

## Order of work

1. C1 sealed box and HKDF module.
2. C2 publish and resolve.
3. C3 filters and output hook.
4. DM provider Phase 1 with sealed bodies. See `docs/dm/SPEC.md`.
5. C4 session v2 and whitepaper correction.
6. C7 replay guard eviction.
7. C6 version bump, changelog, `arc version`. Last, so the changelog is
   complete.
8. C5 event frame, then DM Phase 3.

Each item is one pull request with its own tests. `mise run check` must
pass on every one. CI runs `mix test --warnings-as-errors`, so a warning
in test code fails the build.

## Status

All seven items landed on 2026-09-14 on branch `feat/sealed-box`, one
commit each, with the DM provider between C3 and C4. The full suite and
`mix lint` pass on every commit. `arc version` reports `0.2.0`.
