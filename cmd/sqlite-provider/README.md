# SQLite over ARC provider

This bundle exposes selected SQLite databases through ARC request/reply
transport. It never selects a database path from a request. The operator maps
public database names to absolute local paths and grants each ARC public key a
`read` or `write` role.

## Configure

Install the provider's pinned Python runtime before serving it:

```sh
cd go/cmd/sqlite-provider
mise install
```

`SQLITE_CONFIG` is required and must be an absolute path to a regular JSON
file. The provider refuses to start without valid configuration.

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

From the repository root, serve the bundle with ARC's normal provider flow:

```sh
ARC_KEY=<provider-key> mise run arc -- serve go/cmd/sqlite-provider
```

Configure `ARC_RELAY` and `ARC_RELAY_PUBKEY` for the provider and citizen. To
share through onward relay partners, add `--federate-network` to `serve`.
Then use the active citizen identity:

```sh
mise run arc -- request 'sqlite+arc://<provider-public-key>/main' \
  --body '{"sql":"SELECT 1 AS n"}'
```

For explicitly local operation, run the provider without relay configuration
and add `--local` to `request`. The query goes through the same provider contract.

## Tests

From this provider directory:

```sh
mise exec -- python -m unittest discover -s tests -v
```

## Request protocol

The provider accepts ARC exec request events with `meta.method` `QUERY` and a
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
the provider caps request bodies and output at **1 MiB**, SQL at **1 MiB**,
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
The provider creates one transaction for every request and rolls it back on
every statement, limit, or result-serialization error.

The provider sees query text, parameters, caller public keys, result data, and
access patterns. It is not a private-compute boundary from its operator.
