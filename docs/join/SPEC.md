# Joining a relay

`arc join <host[:port]>` selects a citizen's default relay. A hostname without
a port uses ARC's default TCP port, 7331; normal host resolution finds the
address. For example, `arc join the.republic.sh` works when that host resolves
to a reachable ARC relay on that port. This does not provision the hostname,
add a DNS service record, or configure relay federation.

## First join

The command checks the relay's public status using a temporary in-memory
identity. It prints the relay's full public-key fingerprint and asks for an
explicit `yes` before saving trust. Declining or reaching end-of-input leaves
relay settings and identity selection unchanged. An operator-supplied
`--relay-pubkey KEY` provides the pin for unattended use instead of a prompt.
The command rechecks the relay with the confirmed pin before saving settings.

The fingerprint must be compared with a trusted value from the relay operator.
The current relay greeting presents a key but does not prove server possession
of its private key. Joining uses the existing handshake; it does not introduce
stronger server authentication or make an unverified first connection safe.

An active identity is reused, respecting `ARC_KEY`, the current directory's
`arc.key`, and the global default. With no selector and an empty key store, a
new identity and global selector are created after trust confirmation. Existing
keys without an active selection require `arc keys use NAME`; invalid explicit
selectors fail instead of creating or selecting another identity.

Joining never sends the citizen's private key to the relay. The initial probe
does not use the citizen's identity or replace a running provider's route.
The command closes its probe connection. Future commands connect on demand;
there is no background citizen connection after `arc join` exits.

## Saved defaults and trust

`~/.config/arc/relays.json` contains the default address and remembered public
pins keyed by relay address. It contains no private keys. Rejoining a known
address uses its saved pin without another trust prompt. A different key is
rejected, even if supplied explicitly; joining cannot silently rotate trust.
Previously joined addresses retain their pins when the default changes.
Writes use a private file and an exclusive lock. If a writer crashes with
`relays.json.lock` present, later joins fail closed; check that no join is
running before removing that stale lock. Readers still use the saved file.

Connection selection is:

1. Explicit command flags.
2. `ARC_RELAY` and `ARC_RELAY_PUBKEY` environment settings.
3. The saved default and the saved pin for the selected address.

A saved pin for one address must never be applied to another address. Invalid
saved settings fail instead of silently selecting local delivery. Commands
that support explicit `--local` retain local operation without loading relay
settings. Joining reports conflicting environment overrides rather than
editing shell startup files. Remove those overrides to use the saved default.

`arc status`, discovery, provider installation, messaging, serving, protocol
requests, the local host, MCP, and the local Agora interface use these defaults.
Already-running processes keep their current connections; joining changes
future command configuration only.

Saving the identity and saving relay settings are separate filesystem actions.
A storage failure after identity creation can leave that identity selected;
the command reports failure and does not claim setup completed. Retrying reuses
the identity. No existing identity is deleted to undo a failed join.

## Scope

Joining does not install discovered tools, publish capabilities, approve
federation partners, or admit onward traffic. Relay-to-relay federation remains
an operator action described in `docs/federation/SPEC.md`.

## Verification

Tests use disposable local relays and isolated configuration/key directories.
They cover accepted and declined trust, unattended explicit pins, remembered
key changes, identity preservation, malformed settings, override precedence,
and subsequent status queries using only the saved default.
