# HTTP over ARC

Status: built. Two adapters serve HTTP over ARC:

- `provider.HTTP` serves a Go `http.Handler` in the provider program.
- `http-provider` serves an HTTP server of any language. See section 10.

`mise run compose` proves both. One notes service runs in the provider
program, in `examples/notes`, and as a plain HTTP server, in
`examples/notes-server`.

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

The manifest of `examples/notes` shows the reply as its status and its
content. A status of 4xx or 5xx exits 22, as `curl --fail` does.

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
The notes example keeps its notes in SQLite over ARC this way. See
docs/interface/SPEC.md, section 14.2. A server behind `http-provider` calls
through a local endpoint, see section 10.2.

## 10. The adapter for any language

`http-provider` starts an HTTP server as its child, and forwards each call
to it. The server can be in any language. It needs no ARC library:

```sh
http-provider <program> [args...]
arc serve 'exec:///usr/local/bin/http-provider?manifest=/srv/app/manifest.json&args=/srv/app/server'
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
  ends first, `http-provider` fails.
- The standard output of the server goes to the standard error of
  `http-provider`, because standard output carries the ARC protocol.
- One exchange takes at most 50 seconds. A server that does not answer in
  that time gives 502.
- If the server ends, `http-provider` ends, and `arc serve` says that the
  provider stopped.
- When `arc serve` stops, `http-provider` sends SIGTERM to the server. If
  the server has not ended after 3 seconds, it sends SIGKILL.
- If `http-provider` itself gets SIGKILL, the server stays behind.

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
