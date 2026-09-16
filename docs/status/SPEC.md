# Status

`arc status` is a read-only diagnostic snapshot. It does not start services,
generate or select identities, publish capabilities, or register citizen routes.

## Client

Identity selection follows [the identity specification](../identity/SPEC.md).
The report includes the selected public identity and selection source. A missing
selection is a normal setup state; an invalid explicit selector is an error and
never falls through. No secret key material or host token is included.

The client relay comes from `--relay`, otherwise `ARC_RELAY`. The public-key pin
comes from `--relay-pubkey`, otherwise `ARC_RELAY_PUBKEY`. Explicit invalid values
are errors. A configured relay is not labelled connected: normal CLI processes
connect on demand and terminate when their command finishes.

`--check` opens a temporary connection, reads the ARC relay greeting, optionally
compares the configured public-key pin, and closes the connection. The total
probe deadline is two seconds, including name resolution. It never sends a
citizen authentication proof, so it cannot take over an existing citizen route.
The result is `reachable`, `unreachable`, or `key_mismatch`. A matching greeting
is not cryptographic proof of relay ownership or end-to-end delivery.

## Server

The local host is queried using its existing public `status` operation at
`~/.config/arc/host.sock`, or `--socket PATH`. Its reported states are `running`,
`not_running`, and `unavailable`; an unresponsive or incompatible host is not
declared stopped. The query is bounded.

Host status adds `relay_connections` without removing older fields. Each entry
contains only the public identity name and public key, connection state, and
relay host/port when configured. States are `connected`, `reconnecting`,
`disconnected`, `local`, or `unknown` when the runtime cannot be observed.
An empty loaded identity set yields no connections, even with a configured
relay. An older host without this field reports live connection data unavailable.
Connections for the selected identity are highlighted separately from shell
configuration, since the host may use a different relay.

Docker inspection uses only public container-list metadata for the exact Compose
project `arc-local` (override with `--docker-project NAME`). It follows the current
Docker context and does not inspect environment, logs, volumes, or credentials.
`--no-docker` skips it. Missing Docker and an unavailable daemon are distinct
states. Container state and health are not evidence of ARC connectivity.

Standalone `arc serve`, `arc listen`, `arc mcp`, and native `arc relay` processes
do not expose a shared status endpoint and are outside this snapshot. Their
absence from the report must not be interpreted as stopped or disconnected.

## Output and exit status

`--json` emits the same public snapshot with `version`, `identity`, `relay`,
`host`, and `docker` fields. It remains valid JSON when a diagnostic fails.

Exit status is zero for a completed snapshot, including an unconfigured client,
stopped host, or unavailable optional Docker inspection. Invalid options,
invalid explicit identity/relay selection, an unobservable local host, and a
failed explicit relay check return one. A reachable relay with no pin is clearly
labelled unpinned and does not establish trust.
