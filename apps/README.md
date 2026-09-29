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
Arcfile, a launcher and a Go package in `server/`. Executable entry points stay
in `cmd/arc-<name>` and supply streams, signals and process exit status.

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
