# Status

`arc status` queries the configured relay through ARC's existing control
connection. The default output is a compact key/value table. This is a snapshot
of the remote service, not a persistent connection indicator for the caller.

## Configuration

The relay address comes from `--relay`, otherwise `ARC_RELAY`, otherwise the
default saved by `arc join`. The required public-key pin comes from
`--relay-pubkey`, otherwise `ARC_RELAY_PUBKEY`, otherwise the remembered pin
for that address. See [joining a relay](../join/SPEC.md).
Invalid or missing settings fail without falling back to a local service.
Identity selection is independent: status does not read or change the selected
citizen.

The query uses a temporary in-memory identity, sends a `status` directory
request, and closes its connection. It does not save a key, publish a capability,
or replace an existing citizen route. Its temporary route exists only for the
query. The relay has five seconds to answer the request. The complete command,
with the connection, has fifteen seconds.

Status MUST NOT invoke deployment tools, inspect containers, or assume a Compose
project name. Runtime state comes from ARC-owned interfaces.

## Relay response

The `status` directory request permits only `type` and `request_id`. The relay
returns `ok: true` and a `status` object containing:

- `state`: `running`, meaning the relay answered this query.
- `role`: `relay`.
- `version`: the running relay application version.
- `public_key`: the relay's lowercase hexadecimal public key.
- `uptime_seconds`: elapsed monotonic time since relay startup.
- `federation_transit`: whether onward federation traffic is enabled.

No citizen identities, peer addresses, or private configuration are returned.
A running response does not prove end-to-end delivery, federation health, or
a citizen's persistent connection.
The existing handshake checks the presented key against the pin; its greeting
is not cryptographic proof of server ownership.

Older relays that reject the operation report unsupported status queries.
There is no fallback from an unsupported query to a successful greeting check.

## Output and exit status

The table has five lines: `relay` (the selected address), `state`, `version`,
`uptime`, and `key` (the public key of the relay). `--json` writes the
`status` object of the relay as JSON, with all six fields. An error goes to
standard error. Exit status is zero for a successful query and one for a
configuration, connection, protocol, or option error. `--help` succeeds without
contacting a service.

This replaces the combined client/host/container snapshot shipped in v0.3.2.
Its JSON shape changes accordingly. `--socket`, `--docker`, and
`--docker-project` are no longer accepted by `arc status`. v0.7.0 also
removed `--format` and `--check`.
