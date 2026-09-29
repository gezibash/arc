# HTTP app host

`arc-http` starts an HTTP program and exposes its service through core ARC.
The hosted app supplies its own interface manifest. There is no universal
manifest because routes, permissions and interaction modes belong to that app.

The implementation lives in `server/`; `cmd/arc-http` supplies process streams,
signals and exit status. Reusable HTTP mappings live in `adapters/http`.

The child listens on `127.0.0.1:$PORT`. `ARC_CALL_URL` and `ARC_CALL_TOKEN` let it
call services installed by its operating identity. The same endpoint supports
nested sessions. Calls and sessions use the shared core protocol.

Serve the [Notes example](../../examples/notes-server/README.md) with its Arcfile,
or use the [streaming HTTP example](../../examples/streaming-http/README.md) for
streamed bodies, SSE and WebSockets. Child stdout and stderr are diagnostics;
the host's stdout is reserved for the ARC protocol.

```sh
mise exec -- go test ./apps/http/server
mise run compose
```
