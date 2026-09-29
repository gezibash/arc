# A stateful REPL over core ARC

This example is an in-memory key/value REPL. It runs no shell commands or user
code. Every session gets its own variables; ending the session discards them.
The handler contains application behavior only. Core handles framing, identity,
ordering, flow control, cancellation and final completion.

Build the example from the repository root:

```sh
mise exec -- go build -o /tmp/arc-repl ./examples/repl
```

On a provider ARC identity with a configured relay, serve it using the absolute
path to this directory's `interface.json`:

```sh
arc serve 'exec:///tmp/arc-repl?manifest=/absolute/path/to/arc/examples/repl/interface.json'
```

From a consumer identity using the same relay, install the advertised provider:

```sh
arc install <provider-public-key> repl --yes
arc session 'repl+arc://<provider-public-key>/'
```

The provider first prints `ready`. Enter `SET answer 42`, then `GET answer`.
The replies are `ok` and `42`. `QUIT`, input EOF or cancellation ends the session.
A new session has no `answer` variable. Each line is limited to 4 KiB and each
session to 128 variables. This demonstration allows any authenticated caller;
a private provider must apply its own caller access policy before handling work.

The same provider supports `--mode server_stream`, which streams its command
help, and ordinary `arc call` request/reply. No transport-specific code appears
in the provider. HTTP, WebSocket, terminal/PTY adapters and durable session
resumption are separate work; this example uses the existing relay and stdio.

Validation: `go test ./cmd/arc -run TestCLISession -count=1` exercises two ARC
homes, a real local relay and this executable provider, through the normal CLI.
