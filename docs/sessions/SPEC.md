# Core interactions and sessions

ARC participants may provide and consume capabilities. Transport connections
are infrastructure; a session is an ARC interaction with an authenticated peer.
Core owns the protocol and lifecycle. Providers own their application state.

## Modes

A service declares `service.interactions`: `request_reply`, `server_stream`,
and/or `duplex`. Omission means `request_reply`. Unknown modes are errors.
Existing request/reply calls and queued delivery keep their current protocol.
Live sessions use the same core call verification and provider runtime.

- Request/reply produces one complete result, as today.
- Server streaming supplies an initial request and incremental provider output.
- Duplex permits incremental input and output. EOF on input is a half-close:
  the provider may continue producing output before its final result.

Session v1 uses live event delivery. A directory carrier cannot host a live
session and must fail explicitly before submission. A live path is necessary;
latency and throughput still determine whether an application is practical.

## Shared frame protocol

A frame has `version: 1`, a random 32-byte hex `id`, and an operation. The ID
is bound to the authenticated caller and provider for the session's lifetime.

`open` proposes a mode and carries the initial call in the enclosing message.
`accept` confirms that mode. `data` has a positive sequence number and at most
16 KiB of bytes (base64 in JSON). Sequences start at one in each direction.
An `ack` returns credit after the receiver reads that chunk. Only one chunk
may be unacknowledged per direction. Duplicate data is not delivered twice.
A sequence gap, excess credit or unsupported input ends the session with an
explicit protocol error. Transport acceptance is not application completion.

`end` carries the next sequence number and ends input in that direction.
`close` is the provider's final success or error. `cancel` aborts the interaction.
Close, cancellation, deadlines and disconnection unblock reads and writes.
Queues, session counts and message sizes are bounded. Cancellation and credit
messages must bypass request admission and cannot wait behind their handlers.

Connections do not resume sessions. Version 1 has no automatic resumption,
retransmission, durable session recovery or exactly-once claim. A disconnect
ends the session with an explicit error; callers must treat effects as uncertain.
A new session never silently replays commands from the old session.

## Boundaries

`core/session` owns modes, frames, ordering, credit, half-close and cancellation.
`core/call` authenticates and encrypts live event messages and bridges providers.
`core/provider` supplies the same session machinery to provider handlers.
Adapters supply event delivery or process I/O. No adapter reimplements sessions.
Applications select paths, enforce install consent and choose lifetime budgets.
A provider must apply its access rules before handling a session, just as for
a single request. REPL variables and SQL transaction state stay in the provider.

A provider handler must honor its context. A closed connection or canceled
caller must not leave provider work running. Session state is memory-only in v1.
EOF does not imply transaction commit; application-level commit remains explicit.

## Event and provider mapping

Live session messages are rumors of kind **3276**, encrypted and authenticated
with the existing `core/private` live wrap (21059). A message's JSON content
contains `frame` and, for an open only, `request`. The signed `session` tag must
match the frame ID. `core/call` verifies the wrap and authenticated author on
every frame. An active ID cannot be used by another participant. The initial
request is limited to 16 KiB and the service's declared request limit.

Provider I/O carries `{"op":"session","request_id":"<id>","session":{...}}`.
An incoming open also has `from`, `message`, `meta` and `deadline_ms`, like a
normal provider request. A provider-initiated open uses `call_id` instead of
`request_id`, with `address`, `body` and `deadline_ms`. The host applies the same
installed-service consent and mode checks used by CLI consumers.

A Go provider implements `provider.SessionHandler` for streaming modes. It gets
a `session.Stream` implementing `io.Reader` and `io.Writer`. Core adapts a
`request_reply` session to the existing `HandleRequest` method. Writes split
large results into bounded frames. `CloseWrite` ends one direction; `Wait`
reports final completion. Read EOF alone does not prove final success.
`session.RemoteError` preserves an explicit peer refusal through nested calls.

A runtime admits at most its configured `MaxConcurrent` handlers across ordinary
requests and incoming sessions. Hosted live sessions also share the host's
64-operation admission limit. Rejection queues and per-session bridging queues
are bounded. One pending input chunk and one partial read remainder bound a
reader's payload buffering. The one-chunk credit window favors bounded memory;
it does not promise maximum throughput on high-latency links.

A supplied session deadline is capped at 30 minutes; an omitted deadline defaults
to two minutes. The earlier caller deadline wins. Open, admission and provider
execution share that lifetime. Disconnect and cancellation attempt a bounded
cancellation notice. If the path is unavailable, the peer's deadline remains the
cleanup bound. Providers must release application resources when their context
ends. This protocol adds no forward-secrecy or durable-resumption guarantee.
