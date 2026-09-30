# ARC apps

An app supplies commands through an interface manifest. It can also include a
service program. Apps compose through core ARC calls, sessions and events.

| App | Commands and execution |
| --- | --- |
| [Journal](journal/README.md) | Local encrypted Markdown notebooks, navigation, indexes and search. |
| [SQLite](sqlite/README.md) | Manifest-generated client commands; a service program executes SQL and REPL sessions. |
| [Exec](exec/README.md) | Manifest-generated client commands; a service program runs granted processes and terminals. |
| [HTTP](http/README.md) | Hosts an HTTP program and exposes its own manifest through ARC calls and sessions. |
| [Releases](releases/README.md) | Reads signed update channels and serves archive chunks or streams. |
| [Direct messages](dm/manifest.json) | Local commands that send and read private events. |
| [Files](files/manifest.json) | Local commands that store and read sealed files. |
| [Agora](agora/manifest.json) | Commands that publish and read events on a configured group relay. |

## Terms

| Term | Meaning |
| --- | --- |
| App | A named package of commands, interface definitions and optional programs. |
| Command | An operation invoked through `arc <installed-name> <command>`. |
| Program | Executable code; the release includes `arc-exec`, `arc-sqlite`, `arc-http` and `arc-releases`. |
| Instance | A running program with an operating identity and configuration. |
| Service | An interface that an instance makes available to callers. |
| Session | One request/reply, server stream or duplex interaction. |
| Adapter | Concrete transport, process, protocol or storage integration. |

Client/server and provider/consumer are interaction roles. The same participant
can serve one app and call another. The signer of an interface announcement is
the manifest author; for a service, it is currently also its operating identity.
Installing that interface records trust in that signer and its declared
permissions. It does not establish the provenance of a downloaded program.

## Layout

Manifest-driven data apps need only `manifest.json` and their documentation.
They run through the shared application runtime. Service apps also have an
Arcfile, a launcher, a Go package in `server/`, and a program in
`apps/<name>/cmd/arc-<name>`. The program supplies streams, signals and the
process exit status.

The client commands are generated from the manifest. Add a client package only
when an app actually needs its own client code. App source does not belong in
core. Runtime libraries and adapters do not depend on concrete apps.

Each built-in interface has one canonical `manifest.json`. Historical external
bundles with an adjacent `interface.json` still load through the existing parser.

## Commands

```sh
arc discover
arc install <author-or-service> <app-id>
arc apps list
arc apps info <installed-name>
arc help <installed-name>
arc apps remove <installed-name>

arc apps init ./weather
arc serve ./weather
```

`arc install` installs interface commands and records consent. It does not
download a program or start an instance. The operating identity selects which
program to run with `arc serve`. Executable distribution and supervision are
separate concerns.

Command results go to stdout and diagnostics to stderr. Formats such as JSON,
NDJSON and raw bytes permit ordinary shell composition. A hosted program's
stdout carries the ARC stdio protocol; it writes logs to stderr.

Live sessions require a delivery adapter that declares live support. EOF on a
stream is separate from its final outcome. A failed connection ends the session;
it does not silently replay SQL, restart a command or resume a transfer. Signed
events and carried calls remain available for durable or offline delivery.

## Sessions of the bundled apps

The [session spec](../docs/sessions/SPEC.md) defines the protocol. To try the
sessions of the bundled apps, build with `mise run build`, and use its
`bin/arc` and app programs. A release older than this one has no
`arc session`. Configure a relay on each identity. Serve each app with its
`manifest.json`, then install it on the consumer. Install it again after an
interface update.

| App | Initial request | Modes | Input and output |
| --- | --- | --- | --- |
| SQLite | Empty for a REPL, or SQL/JSON for a query | request_reply, server_stream, duplex | One SQL statement or JSON query per line; NDJSON result records. |
| Exec | JSON argv/script and command options | request_reply, server_stream, duplex | Process I/O records; `--exec` decodes them, `--tty` handles terminals. |
| HTTP | HTTP envelope, optionally stream_body or websocket | request_reply, server_stream, duplex, as declared by the application | HTTP/WS records; `--http` decodes bodies, `--websocket` maps lines to text messages. |
| Releases | JSON archive operation and sha256 digest | request_reply, server_stream | Raw archive bytes; updater checks size, hash and final session completion. |

A session uses the same configuration and access checks as a single call. See
the READMEs of [SQLite](sqlite/README.md), [Exec](exec/README.md),
[HTTP](http/README.md) and [Releases](releases/README.md).

A quick local proof builds real provider subprocesses, runs a local relay and
separate identities, and uses the normal CLI:

```sh
mise exec -- go test ./cmd/arc -run TestBundledProviderSessionsThroughCLI -count=1
```

It covers SQL state, SSE, WebSocket echo, HTTP-to-SQLite sessions, incremental
process I/O, the terminal CLI and terminal restoration, and archive transfer.
`TestDelivery` in `internal/proof` also checks a signed archive update through
both the existing chunk path and the installed provider's streaming path.

Core owns authentication, framing, flow control and session lifetime. Apps own
SQL transactions, subprocesses, HTTP mappings and file access:

- A lost session never implicitly repeats a write. Close or EOF does not
  commit a SQL transaction.
- Exec jobs remain independent of live sessions. Exec retains its configured
  process timeout.
- Long sessions remain subject to core's 30-minute cap and any shorter
  provider limit.
- WebSocket messages are capped at 1 MiB. Streaming HTTP bodies have no
  aggregate byte cap, but their buffering and lifetime are bounded.
- No session resumes automatically after disconnection.
- Archive verification still requires the signed publisher metadata. The
  updater streams transport data, but retains the archive for the existing
  in-memory unpacker.
- The one-chunk credit window favors bounded memory. Local tests do not
  establish throughput over a wide-area network.

## An app in another repository

An app needs no code from this directory. Each app here obeys the rules that
an app in another repository obeys:

- The app holds a manifest, an `Arcfile`, and a program that reads and writes
  JSON lines on standard input and output. `arc apps init` writes a starter.
- A Go app imports only `github.com/gezibash/arc/sdk/...`. `sdk/provider` is
  the runtime, `sdk/stdio.Main` is the entry point, and `sdk/providertest`
  gives a real session for tests.
- An operator runs the app with `arc serve <app-directory>`. A caller gets its
  commands with `arc install <key>`.

`internal/architecture` fails if an app here imports another package of this
module.
