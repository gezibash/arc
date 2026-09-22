# Protocol transport over ARC

## Scope

ARC carries application bodies between citizen and provider identities. Relay
operators route the encrypted messages; the provider implements the application
protocol. The first general-purpose client is `arc call`, using the existing
request/reply frame format. SQLite is the first real database provider using it.

Implemented:

- Identity-addressed `<scheme>+arc://<full-provider-public-key>/<resource>` requests.
- Signed capability lookup and provider-key verification before application calls.
- Relay delivery, including permitted onward federation.
- Opaque request/reply bytes, including a binary-safe opt-in for external runtimes.
- A real SQLite provider with named databases and citizen access grants.

This is a bounded request/reply profile. It does not implement an arbitrary TCP
tunnel, Git remote helper, browser proxy, or a reliable continuous byte stream.

```mermaid
flowchart LR
    C[Citizen client] --> T[ARC request transport]
    T <--> A[Citizen relay]
    A <--> B[Permitted relay partners]
    B <--> P[Provider relay]
    P <--> E[ARC exec bridge]
    E <--> S[SQLite provider]
    S <--> D[(Local database)]
```

## Address and capability

```text
sqlite+arc://<64-hex-provider-public-key>/main
```

The URI scheme appends `+arc` to the advertised application protocol: a capability
with scheme `sqlite` is addressed using `sqlite+arc://`. The authority is the
complete provider key, never an IP address. The path names
a provider-defined resource. It does not grant filesystem access.

The initial parser accepts lowercase protocol names made of letters, digits, and
hyphens; hexadecimal provider keys; and paths containing ASCII letters, digits,
`/`, `.`, `_`, `~`, or `-`. It rejects user information, ports, queries, fragments,
percent escapes, dot segments, and the reserved `/info` control namespace. Names
and key prefixes are not supported by this client. This makes the trust target
explicit while leaving richer addressing for a future version.

The client fetches `GET /info/capabilities/primary` over the same ARC route. Use
`--capability ID` to select another capability. The signed package must verify,
its provider key must equal the address key, its capability ID must equal the
selection, and its scheme must match the URI. Only `request_reply` capabilities
are accepted. ARC takes the method from that package and the resource path from
the URI. It does not interpret the body or execute provider-authored client code.

The standard manifest `invocation` shape already describes request and response
bodies. A protocol provider can use a normal text body or opt into binary bytes:

```json
{
  "mode": "request_reply",
  "method": "RAW",
  "path": "/",
  "request_body": {"type": "bytes", "encoding": "base64"},
  "response_body": {"type": "bytes", "encoding": "base64"}
}
```

## Client usage

From the repository root, with an active citizen key:

```sh
arc call 'sqlite+arc://<provider-public-key>/main' \
  '{"sql":"SELECT name FROM sqlite_schema WHERE type = ?","params":["table"]}' \
  --relay 127.0.0.1:7331 --relay-pubkey '<relay-public-key>'
```

`ARC_RELAY` and `ARC_RELAY_PUBKEY` also configure the relay, and so does the
relay that `arc join` saved. The command requires a relay and its pinned key.
An invalid or unavailable configured relay is an error, with no local fallback.

The second argument is the request body. Without it, `arc call` reads the
body from standard input, for example `arc call ADDRESS < request.json`. If
standard input is a terminal, the body is empty. The output is the response
body on standard output, byte for byte. Only when standard output is a
terminal, and the body does not end with a newline, `arc call` adds one.
Output failures after a completed request do not undo provider changes.

The Go API is `client.Peers().Request(ctx, peer, meta, body)`. The caller owns
the connection and its delivery policy. Success returns the reply, which holds
a body and its meta. The call reads only the reply that matches both the
provider key and the generated request ID. Other messages stay where they are.

## Binary boundary

ARC frame bodies already carry raw bytes. Base64 is used only across the local
newline-delimited JSON connection to an external provider runtime. It is not an
extra network encoding layer or a change to the relay protocol.

With request encoding `base64`, the runtime receives:

```json
{"op":"request","request_id":"...","from":"<caller-key>","meta":{"method":"RAW","path":"/"},"encoding":"base64","message":"AP+A"}
```

With response encoding `base64`, it must reply:

```json
{"op":"reply","request_id":"...","encoding":"base64","reply":"AP+A"}
```

The bridge decodes the response before placing its bytes into an ARC frame.
Missing IDs, wrong encoding, invalid base64, and oversized decoded bodies fail
explicitly. Text providers retain the existing JSON contract. Non-UTF-8 input to
a text provider is rejected by the URI client before application submission.

## Limits and failure semantics

- URI client request and response bodies: at most 1 MiB each. Signed detail
  documents fetched by that client: 256 KiB. These limits do not replace the
  larger legacy exec text contract used by other clients.
- Providers can declare `invocation.request_body.max_bytes`, which the exec bridge
  enforces even when a caller bypasses the URI client. SQLite declares 1 MiB and
  applies its own lower configured query-body limit. Legacy text runtimes default
  to 64 MiB; binary runtimes have a 1 MiB ceiling.
- Each exec provider admits at most 256 pending requests. Duplicate pending IDs,
  a full queue, and a busy runtime pipe fail before forwarding. Pending slots are
  released by replies or process exit, not client timeouts. A stuck runtime can
  require an operator restart; freeing slots while its work remains queued would
  let callers accumulate unbounded work.
- Time budget: `--timeout` seconds, 30 by default. The budget covers the relay
  connection, the capability lookup and the application reply.
- Smaller relay frame caps can reject messages including their framing overhead.
- A request is sent once. There is no automatic retry, offline queue, reconnect
  recovery, or exactly-once execution guarantee.
- After a timeout, `arc call` reports `client: the peer did not answer`. The
  outcome is unknown: a provider can have committed a write before its
  response was lost. Check its state before submitting that operation again.
  Other connection failures can also leave uncertain effects; an error alone
  is not proof of rollback.
- Relays see identities and routing metadata. A normal provider sees the request
  and its data. Transport encryption does not hide data from that provider's host.

## Next profile: continuous byte streams

Git and other stream protocols should build on a common authenticated stream
profile. Before exposing a native client bridge, it needs an explicit opening
acknowledgement, provider-key and capability binding, ordered byte offsets, and
bounded flow control. The receiver grants a byte budget so a fast sender cannot
grow its buffers indefinitely. Closing one direction must preserve the other
direction until its remaining bytes are read. Reset/disconnect must surface an
error to the native client, rather than silently losing data.

Those stream semantics must be verified over multiple relay hops and under
disconnects. They are not supplied by naming a URI or by the current terminal
stream events. A later Git helper can connect Git's standard input/output to
that profile, while the provider maps the citizen identity to repository grants.
Relays should remain unaware of repository and database operations.

## Optional direct promotion

[Direct request/reply](DIRECT.md) is an implemented, explicit opt-in profile.
Both `arc serve` and `arc call` need matching `--direct-policy` files and
configured pinned relays. Relay delivery remains the default. The
identity-addressed URI and signed capability checks do not change when a
route promotes to direct TLS.

The profile has one reachable listener, literal operator-approved addresses,
finite leases, and relay-negotiated renewal. It has no automatic route ranking,
NAT traversal, backup relay configuration, stream migration, or application
replay. [Promotion](PROMOTION.md), [path selection](PATHS.md), and
[the carrier](CARRIER.md) describe the implemented boundary and future scope.

## Validation

```sh
go test ./client/... ./provider/... ./citizen/...
```

See [SQLite provider](../../cmd/sqlite-provider/README.md) for its independent tests
and [federation](../federation/SPEC.md) for operator routing policy.
