# Sessions of the bundled apps

Build with `mise run build`, and use its `bin/arc` and app programs. A release
older than this one has no `arc session`. Configure a relay on each identity.
Serve each app with its `manifest.json`, then install it on the consumer.
Install it again after an interface update.

| App | Initial request | Modes | Input and output |
| --- | --- | --- | --- |
| SQLite | Empty for a REPL, or SQL/JSON for a query | request_reply, server_stream, duplex | One SQL statement or JSON query per line; NDJSON result records. |
| Exec | JSON argv/script and command options | request_reply, server_stream, duplex | Process I/O records; `--exec` decodes them, `--tty` handles terminals. |
| HTTP | HTTP envelope, optionally stream_body or websocket | request_reply, server_stream, duplex, as declared by the application | HTTP/WS records; `--http` decodes bodies, `--websocket` maps lines to text messages. |
| Releases | JSON archive operation and sha256 digest | request_reply, server_stream | Raw archive bytes; updater checks size, hash and final session completion. |

A session uses the same configuration and access checks as a single call. See
the guides for
[SQLite](../../apps/sqlite/README.md), [Exec](../../apps/exec/README.md),
[HTTP](../../apps/http/README.md), and
[Releases](../../apps/releases/README.md).

A quick local proof builds real provider subprocesses, runs a local relay and
separate identities, and uses the normal CLI:

```sh
mise exec -- go test ./cmd/arc -run TestBundledProviderSessionsThroughCLI -count=1
```

It covers SQL state, SSE, WebSocket echo, HTTP-to-SQLite sessions, incremental
process I/O, the terminal CLI and terminal restoration, and archive transfer.
`TestDelivery` in `internal/proof` also checks a signed archive update through both the
existing chunk path and the installed provider's streaming path.

Core owns authentication, framing, flow control and session lifetime. Providers
own SQL transactions, subprocesses, HTTP mappings and file access. A lost session
never implicitly repeats a write. Close or EOF does not commit a SQL transaction.
Exec jobs remain independent of live sessions. Long sessions remain subject to
core's 30-minute cap and any shorter provider limit. Exec retains its configured
process timeout. WebSocket messages are capped at 1 MiB; streaming HTTP bodies
have no aggregate byte cap, but their buffering and lifetime are bounded.

No session resumes automatically after disconnection. Archive verification still
requires the signed publisher metadata. The updater streams transport data but
retains the archive for the existing in-memory unpacker. The one-chunk credit
window favors bounded memory, and WAN throughput is not established by local tests.
