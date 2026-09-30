# HTTP over ARC

- Status: built
- Layers: sdk, app
- Owns: sdk/httpadapter
- Proof: go test -count=1 -run '^TestAGoHandlerAnswersACallThroughARelay$' ./cmd/arc/
- Unverified: No test serves an HTTP program written in a language other than Go.

Two adapters serve HTTP over ARC:

- `httpadapter.New` serves a Go `http.Handler` in the provider program.
- `arc-http` serves an HTTP server of any language. See section 10.

Go tests prove both: `go test ./sdk/httpadapter ./apps/http/server`. The test
`TestBundledProviderSessionsThroughCLI` in `cmd/arc` runs `arc-http` through a
local relay and the normal CLI. The test
`TestAGoHandlerAnswersACallThroughARelay` runs a Go handler from
`httpadapter.New` the same way.

The Go adapter is `github.com/gezibash/arc/sdk/httpadapter` (package
`httpadapter`). Shared service contracts are in `sdk/provider`. The HTTP app
implementation is in `apps/http/server`; `apps/http/cmd/arc-http` supplies process streams
and signals. Other entry points can use `sdk/stdio.Run`. See
[package boundaries](../ARCHITECTURE.md).

## 1. Purpose

One ARC call carries one HTTP exchange. A service keeps its HTTP code. ARC
gives it an identity, sealed calls, and a path through relays. The service
opens no port.

This document follows the semantics of HTTP in RFC 9110. It maps them onto
the call of docs/delivery/SPEC.md, section 11.4.

## 2. The capability

The capability id is `http`. The id is the scheme, so an address names the
capability like this:

```text
http+arc://<provider>/<path>
http+arc://<64-hex-key>/notes
```

One provider key serves one `http` capability. A second HTTP service needs
its own key.

## 3. The request

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

The adapter refuses the message with `invalid_request` in these cases:

- The message is not a JSON object.
- The object has a field that this table does not name.
- The object has `body` and `body_base64` together.
- The query has `#`, or a percent escape that is not valid.
- The method is not an HTTP token.

An address has no query. The query goes in the message.

## 4. The reply

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

## 5. Header fields

- The adapter drops the fields of one connection, both ways: `Connection`,
  `Keep-Alive`, `Proxy-Connection`, `TE`, `Trailer`, `Transfer-Encoding`, and
  `Upgrade`. It drops `Content-Length` too, because the content sets it.
- The adapter sets `Arc-Caller` to the public key of the caller, in
  lower-case hex. The seal of the call proves this key. If the message holds
  an `Arc-Caller` field, the adapter replaces it.

A handler uses `Arc-Caller` as the login. It needs no password and no token.

## 6. Errors

| Error | Cause |
| --- | --- |
| `invalid_request` | The message is not a request. See section 3. |
| `response_too_large` | The content of the response is over 1 MiB. The adapter never cuts a response short. |
| `internal_error` | The handler panicked. |

## 7. Limits

- The whole message is at most `max_bytes` of the manifest.
- The content of a response is at most 1 MiB.
- An exchange is one request and one response. No stream, no WebSocket, no
  trailer, and no 1xx response crosses the adapter.

## 8. A manifest

This manifest shows the reply as its status and its content. A status of 4xx or 5xx exits 22, as `curl --fail` does.

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

## 9. A service that calls

An HTTP handler can call another capability, as the citizen that serves it.
A notes service can keep its notes in SQLite over ARC this way. See
docs/interface/SPEC.md, section 14.2. A server behind `arc-http` calls
through a local endpoint, see section 10.2.

## 10. The adapter for any language

`arc-http` starts an HTTP server as its child, and forwards each call
to it. The server can be in any language. It needs no ARC library:

```sh
arc-http <program> [args...]
arc serve 'exec:///usr/local/bin/arc-http?manifest=/srv/app/manifest.json&args=/srv/app/server'
```

### 10.1 The server

The server gets three variables in its environment:

| Variable | Meaning |
| --- | --- |
| `PORT` | The port on `127.0.0.1` where the server listens. |
| `ARC_CALL_URL` | The endpoint for the calls of the server. See 10.2. |
| `ARC_CALL_TOKEN` | The bearer token of each call. |

These rules hold:

- The server listens on `127.0.0.1:$PORT` in 30 seconds. If not, or if it
  ends first, `arc-http` fails.
- The standard output of the server goes to the standard error of
  `arc-http`, because standard output carries the ARC protocol.
- One exchange takes at most 50 seconds. A server that does not answer in
  that time gives 502. An earlier caller deadline applies. The HTTP adapter
  rejects canceled requests before dispatch and reports cancellation if the
  handler returns after the caller stops. An empty canceled response is not 200.
- If the server ends, `arc-http` ends, and `arc serve` says that the
  provider stopped.
- When `arc serve` stops, `arc-http` sends SIGTERM to the server. If
  the server has not ended after 3 seconds, it sends SIGKILL.
- If `arc-http` itself gets SIGKILL, the server stays behind.

### 10.2 Calls of the server

The server calls a capability that its citizen installed with one POST:

```http
POST $ARC_CALL_URL
Authorization: Bearer $ARC_CALL_TOKEN
Content-Type: application/json

{"address": "sqlite+arc://<key>/main", "body": "{\"sql\": \"select 1\"}"}
```

Any HTTP client can make the call. With `curl`:

```sh
curl -s -H "Authorization: Bearer $ARC_CALL_TOKEN" -H 'Content-Type: application/json' \
  -d '{"address": "sqlite+arc://<key>/main", "body": "{\"sql\": \"select 1\"}"}' \
  "$ARC_CALL_URL"
```

The endpoint listens on `127.0.0.1` only. It takes a call only with the
token, so another program on the machine cannot call as the citizen. The
answer is one JSON object:

| Status | Object | Meaning |
| --- | --- | --- |
| 200 | `{"reply": "..."}` | The reply of the provider that got the call. |
| 502 | `{"refused": "unauthorized"}` | That provider answered with an error. |
| 503 | `{"error": "..."}` | ARC could not make the call, for example `not_installed`. |
| 400 | `{"error": "..."}` | The request is not a call. |
| 401 | `{"error": "..."}` | The token is missing or wrong. |


## Live session mapping

A service opts into `server_stream` and/or `duplex` in `service.interactions`.
The existing HTTP call envelope remains valid as the initial session request.
HTTP sessions emit NDJSON records: `response` (status and headers), `body`
(base64 data), optional `trailers`, and `end`. Core's final outcome follows
`end`; clients must check both. Response bodies are incremental and have bounded
chunks, not the buffered call's 1 MiB total limit. Core deadlines and flow control
bound lifetime and buffering. Redirects are returned to the caller, never followed.

Set `stream_body: true` in a duplex initial request to stream raw request bytes
through the session input. Do not also set `body` or `body_base64`. Input EOF
ends the request body and leaves response output open. SSE uses the same mapping;
HTTP writes and flushes reach the consumer before the response ends.

For WebSockets, use duplex with `websocket: true`, GET, and optional
`subprotocols`. The adapter supplies a real HTTP upgrade to the handler. A
`response` record reports status 101 and the negotiated `protocol`; a failed
handshake reports its HTTP status and fails the session. After upgrade, NDJSON
input/output records are `text` with `text`, or `binary` with base64 `data`.
Each WebSocket message is limited to 1 MiB. An input `ping` yields `pong` after
the peer responds. Input `close` takes a code and reason; EOF requests normal
closure. Output `ws_close` preserves the peer's close code/reason. Abnormal
closure also fails the core session. Cancellation closes the underlying connection.
Peer ping/pong frames are handled by the WebSocket library.

Both in-process `httpadapter.New(handler)` and `arc-http` use this mapping.
Caller identity is assigned by the adapter; a supplied `Arc-Caller` cannot replace
it. Provider-to-provider calls from hosted applications can use authenticated
`POST /session` on the same local server as `ARC_CALL_URL`. Query parameters are
`address`, `mode` (default `server_stream`), and `body` (the initial request).
The HTTP body carries raw duplex input and the response carries raw session output.
Read to EOF and inspect the `Arc-Session-Error` trailer (a JSON string on failure).
A 200 response alone does not prove successful session completion. The existing
bearer token and core installed-capability consent checks apply.

CLI examples, after installing an HTTP service that declares the mode:

```sh
arc session --http --mode server_stream 'http+arc://<provider>/events'
printf 'hello' | arc session --http 'http+arc://<provider>/upload' '{"stream_body":true}'
arc session --websocket 'http+arc://<provider>/socket' '{"subprotocols":["echo"]}'
```

The capability's manifest supplies the HTTP method; publish an appropriate
POST service for the upload example. `--http` writes decoded response bytes and
exits 22 for HTTP error statuses. It also rejects an `Arc-Session-Error` trailer
from a nested provider session. `--websocket` sends each input line as a text
message and displays received messages. Without these flags, `arc session`
exposes the record protocol directly, including headers and binary messages.
