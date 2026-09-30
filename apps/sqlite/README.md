# SQLite app

The [manifest](manifest.json) generates client commands. `server/` owns SQL policy, grants and
REPL connection state. `cmd/arc-sqlite` is the executable entry point. The
service can run on the same machine as its client or on another participant.

This bundle exposes selected SQLite databases through ARC request/reply
transport. It never selects a database path from a request. The operator maps
public database names to absolute local paths and grants each ARC public key a
`read` or `write` role.

## Configure

`SQLITE_CONFIG` is required and must be an absolute path to a regular JSON
file. The service refuses to start without valid configuration.

```json
{
  "databases": {
    "main": {
      "path": "/srv/arc/sqlite/main.db",
      "grants": {
        "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa": "write",
        "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb": "read"
      }
    }
  },
  "limits": {
    "body_bytes": 262144,
    "sql_bytes": 65536,
    "batch_statements": 32,
    "rows": 1000,
    "output_bytes": 1048576,
    "query_ms": 5000,
    "busy_ms": 1000
  }
}
```

Database names use lowercase letters, digits, `_`, and `-`; `main` is invoked
as the ARC path `/main`. Keys are exactly 64 lowercase hexadecimal characters.
The operator can give one citizen a private database or give several citizens
access to one shared database by changing this mapping.

```sh
export SQLITE_CONFIG=/absolute/path/sqlite.json
```

Serve the service with `arc serve`. The identity of the service must have
a relay:

```sh
arc serve "exec://$(command -v arc-sqlite)?manifest=$PWD/apps/sqlite/manifest.json"
```

A citizen adds the same relay, installs the capability, and queries it:

```sh
arc install <service-public-key> --yes
arc sqlite select 1 as n
```

## Tests

From the repository root:

```sh
go test ./apps/sqlite/server
```

## Request protocol

The service accepts ARC exec request events with `meta.method` `QUERY` and a
configured `meta.path`, for example `/main`. `message` is UTF-8 JSON:

```json
{"sql":"SELECT id, body FROM notes WHERE id = ?","params":[7]}
```

An atomic batch uses `statements`:

```json
{"statements":[{"sql":"INSERT INTO notes(body) VALUES(?)","params":["hello"]},{"sql":"SELECT count(*) AS count FROM notes"}]}
```

Every reply contains `{"results":[...]}`. Each result has `columns`, `rows`,
and `changes`. BLOB output is represented as `{"base64":"..."}`; use that
same one-key object to bind a BLOB parameter.

The default limits are **256 KiB** request bodies, **64 KiB** SQL, **32** batch
statements, **1,000** result rows, **1 MiB** encoded output, **5 seconds** for
the whole request, and **1 second** SQLite busy wait. Operators may reduce any limit;
the service caps request bodies and output at **1 MiB**, SQL at **1 MiB**,
batches at **256** statements, rows at **10,000**, query time at **30 seconds**,
and busy waits at **10 seconds**. The body limit covers the whole JSON request,
including every statement in a batch. The output limit covers the entire reply.
The query deadline includes the batch, result conversion, and reply validation;
the SQLite busy wait is shortened to the remaining query time. Results are
rejected rather than truncated.

## Safety boundary

Authorization is enforced by SQLite itself. Read keys use a read-only SQLite
connection and an authorizer that denies writes. All callers are denied
`ATTACH`, `DETACH`, `PRAGMA`, extension loading, and caller transaction control.
The service creates one transaction for every request and rolls it back on
every statement, limit, or result-serialization error.

The service answers calls of the [delivery layer](../../docs/delivery/SPEC.md),
section 11.4, and announces the `sqlite` scheme. It is a query service backed
by SQLite. It is not a network filesystem for `.db` files, and it does not
change the stock SQLite client.

Operators can map several citizens to a shared database, or map separate
resource names to separate databases with different grants. There is no
implicit public database, and no automatic database creation for strangers.
Named resources such as `/main` resolve only through operator configuration.
The URI cannot choose a filesystem path.

Each request is one provider-owned transaction, including single statements
and atomic batches. Results must fit the configured limits and serialize
successfully before commit. A lost reply after commit remains an uncertain
outcome at the transport layer, and is never retried automatically.

The service sees query text, parameters, caller public keys, result data, and
access patterns. It is not a private-compute boundary from its operator.


## Live SQL sessions

Use the session-capable ARC binary and serve `manifest.json` from this directory.
After installing the service, open a connection:

```sh
arc session --timeout 10m 'sqlite+arc://<service-public-key>/main'
```

Enter one SQL statement per line, or one JSON query/batch per line (JSON can
carry multiline SQL and parameters). `.quit` or EOF ends the session. Output is
NDJSON: `ready`, then `columns`, `row`, `statement`, and `done` records. Each query
is bounded by the configured row, byte and execution limits. An `error` record
ends that query; the connection stays usable. Rows are provisional until `done`.

The connection retains temporary tables. `BEGIN`, `BEGIN TRANSACTION`, `COMMIT`,
`END`, and `ROLLBACK` are explicit session commands. Every ordinary query/batch
uses a savepoint and rolls back on failure, including output-limit failures.
`done.transaction` says whether an explicit transaction remains open. EOF,
cancellation and disconnection never commit that transaction. Existing grants
and denials of ATTACH, PRAGMA, and arbitrary transaction/savepoint SQL still apply.

For one streaming query:

```sh
arc session --mode server_stream 'sqlite+arc://<service-public-key>/main' \
  '{"sql":"SELECT 42 AS answer"}'
```

Server-stream errors are final session failures. Duplex query errors are records.
A failure after a commit can leave its outcome unknown; never retry writes
implicitly. Ordinary `arc sqlite` and request/reply calls retain their atomic,
buffered behavior and existing limits.
