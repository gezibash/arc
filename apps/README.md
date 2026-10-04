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

## The Arcfile

`arc serve <app-directory>` reads the `Arcfile` of the directory. The file
says how the program goes into ARC, and how the program goes out of ARC. It
does not build the program. The operator builds it.

```toml
version = 2

# Into ARC: the program, its protocol, its manifest and its callers.
[serve]
command = "./run.sh"
args = []
cwd = "."
protocol = "stdio"
manifest = "./manifest.json"
allow = ["<key>", "<key>"]

# Out of ARC: the installed apps that the program calls.
[uses]
warehouse = "warehouse"
```

| Field | Meaning |
| --- | --- |
| `version` | Must be 2. |
| `serve.command` | Required. The program. A name with a slash is a path from `cwd`. A name without a slash comes from `PATH`, as in a shell. |
| `serve.args` | The arguments of the program. No shell reads them, so `$PORT` stays as text. |
| `serve.cwd` | The directory of the program, from the directory of the `Arcfile`. The default is the directory of the `Arcfile`. |
| `serve.protocol` | The protocol of the program. The default is `stdio`, see [The stdio protocol](#the-stdio-protocol). Another value runs the translator `arc-<protocol>`, and the translator runs the program. |
| `serve.manifest` | Required. The interface manifest that `arc serve` announces. |
| `serve.allow` | Optional. The callers that reach the program, as keys in each form that a command takes. Leave it out to allow every citizen. |
| `[uses]` | Optional. One line for each installed app that the program calls: `<name> = "<installed name>"`. |

Rules:

- A field that version 2 does not define is an error. An `Arcfile` of
  version 1 is an error. The error message gives the steps of the rewrite.
- `arc serve` finds the translator `arc-<protocol>` in the directory of
  `arc` first, and then in `PATH`. A release puts `arc-http` beside `arc`.
  If there is no translator, `arc serve` does not start.
- If a caller is not in `allow`, the caller gets `access_denied`. The
  program does not see the request or the session. `allow` comes before the
  checks of the app. The exec app keeps its own grants in `EXEC_CONFIG`.
- For each name in `[uses]`, the program gets the variable `ARC_USE_<NAME>`
  with the address of the app, for example
  `ARC_USE_WAREHOUSE=warehouse+arc://<key>/`. A `-` in the name becomes
  `_`.
- If `[uses]` is present, a call or a session of the program to another app
  fails with `not_in_uses`. An empty `[uses]` allows no call. If `[uses]` is
  absent, the program can call each app that its operating identity
  installed.
- The operating identity must install each app that `[uses]` names. If not,
  `arc serve` does not start. `[uses]` names installed apps, not keys, so
  one `Arcfile` works for each operator.
- `arc serve` reads `allow` and `[uses]` when it starts. After a change,
  restart `arc serve`.

An HTTP server, for example FastAPI, needs no ARC library. The translator
`arc-http` gives it `PORT`, see [the HTTP app](http/README.md):

```toml
version = 2

[serve]
command = "sh"
args = ["-c", "uvicorn main:app --host 127.0.0.1 --port $PORT"]
protocol = "http"
manifest = "./manifest.json"
```

## The stdio protocol

`arc serve` starts the program of an app, and speaks to it on standard
input and standard output. Each message is one line: one JSON object,
then a newline. The program writes its log to standard error, and
`arc serve` copies that log to its own log.

`arc serve` holds the key, signs and verifies events, and uses the relays.
The program holds no key and opens no ARC connection. Thus a program in any
language can serve an app. The Go package `sdk/provider` implements this
protocol for Go programs. The type `wire.Event` in `core/wire` defines each
field.

### Start and stop

- `arc serve` runs the `command` and `args` of the `Arcfile`, in the
  directory that `cwd` names. [The Arcfile](#the-arcfile) says how
  `command` resolves.
- The program gets the environment of `arc serve`, and two more variables:
  `ARC_IDENTITY` is the name of the operating identity, and
  `ARC_PUBLIC_KEY` is its public key in hex.
- To stop the program, `arc serve` closes standard input. The program must
  then finish its work and exit. If the program runs 5 seconds after
  that, `arc serve` kills it.

### Requests

`arc serve` writes one line for each call that reaches the app:

```json
{"op":"request","request_id":"9f2c…","from":"<64 hex>","message":"Tirana","meta":{"method":"RAW","path":"/","capability":"weather"},"framed":true,"deadline_ms":1791112345000}
```

| Field | Meaning |
| --- | --- |
| `request_id` | A string. Copy it exactly into the answer. |
| `from` | The public key of the caller, in hex. ARC verified the signature. |
| `message` | The body of the request, as text. |
| `meta` | The `method` and `path` of the call, and the `capability` id. |
| `deadline_ms` | The deadline of the request, in Unix milliseconds. |
| `framed` | Ignore this field. |

The program writes one answer for each request:

```json
{"op":"reply","request_id":"9f2c…","reply":"{\"forecast\":\"sunny\"}"}
{"op":"error","request_id":"9f2c…","error":"unknown_city"}
```

Rules:

- `reply` is text. To send JSON, encode it as a string.
- If `error` is not empty, the request failed. The caller sees
  `the provider refused: <error>`. Use a short code, for example
  `access_denied`.
- An answer with no `reply` and no `error` gives the caller `invalid_reply`.
- Requests can arrive before the program answers earlier requests. The
  program can answer in any order.
- `arc serve` does not check grants. Any citizen can send a request. If the
  app must limit its callers, compare `from` with a list of keys.
- `arc serve` ignores an answer to an unknown `request_id`.

### Cancel

When the caller stops waiting, or the deadline passes, `arc serve` writes:

```json
{"op":"cancel","request_id":"9f2c…"}
```

The program must stop the work of that request. `arc serve` ignores a later
answer to it. The caller gets no answer. Its `arc` reports that the outcome
is unknown, because the work can have run. A live request has at most
120 seconds.

### Calls to other apps

A program can call an app that its operating identity installed. `arc serve`
signs the call. The program writes:

```json
{"op":"call","call_id":"7","address":"geo+arc://<key>/","body":"Tirana"}
```

Each active call needs its own `call_id`. `arc serve` writes one result with
the same `call_id`:

| Result | Meaning |
| --- | --- |
| `{"op":"result","call_id":"7","reply":"…"}` | The reply of the other app. |
| `{"op":"result","call_id":"7","refused":"…"}` | The other app answered with an error. |
| `{"op":"result","call_id":"7","error":"…"}` | `arc serve` could not make the call, for example `not_installed`. |

To stop a call, the program writes `{"op":"cancel","call_id":"7"}`. The
section "Calls of a provider" of [the interface spec](../docs/interface/SPEC.md)
gives the deadline rules.

### Sessions

A session line has `"op":"session"`, and carries one frame of
[the session protocol](../docs/sessions/SPEC.md). `arc serve` sends session
lines for the modes in `service.interactions` of the manifest. Only Go has a
library for sessions: `sdk/provider`.

A manifest with no `interactions` permits `request_reply`. A caller can
still open a `request_reply` session, for example with
`arc session --mode request_reply`. That session arrives as a session line
with the frame op `open`, not as a request line. If the program does not
serve sessions, it must refuse each `open` frame. Otherwise the caller
waits until its timeout. Copy the `id` of the frame into `request_id` and
into the frame:

```json
{"op":"session","request_id":"<id>","session":{"version":1,"id":"<id>","op":"close","error":"unsupported"}}
```

### Rules for lines

- Standard output carries protocol lines only. A line that is not JSON is
  dropped, and `arc serve` logs a warning.
- Write each line in one write. Two threads must not write parts of their
  lines between each other.
- A line from the program can hold at most 64 MiB. `arc serve` drops a
  longer line.
- Ignore a line with an `op` that the program does not know.
