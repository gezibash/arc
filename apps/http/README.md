# HTTP app host

`arc-http` starts an HTTP program and exposes its service through core ARC.
The hosted app supplies its own interface manifest. There is no universal
manifest because routes, permissions and interaction modes belong to that app.

The implementation lives in `server/`; `cmd/arc-http` supplies process streams,
signals and exit status. Reusable HTTP mappings live in `sdk/httpadapter`.
[HTTP over ARC](../../docs/http/SPEC.md) defines the mapping of calls and
sessions.

## Run a server of any language

`arc-http` starts an HTTP server as its child, and forwards each call to it.
The server can be in any language. It needs no ARC library:

```sh
arc-http <program> [args...]
arc serve 'exec:///usr/local/bin/arc-http?manifest=/srv/app/manifest.json&args=/srv/app/server'
```

The child can stream bodies, and can serve SSE and WebSockets. Child stdout
and stderr are diagnostics; the host's stdout is reserved for the ARC
protocol.

## The server

The server gets three variables in its environment:

| Variable | Meaning |
| --- | --- |
| `PORT` | The port on `127.0.0.1` where the server listens. |
| `ARC_CALL_URL` | The endpoint for the calls of the server. See [Calls of the server](#calls-of-the-server). |
| `ARC_CALL_TOKEN` | The bearer token of each call. |

These rules hold:

- The server listens on `127.0.0.1:$PORT` in 30 seconds. If not, or if it
  ends first, `arc-http` fails.
- The standard output of the server goes to the standard error of
  `arc-http`, because standard output carries the ARC protocol.
- One exchange takes at most 50 seconds. A server that does not answer in
  that time gives 502. An earlier caller deadline applies.
- If the server ends, `arc-http` ends, and `arc serve` says that the
  provider stopped.
- When `arc serve` stops, `arc-http` sends SIGTERM to the server. If the
  server has not ended after 3 seconds, it sends SIGKILL.
- If `arc-http` itself gets SIGKILL, the server stays behind.

## Calls of the server

`ARC_CALL_URL` and `ARC_CALL_TOKEN` let the server call services installed by
its operating identity. The server calls a capability that its citizen
installed with one POST:

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

### Nested sessions

The same endpoint supports nested sessions. Calls and sessions use the shared
core protocol.

- A hosted application opens a session with an authenticated `POST /session`
  on the same local server as `ARC_CALL_URL`.
- The query parameters are `address`, `mode` (default `server_stream`), and
  `body` (the initial request).
- The HTTP body carries raw duplex input, and the response carries raw
  session output.
- Read to EOF, and inspect the `Arc-Session-Error` trailer (a JSON string on
  failure). A 200 response alone does not prove successful session
  completion.
- The existing bearer token and core installed-capability consent checks
  apply.

## Tests

```sh
mise exec -- go test ./apps/http/server
```
