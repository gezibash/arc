# Core interactions and sessions

- Status: built
- Layers: core, sdk, runtime
- Owns: core/session
- Proof: go test -count=1 -run '^TestBundledProviderSessionsThroughCLI$' ./cmd/arc/
- Unverified: No test runs a session across a network other than loopback.

ARC participants may provide and consume capabilities. Transport connections
are infrastructure; a session is an ARC interaction with an authenticated peer.
Core owns the protocol and lifecycle. Providers own their application state.

## 1. Purpose

This spec defines three interaction modes between a caller and a provider:
one request and one reply, a stream of output, and a duplex stream. It
defines the frames that carry a session, how events and provider I/O carry
the frames, and the limits of a session.

Version 1 does not give these things:

- An automatic resumption of a session after a disconnect.
- A retransmission, a durable recovery of a session, or an exactly-once
  claim.
- Forward secrecy.

## 2. Terms

| Term | Meaning |
| --- | --- |
| Session | One interaction with an authenticated peer: one request/reply, server stream or duplex interaction. |
| Mode | The kind of a session: `request_reply`, `server_stream` or `duplex`. |
| Frame | One message of the session protocol, with a version, an ID and an operation. |
| Credit | Permission to send the next data chunk. An `ack` returns it. |
| Half-close | The end of input in one direction, while output can continue. |

## 3. Rules

### 3.1 Modes

A service declares `service.interactions`: `request_reply`, `server_stream`,
and/or `duplex`. Omission means `request_reply`. Unknown modes are errors.

- Request/reply produces one complete result, as today.
- Server streaming supplies an initial request and incremental provider
  output.
- Duplex permits incremental input and output. EOF on input is a
  half-close: the provider may continue producing output before its final
  result.

Session v1 uses live event delivery. A directory carrier cannot host a live
session, and MUST fail explicitly before submission.

### 3.2 Frames

A frame has `version: 1`, a random 32-byte hex `id`, and an operation. The ID
is bound to the authenticated caller and provider for the session's lifetime.

| Operation | Rule |
| --- | --- |
| `open` | Proposes a mode. The enclosing message carries the initial call. |
| `accept` | Confirms that mode. |
| `data` | Has a positive sequence number, and at most 16 KiB of bytes (base64 in JSON). |
| `ack` | Returns credit after the receiver reads that chunk. |
| `end` | Carries the next sequence number, and ends input in that direction. |
| `close` | The provider's final success or error. |
| `cancel` | Aborts the interaction. |

- Sequences start at one in each direction.
- A sender MUST NOT have more than one unacknowledged chunk in each
  direction.
- Live delivery does not keep the order of frames. Only an accepting
  responder sends `data`, `end` or a `close` without an error. An initiator
  MUST take the first of these as acceptance. A later `accept` MUST still
  confirm the mode.
- Duplicate data MUST NOT be delivered twice.
- A sequence gap, excess credit or unsupported input MUST end the session
  with an explicit protocol error.
- Queues, session counts and message sizes are bounded.
- Cancellation and credit messages MUST bypass request admission. They
  cannot wait behind their handlers.

### 3.3 Events

Live session messages are rumors of kind 3276, encrypted and authenticated
with the existing `core/private` live wrap (21059).

- A message's JSON content contains `frame` and, for an open only, `request`.
- The signed `session` tag MUST match the frame ID.
- `core/call` MUST verify the wrap and the authenticated author on every
  frame.
- An active ID cannot be used by another participant.
- The initial request is limited to 16 KiB and the service's declared request
  limit.

### 3.4 Provider I/O

Provider I/O carries `{"op":"session","request_id":"<id>","session":{...}}`.

- An incoming open also has `from`, `message`, `meta` and `deadline_ms`,
  like a normal provider request.
- A provider-initiated open uses `call_id` instead of `request_id`, with
  `address`, `body` and `deadline_ms`.
- The host MUST apply the same installed-service consent and mode checks that
  CLI consumers use.

### 3.5 Limits

- A runtime admits at most its configured `MaxConcurrent` handlers across
  ordinary requests and incoming sessions.
- Hosted live sessions also share the host's 64-operation admission limit.
- Rejection queues and per-session bridging queues are bounded.
- One pending input chunk and one partial read remainder bound a reader's
  payload buffering.
- A supplied session deadline is capped at 30 minutes. An omitted deadline
  defaults to two minutes. The earlier caller deadline wins. Open, admission
  and provider execution share that lifetime.

## 4. Behavior

### 4.1 Who owns what

- `core/session` owns modes, frames, ordering, credit, half-close and
  cancellation.
- `core/call` authenticates and encrypts live event messages, and bridges
  providers.
- `sdk/provider` supplies the same session machinery to provider handlers.
- Adapters supply event delivery or process I/O. No adapter reimplements
  sessions.
- Applications select paths, enforce install consent and choose lifetime
  budgets.
- A provider must apply its access rules before it handles a session, just as
  for a single request. REPL variables and SQL transaction state stay in the
  provider.

Existing request/reply calls and queued delivery keep their current protocol.
Live sessions use the same core call verification and provider runtime.

### 4.2 A Go provider

A Go provider implements `provider.SessionHandler` for streaming modes. It
gets a `session.Stream` that implements `io.Reader` and `io.Writer`.
`sdk/provider` names the same type `provider.Stream`.

- Core adapts a `request_reply` session to the existing `HandleRequest`
  method.
- Writes split large results into bounded frames.
- `CloseWrite` ends one direction. `Wait` reports final completion. Read EOF
  alone does not prove final success.
- `session.RemoteError` preserves an explicit peer refusal through nested
  calls.
- A provider handler must honor its context. A closed connection or a
  canceled caller must not leave provider work running.
- Providers must release application resources when their context ends.

### 4.3 The end of a session

- Close, cancellation, deadlines and disconnection unblock reads and writes.
- Disconnect and cancellation attempt a bounded cancellation notice. If the
  path is unavailable, the peer's deadline remains the cleanup bound.
- EOF does not imply a transaction commit. An application-level commit
  remains explicit.

## 5. Failures

- A sequence gap, excess credit or unsupported input ends the session with
  an explicit protocol error.
- A directory carrier fails explicitly before submission.
- A disconnect ends the session with an explicit error. Callers must treat
  its effects as uncertain.
- Transport acceptance is not application completion.
- A lost path leaves the peer's deadline as the cleanup bound.

## 6. Security

- `core/call` verifies the wrap and the authenticated author on every frame,
  and the signed `session` tag must match the frame ID.
- An active ID cannot be used by another participant.
- A provider applies its access rules before it handles a session.
- Queues, session counts, message sizes and deadlines are bounded, so a peer
  cannot grow them without limit.
- This protocol adds no forward-secrecy or durable-resumption guarantee.

## 7. Compatibility

- Omission of `service.interactions` means `request_reply`, so an existing
  manifest keeps its contract.
- Existing request/reply calls and queued delivery keep their current
  protocol.
- Connections do not resume sessions. A new session never silently replays
  commands from the old session.
- Session state is memory-only in version 1.

## 8. Proof

- `TestBundledProviderSessionsThroughCLI` in `cmd/arc` runs the bundled apps
  through a local relay and the normal CLI.
- The tests of `core/session`: credit, half-close, duplicates, gaps,
  cancellation and the request/reply mode.
- The tests of `core/call` and `sdk/provider`: identity binding, mode
  refusals, a provider that consumes another streaming provider, watch loss,
  shared admission, and a malformed frame.

Not verified:

- A live path is necessary; latency and throughput still determine whether
  an application is practical.
- The one-chunk credit window favors bounded memory. It does not promise
  maximum throughput on high-latency links. Local tests do not establish
  throughput over a wide-area network.

## Gates

This spec predates the gates. See [the grandfathered list](../GRANDFATHERED.md).
