# Notes: HTTP over ARC

This example is a `net/http` service in one program. `provider.HTTP` serves
it as the capability `http`, as docs/http/SPEC.md defines. It keeps its notes
in SQLite over ARC. It calls a `sqlite` capability that its citizen
installed, through `provider.Caller`, see docs/interface/SPEC.md, section
14.2.

Each caller sees only its own notes. The service reads the key of the caller
from the header field `Arc-Caller`. It has no login code.

The HTTP handlers are in `service`. `examples/notes-server` serves the same
handlers as a plain HTTP server, behind `http-provider`.

## Prove it

```sh
mise run compose
```

The proof starts a relay, the sqlite provider, and both notes examples. Two
callers use each one. Only the notes services hold a grant on the databases.

## Serve it

1. Serve a sqlite provider that grants the key of this service `write`. See
   docs/sqlite/SPEC.md.
2. Install that provider as the citizen that serves this service:

   ```sh
   arc install <sqlite-provider-key> --yes
   ```

3. Serve this directory. `NOTES_DB` names the database:

   ```sh
   NOTES_DB='sqlite+arc://<sqlite-provider-key>/main' arc serve examples/notes
   ```

A caller installs the service, and keeps a note:

```sh
arc install <notes-key> --as notes --yes
arc notes add hello
arc notes list
arc call --method POST 'http+arc://<notes-key>/notes' '{"body": "hello"}'
```
