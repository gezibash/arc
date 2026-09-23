# Notes server: HTTP over ARC for any language

This example is the notes service of `examples/notes`, as a plain HTTP
server. It uses no ARC library. `http-provider` starts it, and serves it over
ARC as the capability `http`. See docs/http/SPEC.md, section 10.

A server in another language does the same three things:

1. It listens on `127.0.0.1:$PORT`.
2. It reads the key of the caller from the header field `Arc-Caller`.
3. It calls a capability with one POST to `$ARC_CALL_URL`, with the token
   `$ARC_CALL_TOKEN`. This server keeps its notes in SQLite over ARC this
   way.

The two notes examples share their HTTP handlers, in
`examples/notes/service`, and their manifest, in `examples/notes`.

## Prove it

```sh
mise run compose
```

The proof runs both notes examples against one relay. Each passes the same
checks.

## Serve it

1. Serve a sqlite provider that grants the key of this service `write`. See
   docs/sqlite/SPEC.md.
2. Install that provider as the citizen that serves this service:

   ```sh
   arc install <sqlite-provider-key> --yes
   ```

3. Serve this directory. `NOTES_DB` names the database:

   ```sh
   NOTES_DB='sqlite+arc://<sqlite-provider-key>/main' arc serve examples/notes-server
   ```

`run.sh` builds `http-provider` and this server, and starts the server
through `http-provider`.
