# Running the bundled session providers

Build this branch with `mise run build`. Use its `bin/arc` and provider binaries;
an older installed ARC binary does not contain these commands. Configure a relay
on each identity. Serve each provider with its `interface.json`, then install it
from the consumer. Repeat installation after an interface update.

| Provider | Initial request | Modes | Input and output |
| --- | --- | --- | --- |
| SQLite | Empty for a REPL, or SQL/JSON for a query | request_reply, server_stream, duplex | One SQL statement or JSON query per line; NDJSON result records. |
| Exec | JSON argv/script and command options | request_reply, server_stream, duplex | Process I/O records; `--exec` decodes them, `--tty` handles terminals. |
| HTTP | HTTP envelope, optionally stream_body or websocket | request_reply, server_stream, duplex, as declared by the application | HTTP/WS records; `--http` decodes bodies, `--websocket` maps lines to text messages. |
| Releases | JSON archive operation and sha256 digest | request_reply, server_stream | Raw archive bytes; updater checks size, hash and final session completion. |

Provider configuration and access checks are unchanged. See the guides for
[SQLite](../../cmd/sqlite-provider/README.md), [Exec](../../cmd/exec-provider/README.md),
[streaming HTTP](../../examples/streaming-http/README.md), and
[Releases](../../cmd/releases-provider/README.md).

A quick local proof builds real provider subprocesses, runs a local relay and
separate identities, and uses the normal CLI:

```sh
mise exec -- go test ./cmd/arc -run TestBundledProviderSessionsThroughCLI -count=1
```

It covers SQL state, SSE, WebSocket echo, HTTP-to-SQLite sessions, incremental
process I/O, the terminal CLI and terminal restoration, and archive transfer.
`scripts/test-delivery.sh` also checks a signed archive update through both the
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
