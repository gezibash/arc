# ARC in Go

Status: the port is done, except the host service and the TCP hole punch. ARC
runs end to end in Go. Go holds the repository, and the Elixir tree is gone.
This plan records the port, and section 5 records the proofs that it passed.

```bash
mise run test    # the tests
mise run lint    # gofmt and go vet
mise run build   # every command into bin/
mise run cli     # the whole stack: relay, citizen, provider, caller
```

## 1. Purpose

Today ARC is one program. A person who wants a citizen, a provider, or a relay
installs the ARC release. That release carries its own Erlang runtime.

In Go, ARC becomes a set of packages and a few small binaries:

```go
import "github.com/gezibash/arc/provider"
import "github.com/gezibash/arc/client"
```

A person writes a provider in 50 lines, builds one static binary, and copies
it to any machine. Nothing else is installed.

This plan serves three goals:

1. A citizen machine needs one file, and no language runtime.
2. Another person builds on ARC without this repository.
3. One language covers the core, the providers, and the tools.

## 2. Size

| Part | Elixir lines | Tests |
| --- | --- | --- |
| `arc_cli` | 10939 | 9227 |
| `arc_data` | 10049 | 4350 |
| `arc_net` | 9031 | 5599 |
| `arc_identity` | 705 | 665 |
| `arc_provider` | 419 | 332 |
| `arc_control` | 243 | 128 |
| `arc_storage` | 38 | 9 |
| Providers | 5865 | included above |

Go needs about 1.3 times the lines of Elixir for the same work. The port is
therefore about 45000 lines, with about 25000 lines of tests.

## 3. What this port drops

- **The hot upgrade of a running relay.** BEAM replaces code under live
  connections. Go does not. A relay restarts, and its clients reconnect. The
  update engine in `arc_cli/lib/arc/cli/update` of the Elixir tree becomes a smaller
  program: download, verify, replace the binary, restart.
- **Live introspection of a running node.** A relay operator reads logs and
  metrics instead of a remote shell.
- **Supervision trees.** A goroutine that panics takes the process down unless
  the code recovers. Each long-running goroutine needs an explicit recover and
  restart.

CAUTION: Do not start this port while the wire format changes every week. Each
change then lands two times, in two languages.

## 4. Packages

The module is `github.com/gezibash/arc`. Each package holds one concept.

| Package | Contents | Public? |
| --- | --- | --- |
| `identity` | Seeds, Ed25519 keys, the X25519 conversion, petnames, the key store | Yes |
| `announce` | The signed announcement that a relay directory holds | Yes |
| `frame` | The request and response frame of the handler protocol | Yes |
| `sealedbox` | Seal to a public key, open with an identity | Yes |
| `session` | The version 2 session, its ephemeral key, and its packet key | Yes |
| `packet` | Framing, the header, the sequence, and the replay guard | Yes |
| `capability` | The manifest, the signed capability package, and its interfaces | Yes |
| `citizen` | The serving citizen: the provider process, the sessions, and the manifest | Yes |
| `relays` | The pinned relay of this machine | Yes |
| `toolbox` | The capabilities that a citizen installed, the signers it trusts, and the command line of each one | Yes |
| `control` | The local answer to who an identity is | Yes |
| `lists` | The saved sets of peers of one machine | Yes |
| `direct` | The carrier that two citizens hold between themselves, and the rules that allow it | Yes |
| `control` | The local control plane: publish, resolve, revoke | Yes |
| `client` | Connect to a relay, announce, discover, request, listen | Yes |
| `provider` | The provider runtime, the interface, the configuration helpers, and jobs | Yes |
| `relay` | The relay server, its directory, and federation | Yes |
| `internal/wire` | The relay framing and the handshake | No |
| `internal/canonical` | The deterministic JSON that a signature covers | No |

Binaries live under `cmd`:

| Binary | Purpose |
| --- | --- |
| `cmd/arc` | The command line tool. keys, join, status, serve, call, discover, resolve. |
| `cmd/arc-relay` | A relay, for a server or a container. Done. |
| `cmd/exec-provider` | The exec provider. Done. |
| `cmd/dm-provider` | The DM provider. Done. |
| `cmd/sqlite-provider` | SQL over ARC. Done. |
| `cmd/files-provider` | Private files. Done. |
| `cmd/agora-provider` | The public board. Done. |
| `cmd/journal-provider` | The notebooks. Done. |
| `cmd/releases-provider` | The release channels. Done. |

### 4.1 The provider interface

```go
package provider

type Request struct {
    From      string
    Method    string
    Path      string
    Body      []byte
    RequestID any
}

type Provider interface {
    HandleRequest(ctx context.Context, r Request) ([]byte, error)
}

// Run reads events from in and writes replies to out until in closes.
func Run(ctx context.Context, p Provider, opts ...Option) error
```

A provider in full:

```go
type echo struct{}

func (echo) HandleRequest(_ context.Context, r provider.Request) ([]byte, error) {
    return r.Body, nil
}

func main() {
    provider.Run(context.Background(), echo{})
}
```

### 4.2 The client interface

```go
package client

func Dial(ctx context.Context, relay string, pin identity.PublicKey, me *identity.Identity) (*Client, error)

func (c *Client) Request(ctx context.Context, uri string, body []byte) ([]byte, error)
func (c *Client) Serve(ctx context.Context, bundle string) error
func (c *Client) Discover(ctx context.Context, query string) ([]capability.Summary, error)
```

## 5. Two implementations, one protocol

The Elixir code ran beside the Go code for the whole port. The wire format
was the contract between them. A harness ran both directions at each step,
and every check passed at commit `2204d44`, which is the last commit that
held both implementations:

| Check | Result |
| --- | --- |
| Go client, Elixir relay | Passed. The Go packets and announcements are valid. |
| Elixir clients, Go relay | Passed. The Go relay answers the packets of today. |
| Go provider, Elixir `arc serve` | Passed. The provider protocol matches. |
| Go relay, Elixir relay, federated | Passed. A Go client reached an Elixir citizen across two relays. |
| Go client, Go relay | Passed. `mise run cli`, which still runs. |

The Elixir tree went at the commit after that one. Only `mise run cli`
remains, because it needs nothing else.

Shared test vectors hold the parts that must not drift:

- An identity from a seed, its public key, and its X25519 key.
- A sealed box, and the plain text that it holds.
- A signed relay announcement, and its canonical bytes.
- A capability package, and its hash.

The vectors live in `test/vectors/identity.json`. The Elixir implementation
wrote that file, and Go reads it in `internal/vectors`. Nothing generates it
now, so it is a fixture. A change to a vector is a change to the protocol,
and needs a version. See `test/vectors/README.md`.

The module path is `github.com/gezibash/arc`.

## 6. Order of work

| Phase | Scope | Proof |
| --- | --- | --- |
| 1 | `identity`, `sealedbox`, `session`, `packet` | Done. The shared vectors pass. |
| 2 | `client`: connect, announce, discover, request | Done. A Go client talks to the Elixir relay. |
| 3 | `provider`, and the exec provider | Done. The Elixir `arc serve` runs the Go provider. |
| 4 | `relay`: sessions, directory, routes | Done. An Elixir client talks to the Go relay. |
| 5 | Federation, direct connections | Done. Two Go relays federate, and a Go relay federates with an Elixir relay. A conversation leaves the relay and outlives it. The TCP hole punch of the Elixir release is not ported. |
| 6 | `cmd/arc`: the command surface of today | Done, except the host and the update engine. keys, join, status, serve, call, discover, resolve, info, send, listen, publish, lists, install, tool, trust, and the commands that an install adds. |
| 7 | The remaining providers | Done: exec, dm, sqlite, files, releases, agora and journal. MCP is deprecated, and does not go to Go. |

Each phase is one pull request. A phase that does not pass its proof does not
merge.

## 7. What ARC keeps

- The wire format, byte for byte.
- The identity model: the seed is the identity, and the public key is the
  address.
- `Arcfile`, `manifest.json`, and the grant model of each provider.
- The specs in `docs/`. They describe the protocol, not the language.

## 8. Risks

| Risk | Answer |
| --- | --- |
| The protocol changes during the port. | Freeze the wire format first. Land protocol changes in Elixir, then port them. |
| A panic stops a relay. | Every goroutine that serves a connection recovers and logs. One connection never stops the process. |
| The SQLite provider needs the authorizer with its arguments. | Answered. `zombiezen.com/go/sqlite` is pure Go and gives a typed authorizer. It does not name the function of an OpFunction action, so the provider leans on SQLite itself, which refuses to load an extension. A test proves it. |
| cgo breaks the static binary. | Answered. No package of the port needs cgo, the SQLite provider included. |
| The port stalls half finished. | Each phase ships a binary that does one job. A stall leaves working parts, not a broken tree. |

## 9. Open questions

- Does the Go module live in this repository, or in its own? One repository
  keeps the specs and the vectors next to both implementations.
- Which implementation owns the specs when they disagree?
- Does the Elixir code stay after the port, as a second implementation, or
  does it go?
- Does `cmd/arc` keep every command of today, or only the commands that a
  citizen and a provider need?
