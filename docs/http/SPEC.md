# HTTP over ARC

- Status: built
- Layers: sdk
- Owns: sdk/httpadapter
- Proof: go test -count=1 -run '^(TestAGoHandlerAnswersACallThroughARelay|TestLargeBodyOverPipeThroughRelay|TestOlderProviderFallsBackToFrames|TestPipeFailureIsAnError|TestBrokenPipeIsAnError)$' ./cmd/arc/
- Unverified: No test serves an HTTP program written in a language other than Go. No automated test sends a body on a pipe between two machines, or through the TURN server of a relay. A provider with `ARC_PATH=turn` under `arc serve` ran by hand on one machine only.

One ARC call carries one HTTP exchange. The package `sdk/httpadapter` maps
the exchange onto a call, and onto a session.

## 1. Purpose

A service keeps its HTTP code. ARC gives it an identity, sealed calls, and a
path through relays. The service opens no port.

This document follows the semantics of HTTP in RFC 9110. It maps them onto
the call of docs/delivery/SPEC.md, section 4.10.

Two adapters serve HTTP over ARC:

- `httpadapter.New` serves a Go `http.Handler` in the provider program.
- `arc-http` serves an HTTP server of any language. The
  [HTTP app](../../apps/http/README.md) documents it.

Both adapters use the same mapping.

## 2. Terms

| Term | Meaning |
| --- | --- |
| Exchange | One HTTP request and its response. |
| Message | The body of the ARC call: the JSON object of section 3.2. |
| Record | One NDJSON line of an HTTP session, see section 3.6. |
| `Arc-Caller` | The header field that holds the public key of the caller, see section 3.4. |
| Pipe | A byte stream between the consumer and the provider outside the relay. [The direct spec](../direct/SPEC.md) defines it. |
| Path | How the body goes: on a pipe, or in session frames over the relay. |

## 3. Rules

### 3.1 The capability

The capability id is `http`. The id is the scheme, so an address names the
capability like this:

```text
http+arc://<provider>/<path>
http+arc://<64-hex-key>/notes
```

One provider key serves one `http` capability. A second HTTP service needs
its own key.

### 3.2 The request

| HTTP | ARC call |
| --- | --- |
| method | The method of the call. The manifest names the default. `arc call --method` and the `method` of a command change it. |
| path | The path of the call: the path of the address, or of the command. |
| query, header fields, content | The message of the call, as below. |

The message is empty, or it is one JSON object:

```json
{"query": "tag=a&limit=10",
 "headers": {"Content-Type": ["application/json"]},
 "body": "{\"text\": \"hi\"}"}
```

| Field | Meaning |
| --- | --- |
| `query` | The query, without `?`. It follows `application/x-www-form-urlencoded`. |
| `headers` | Each header field, with the list of its values. |
| `body` | The content, as UTF-8 text. |
| `body_base64` | The content, as standard base64. Use it for content that is not UTF-8. |

An empty message is a request with no query, no header fields, and no
content.

The adapter MUST refuse the message with `invalid_request` in these cases:

- The message is not a JSON object.
- The object has a field that this table does not name.
- The object has `body` and `body_base64` together.
- The query has `#`, or a percent escape that is not valid.
- The method is not an HTTP token.

An address has no query. The query goes in the message.

### 3.3 The reply

The reply is one JSON object:

```json
{"status": 201,
 "headers": {"Location": ["/notes/7"], "Content-Type": ["application/json"]},
 "body": "{\"id\":7}"}
```

| Field | Meaning |
| --- | --- |
| `status` | The status code. A handler that sets none gives 200. |
| `headers` | The header fields as they were when the handler sent the status. |
| `body` | The content, if it is UTF-8. |
| `body_base64` | The content, as standard base64, if it is not UTF-8. |

A status of 4xx or 5xx is a reply. It is not an error of the call.

### 3.4 Header fields

- The adapter MUST drop the fields of one connection, both ways:
  `Connection`, `Keep-Alive`, `Proxy-Connection`, `TE`, `Trailer`,
  `Transfer-Encoding`, and `Upgrade`. It drops `Content-Length` too, because
  the content sets it.
- The adapter MUST set `Arc-Caller` to the public key of the caller, in
  lower-case hex. The seal of the call proves this key. If the message holds
  an `Arc-Caller` field, the adapter replaces it.

### 3.5 Limits

- The whole message is at most `max_bytes` of the manifest.
- The content of a response is at most 1 MiB.
- A buffered exchange is one request and one response. No stream, no
  WebSocket, no trailer, and no 1xx response crosses the adapter in a
  buffered call. Sessions carry streams, see section 3.6.

### 3.6 Sessions

A service opts into `server_stream` and/or `duplex` in
`service.interactions`. The existing HTTP call envelope remains valid as the
initial session request.

- HTTP sessions emit NDJSON records: `response` (status and headers), `body`
  (base64 data), optional `trailers`, and `end`.
- Response bodies are incremental and have bounded chunks, not the buffered
  call's 1 MiB total limit. Core deadlines and flow control bound lifetime and
  buffering.
- Redirects are returned to the caller, never followed.

Request body stream:

- Set `stream_body: true` in a duplex initial request to stream raw request
  bytes through the session input. Do not also set `body` or `body_base64`.
- Input EOF ends the request body, and leaves response output open.
- SSE uses the same mapping. HTTP writes and flushes reach the consumer before
  the response ends.

WebSockets:

- Use duplex with `websocket: true`, GET, and optional `subprotocols`. The
  adapter supplies a real HTTP upgrade to the handler.
- A `response` record reports status 101 and the negotiated `protocol`.
- After upgrade, NDJSON input/output records are `text` with `text`, or
  `binary` with base64 `data`.
- Each WebSocket message is limited to 1 MiB.
- An input `ping` yields `pong` after the peer responds.
- Input `close` takes a code and reason. EOF requests normal closure.
- Output `ws_close` preserves the peer's close code/reason.
- Peer ping/pong frames are handled by the WebSocket library.

### 3.7 Response bodies on a pipe

A consumer can ask for the response body of a session on a pipe. The
session stays on the relay. It carries the request, the status, the header
fields, the trailers, the end record and the final outcome. The pipe carries
only the body bytes of that one session.

The request:

- The initial request MAY hold the field `direct`: the offer of a pipe,
  as [the direct spec](../direct/SPEC.md), section 3.7, defines it.
- A request with `direct` MUST NOT set `websocket` or `stream_body`. The
  provider refuses it with `invalid_request`.
- A buffered call (section 3.2) MUST NOT hold `direct`. The provider
  refuses it with `invalid_request`.

The provider:

1. If its policy is `session`, it MUST close the session with `no_pipe`.
2. If it holds 4 pipes, it MUST close the session with `busy`.
3. It answers the offer. Its first record MUST be
   `{"type":"direct","sdp":"<answer>"}`.
4. It waits for the pipe, as the direct spec defines. If the pipe does not
   open, it MUST close the session with `no_path`. It MUST NOT run the HTTP
   request before the pipe opens, so a failed attempt has no effect.
5. It sends the `response` record, then writes the body on the pipe, then
   closes the write side of the pipe. It MUST NOT send `body` records.
6. After the consumer has the last byte, it sends the `trailers` record, if
   any, and `{"type":"end","body_bytes":<n>}`. `n` is the number of body
   bytes on the pipe.

Each body of a session with a pipe goes on the pipe, also an SSE stream.
The pipe sends each write when the handler flushes. The setup of the pipe
adds the gathering of the addresses and the DTLS handshake before the first
byte.
7. If the pipe breaks during the body, it MUST close the session with
   `pipe_failed`.

The consumer:

- It picks the path when the session starts: `auto`, `direct`, `turn` or
  `session`. It MUST NOT move a session to another path.
- Each attempt of the direct spec is one new session. An attempt that
  fails did no work, so the next attempt is not a replay.
- It MUST compare the number of bytes on the pipe with `body_bytes`. A
  different number is an error, not a short success.
- If the pipe closes before its end, the consumer MUST stop with an error
  that says the outcome is not known. It MUST NOT ask again.
- In `auto`, if the provider refuses the offer with `invalid_request` or
  `no_pipe`, the consumer asks again with no offer, and writes
  `the provider has no pipe; using session frames over the relay`. In
  `direct` and `turn`, it stops with an error.
- The offer adds about 1 to 3 KiB to the request, and a session request
  holds at most 16 KiB. If the host of the provider refuses the request
  with `request_too_large`, the consumer does the same as for `no_pipe`.
- In `auto`, a pipe that does not open is an error. The consumer MUST NOT
  then use session frames: the TURN server of the relay is faster than the
  frames in each measured run.

## 4. Behavior

### 4.1 A manifest

This manifest shows the reply as its status and its content. A status of 4xx
or 5xx exits 22, as `curl --fail` does.

```json
"service": {"method": "GET", "path": "/", "max_bytes": 262144,
            "output": {"open": {"parse": "json"}, "format": "response",
                       "exit": [{"where": [{"field": "status", "prefix": "4"}], "code": "22"},
                                {"where": [{"field": "status", "prefix": "5"}], "code": "22"}]}},
"formats": {"response": {"record": "{{status}} {{body}}"}}
```

```bash
arc call --method POST 'http+arc://<provider>/notes' '{"body": "hello"}'
```

### 4.2 The login

A handler uses `Arc-Caller` as the login. It needs no password and no token.

### 4.3 A service that calls

An HTTP handler can call another capability, as the citizen that serves it.
A notes service can keep its notes in SQLite over ARC this way. See
docs/interface/SPEC.md, section 4.2. A server behind `arc-http` calls through
a local endpoint, which the [HTTP app](../../apps/http/README.md#calls-of-the-server)
documents.

### 4.4 The CLI

CLI examples, after installing an HTTP service that declares the mode:

```sh
arc session --http --mode server_stream 'http+arc://<provider>/events'
printf 'hello' | arc session --http 'http+arc://<provider>/upload' '{"stream_body":true}'
arc session --websocket 'http+arc://<provider>/socket' '{"subprotocols":["echo"]}'
```

- The capability's manifest supplies the HTTP method. Publish an appropriate
  POST service for the upload example.
- `--http` writes decoded response bytes, and exits 22 for HTTP error
  statuses. It also rejects an `Arc-Session-Error` trailer from a nested
  provider session.
- `--http` offers a pipe for the body (section 3.7), unless the request
  sets `websocket` or `stream_body`. `--path auto|direct|turn|session` picks
  the path. `arc path <policy>` sets the default of the citizen. The
  default is `auto`.
- At the end, `--http` writes the path on standard error:

```text
path direct (host to host): 104857600 bytes in 2.4 s, 348.9 Mbit/s
path session (relay frames): 104857600 bytes in 54.3 s, 15.5 Mbit/s
```

  These two lines are the medians of three runs each on one Mac, through a
  local relay on the loopback address (measured, 2026-10-11). A real link
  is slower. [The direct spec](../direct/SPEC.md), section 8.2, gives the
  commands.

- A provider program reads its policy from `ARC_PATH`. `arc serve` gives
  it `ARC_RELAYS`, `ARC_HOME` and `ARC_PROGRAM`, so a provider with
  `ARC_PATH=turn` can run `arc relay turn`.
- `--websocket` sends each input line as a text message, and displays
  received messages.
- Without these flags, `arc session` exposes the record protocol directly,
  including headers and binary messages.

### 4.5 The Go package

The Go adapter is `github.com/gezibash/arc/sdk/httpadapter` (package
`httpadapter`). Shared service contracts are in `sdk/provider`. The HTTP app
implementation is in `apps/http/server`; `apps/http/cmd/arc-http` supplies
process streams and signals. Other entry points can use `sdk/stdio.Run`. See
[package boundaries](../ARCHITECTURE.md).

## 5. Failures

| Error | Cause |
| --- | --- |
| `invalid_request` | The message is not a request. See section 3.2. |
| `response_too_large` | The content of the response is over 1 MiB. The adapter never cuts a response short. |
| `internal_error` | The handler panicked. |
| `no_pipe` | The request offers a pipe, and the policy of the provider is `session`. |
| `busy` | The request offers a pipe, and the provider holds 4 pipes. |
| `no_path` | The pipe did not open. The handler did not run. |
| `pipe_failed` | The pipe broke during the body. |

What the consumer writes for each failure of a pipe:

| Failure | What the consumer does | What the user sees |
| --- | --- | --- |
| The provider is older, or its policy is `session` | In `auto`, a new session with no offer. Else it stops. | `the provider has no pipe; using session frames over the relay`, or `the provider has no pipe; run again with --path session` |
| No path in an attempt | The next attempt of the direct spec | The notes of the direct spec, section 3.5 |
| No path in each attempt | Stops | `no direct path, and no path through the relay: ...` |
| `busy` | Stops | `the provider has too many pipes open now; try again later` |
| The pipe breaks during the body | Stops | `the pipe closed after N bytes; the outcome is not known` |
| `body_bytes` and the count differ | Stops | `the body has N bytes, the provider sent M` |

- The HTTP adapter rejects canceled requests before dispatch. It reports
  cancellation if the handler returns after the caller stops. An empty
  canceled response is not 200.
- In a session, core's final outcome follows `end`. Clients must check both.
- A failed WebSocket handshake reports its HTTP status, and fails the session.
- Abnormal WebSocket closure also fails the core session. Cancellation closes
  the underlying connection.

## 6. Security

- The seal of the call proves the key in `Arc-Caller`. Caller identity is
  assigned by the adapter; a supplied `Arc-Caller` cannot replace it.
- The service opens no port. The adapter never cuts a response short: an
  oversized response fails with `response_too_large`.
- Core deadlines and flow control bound the lifetime and the buffering of a
  session.
- A pipe shows the IP addresses of each end to the other end. A provider
  shows them to each caller that its access rules let in. A consumer that
  does not want this uses `--path turn` or `--path session`. A provider
  sets `ARC_PATH=turn` or `ARC_PATH=session`.
- The answer and the offer travel in the sealed session, so the DTLS link
  of the pipe ends at the citizen that signed them. The direct spec,
  section 6, gives the details.
- A provider holds at most 4 pipes. A pipe holds sockets and buffers.

## 7. Compatibility

- The existing HTTP call envelope remains valid as the initial session
  request. A service that declares no session mode keeps the buffered
  exchange of sections 3.2 and 3.3.
- Both in-process `httpadapter.New(handler)` and `arc-http` use this mapping.
- An older consumer never sends `direct`. It gets the body in session
  frames, as before.
- An older provider decodes the request strictly, so it refuses `direct`
  with `invalid_request` before it runs the HTTP request. A consumer in
  `auto` asks again with no offer, and gets the body in frames.
- A consumer of this version opens a pipe by default. It thus shows its IP
  address to the provider, and it needs UDP. `--path session` or
  `arc path session` gives the old behavior.

## 8. Proof

- `go test ./sdk/httpadapter ./apps/http/server` tests both adapters.
- `TestBundledProviderSessionsThroughCLI` in `cmd/arc` runs `arc-http`
  through a local relay and the normal CLI.
- `TestAGoHandlerAnswersACallThroughARelay` in `cmd/arc` runs a Go handler
  from `httpadapter.New` the same way.
- `TestLargeBodyOverPipeThroughRelay` in `cmd/arc` gets 16 MiB from
  `arc-http` through a local relay. The body goes on a pipe on the loopback
  address, and arc names the path. `--path session` and `arc path session`
  send the body in frames.
- `TestOlderProviderFallsBackToFrames` calls a provider that refuses
  `direct` as `invalid_request`. In `auto`, arc asks again with no offer.
  With `--path direct`, arc stops.
- `TestPipeFailureIsAnError` calls a provider that answers each offer, but
  no pipe opens. arc makes each attempt and stops with an error. It never
  asks for the body in frames.
- `TestBrokenPipeIsAnError` breaks the pipe during the body.
- `TestFifthPipeIsBusy` in `sdk/httpadapter` holds 4 pipes on one provider.
  The fifth offer gets `busy` before the handler runs.
- `TestBodyOverPipe`, `TestNoPipeAndAuto` and `TestOfferOverLimit` in
  `sdk/httpadapter` check `body_bytes`, the trailers, and the fall back of
  `auto`.

Unverified:

- No automated test sends a body on a pipe between two machines, or through
  the TURN server of a relay. The tests use the loopback address.
- `--path turn`, and a provider with `ARC_PATH=turn` under `arc serve`, ran
  by hand on one Mac, through a local relay with TURN. No test runs them.
- No run on two machines behind NATs.
