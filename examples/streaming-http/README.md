# HTTP sessions through an ordinary web application

Build `arc`, `arc-http`, and this example from this branch. Configure provider
and consumer identities on the same relay, then serve:

```sh
mise exec -- go build -o /tmp/arc-http ./cmd/arc-http
mise exec -- go build -o /tmp/streaming-http ./examples/streaming-http
arc serve "exec:///tmp/arc-http?manifest=$PWD/examples/streaming-http/interface.json&args=/tmp/streaming-http"
```

On the consumer:

```sh
arc install <provider-public-key> --yes
arc session --http --mode server_stream 'http+arc://<provider>/events'
arc session --websocket 'http+arc://<provider>/socket'
```

The event endpoint emits two SSE events. The socket prints `ready`, then echoes
each line. EOF closes it. The `/echo` endpoint streams request bytes back.
The `/nested` endpoint demonstrates the local authenticated session API using
query parameters `address`, `mode`, and `body`. Its provider identity must install
and hold any required grant on the downstream capability. This example allows
any authenticated ARC caller to invoke its endpoints; restrict callers in a
private application before enabling nested calls to private capabilities.
