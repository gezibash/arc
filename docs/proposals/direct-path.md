Status: proposed

# A direct path for bulk bytes

It answers [issue #167](https://github.com/gezibash/arc/issues/167). The
first three steps of section 6 are built: the pipe, http bodies and release
archives. [The direct spec](../direct/SPEC.md),
[the HTTP spec](../http/SPEC.md), section 3.7, and
[the updates spec](../updates/SPEC.md), section 3.7, define them. The other
steps are not built.

Each number has a tag:

- *measured*: a run on real machines or an emulated link. The source is given.
- *arithmetic*: a calculation from other numbers. The inputs are given.
- *estimated*: a judgement. No run supports it.

## 0. Bad news first

- A session over the relay cannot carry a large body in an acceptable
  time. A session allows one unacknowledged chunk of 16 KiB in each
  direction (`core/session/session.go:24`, `core/session/session.go:178`,
  [the session spec](../sessions/SPEC.md#32-frames), section 3.2). At a
  round trip of 44 ms, the ceiling is 2.98 Mbit/s (arithmetic: 16 KiB /
  44 ms).
- The relay limit is lower than that ceiling. The public relay runs with
  `--rate 300 --burst 1000`: 300 events for each IP address in a minute
  (`cmd/arc/main.go:473`). After the burst, the data frames go at
  5 events a second, or 0.66 Mbit/s (arithmetic: 5 × 16 KiB). Issue #167
  gives 0.17 to 0.9 Mbit/s through a public relay (measured).
- A 100 MB download on that path takes 889 to 4706 s (arithmetic: at 0.9
  and 0.17 Mbit/s). A session lives at most 30 minutes
  ([the session spec](../sessions/SPEC.md#35-limits), section 3.5), so the
  slow case fails.
- `arc update` waits at most 10 minutes (`cmd/arc/update.go:122`). An
  archive of 25.0 MB takes 1176 s at 0.17 Mbit/s (arithmetic), so a slow
  update fails.
- A wider session window does not fix this. The window is bound to the
  round trip, and the relay rate limit stays. Section 2 gives the details.
- A direct path shows the IP address of each side to the other side.
  Section 4.2 gives a relay-only setting. That setting uses the TURN quota
  of the citizen.

## 1. Which apps need a direct path

| App | Bulk bytes | Size | Need |
| --- | --- | --- | --- |
| `http` | A response body in a `server_stream` session ([HTTP spec](../http/SPEC.md#36-sessions), section 3.6). The session has no total limit. | 100 MB in the acceptance test of #167. Any size in real use. | Yes. First. |
| `releases` | One archive in a `server_stream` session (`apps/releases/server/session.go:12`). The consumer is `arc update` (`cmd/arc/update.go:341`). | 22,507,192 to 24,988,924 bytes for v0.23.1 (measured: GitHub release assets). | Yes. Second. |
| `transfer` | A file over a WebRTC data channel today (`apps/transfer/direct/part.go:120`). | 64 MiB and 256 MiB in the README runs (measured, `apps/transfer/README.md:237`). | Has its own pipe. It moves to the shared pipe. |
| storage citizen sync (#172) | Journal pages and files between two devices of one citizen, through a storage citizen. | Not known. Files (kind 1063) can be many MB (estimated). | Yes, after #173. Duplex: the bytes go in both directions. |
| `exec` | Command output. | 1 MiB by default, 4 MiB at most (`apps/exec/server/config.go:41`, `:47`). | No. 4 MiB takes 37 to 197 s at 0.9 to 0.17 Mbit/s (arithmetic). The limits keep the output small. |
| `sqlite` | Query rows. | 1 MiB at most (`apps/sqlite/server/config.go:28`, `:32`). | No. Same reason. |

Calls, mail, interactive `exec` sessions and SQL transactions stay on the
relay. They need a low wait time, not a high byte rate.

## 2. Where the shared pipe lives

### 2.1 The three options

**(a) Move the pipe from `apps/transfer/direct` to a new package `sdk/direct`.**

- Packages: a new `sdk/direct`. It takes `NewConnection`, `Describe`,
  `SelectedPath`, `HoldCandidates` and the STUN helpers from
  `apps/transfer/direct/direct.go:106` to `:257`. It also takes the attempt
  order (direct, other order, TURN) from
  `apps/transfer/client/client.go:236` to `:250`. `part.go` and `link.go`
  stay in `apps/transfer`, because they are about files and links.
  `apps/transfer` imports `sdk/direct`. Then `sdk/httpadapter`,
  `cmd/arc/session_http.go`, `cmd/arc/update.go` and
  `apps/releases/server/session.go` use it.
- Rules: `cmd/arc` can import `sdk/` but no app (`AGENTS.md:29`). An app
  can import `sdk/` (`AGENTS.md:27`). So both ends of http and releases can
  use the pipe. pion is in `go.mod` already (`go.mod:17`).
- Specs: a new spec `docs/direct/SPEC.md` owns `sdk/direct`
  (`AGENTS.md:62`). [The HTTP spec](../http/SPEC.md#36-sessions), section
  3.6, gets a `direct` request field and two records. That section is
  `built`, so the change is a protocol change (`AGENTS.md:64`). No new event
  kind: the setup rides on existing session frames.
- An older client: an older consumer never asks for a pipe. It gets the
  relay path as today. An older provider decodes the request strictly
  (`sdk/httpadapter/http.go:90`), so it refuses the `direct` field with
  `invalid_request`. It refuses before it runs the HTTP request. The
  consumer then tells the user, and opens a new session on the relay
  (section 5).

**(b) A delivery carrier under `core/session`: a `transport.Live` over a data channel.**

- Packages: a new `adapters/transport/webrtc` that implements
  `transport.Live` (`core/transport/transport.go:55`). `core/call` opens a
  session on any `transport.Live` (`core/call/session.go:59`), so the plug
  exists. Then `runtime/citizen.Open` selects the carrier, and
  `docs/delivery/TRANSPORTS.md` applies in full.
- The window problem: the session frames keep one chunk of 16 KiB per round
  trip. On a direct path of 44 ms, the ceiling is still 2.98 Mbit/s
  (arithmetic). The measured direct pipe gives a median of 97.0 Mbit/s
  (`apps/transfer/README.md:248`). The carrier wastes 97 % of that
  (arithmetic: 2.98 / 97.0).
- A wider window changes `built` sections 3.2 and 3.5 of
  [the session spec](../sessions/SPEC.md). That is a breaking change. An
  older peer ends the session on excess credit
  (`core/session/session.go:178`).
- Each 16 KiB chunk is still a gift wrap. A wrap of 32 KiB content is 2.35
  times its size (measured, #165 note), on top of base64 in the frame.
  DTLS encrypts the bytes a second time.
- The runtime picks the carrier, not the app. Zim decided on 2026-10-11 that
  the app picks the path when the session starts.

**(c) Keep the pipe in `apps/transfer`. Other apps call `transfer`.**

- `cmd/arc` imports no app (`AGENTS.md:29`). An app imports no other app
  (`AGENTS.md:27`). So `arc update`, `arc session --http` and `arc-http`
  must each run the program `arc-transfer`, and each citizen must install
  it.
- The transfer protocol moves a file that has a known SHA-256 and size
  (`apps/transfer/direct/direct.go:44`). An HTTP body is a stream with no
  known hash. Transfer then needs a new stream operation, and a local
  hand-off between `arc-http` and `arc-transfer` on the provider machine.
- Packages: `apps/transfer` (new operation), `apps/http`, `apps/releases`,
  `cmd/arc`, and the transfer README protocol. An older `arc-transfer`
  refuses the new operation.

### 2.2 Cost table

| | (a) `sdk/direct` | (b) carrier | (c) via transfer |
| --- | --- | --- | --- |
| Speed on a direct path | Pipe speed: median 97.0 Mbit/s (measured) | 2.98 Mbit/s at 44 ms (arithmetic) unless the window changes | Pipe speed, plus one more process hop |
| New packages | 1 (`sdk/direct`) | 1 adapter, plus `core/session`, `core/call`, `runtime/citizen` | 0, but 4 packages change |
| Spec sections | New `docs/direct/SPEC.md`; HTTP 3.6 | Session 3.2, 3.3, 3.5; delivery spec | Transfer README only, but a private protocol for the whole system |
| Older clients | Explicit refusal, then the relay | Breaking: excess credit ends the session | Older `arc-transfer` refuses |
| Who picks the path | The app | The runtime | The app, through a second program |

### 2.3 Recommendation

Use **(a)**. It is the only option that gives pipe speed and keeps the
session spec as it is. It follows the `sdk/` rule for shared helpers
(`AGENTS.md:27`). It reverses the 2026-10-05 answer "the direct connection
is an app" only for the pipe. `transfer` stays an app for file offers,
links and part files.

The pipe is scoped to one session, and the session stays on the relay as
the control path. The relay carries the open, the setup, the status and the
final outcome. The pipe carries only the bulk bytes of that session. The
pipe opens with the session and closes with it. It never carries a second
session. So the session state machine stays in `core/session`, and no
adapter makes a second one (`AGENTS.md:43`).

## 3. The API

### 3.1 What an app sees

The API names no pion type, so a later QUIC pipe can take its place.

```go
package direct // sdk/direct

// Policy is the choice of a citizen for one session.
type Policy string

const (
	Auto      Policy = "auto"    // direct, then the other order, then TURN
	RelayOnly Policy = "turn"    // only an address on the TURN server of the relay
	Off       Policy = "session" // no pipe; session frames over the relay
)

// Offer goes in the open request of a session. Answer comes back in the
// first record of the provider. Both are JSON.
type Offer struct {
	Version int    `json:"v"`
	SDP     string `json:"sdp"`
	HoldMS  int    `json:"hold_ms,omitempty"`
	STUN    string `json:"stun,omitempty"` // the TURN server, for the relay attempt
}
type Answer struct {
	Version int    `json:"v"`
	SDP     string `json:"sdp"`
}

// Config holds the settings of one end.
type Config struct {
	Relays []string // the STUN server is the host of the first relay
	Policy Policy
	// TURN gives a credential. Only the consumer needs one.
	TURN func(ctx context.Context) (Credential, error)
}

// Attempts runs the order of attempts. For each attempt, it calls open
// with an offer. open starts a new session and returns the answer.
// Attempts returns the first pipe that opens.
func Attempts(ctx context.Context, c Config, need int64, notes io.Writer,
	open func(context.Context, Offer) (Answer, error)) (*Pipe, error)

// Accept is the provider side: it answers an offer, and returns the pipe
// that opens when the consumer connects.
func Accept(ctx context.Context, c Config, o Offer) (*Pipe, Answer, error)

// Pipe is one reliable, ordered byte stream for one session.
type Pipe struct{ /* ... */ }

func (p *Pipe) Wait(ctx context.Context) error // the data channel is open
func (p *Pipe) Read(b []byte) (int, error)
func (p *Pipe) Write(b []byte) (int, error)
func (p *Pipe) CloseWrite() error
func (p *Pipe) Close() error
func (p *Pipe) Path() Path // for example {Kind: "direct", Local: "srflx", Remote: "srflx"}
```

### 3.2 How the setup rides on one ARC call

The consumer makes the offer, and the provider answers, as `transfer` does
today (`apps/transfer/client/client.go:357`). Each attempt is one session:

1. The consumer gathers its addresses and puts the `Offer` in the open
   request of the session: `{"direct": {...}, ...}`.
2. The provider checks its access rules, and calls `direct.Accept`. Its
   first output record is `{"type":"direct","sdp":"..."}`.
3. The provider waits for the pipe (at most 12 s, `OpenWait`,
   `apps/transfer/direct/direct.go:33`). It does no work for the request
   before the pipe opens. So an attempt that fails leaves no side effect.
4. If the pipe opens, the session continues (section 3.4). If not, the
   provider closes the session with the error `no_path`. The consumer
   starts the next attempt as a new session.

The session gives the setup what it needs: the wrap authenticates the
author (`core/private/private.go:136`), and a request older than 5 minutes
is refused (`core/call/call.go:37`).

### 3.3 Credentials for TURN

Only the consumer needs a TURN allocation. One relay address on one end is
enough for two symmetric NATs (#170 phase-1 note).

- `cmd/arc` gets the credential in its own process. It uses the code of
  `arc relay turn` (`cmd/arc/main.go:272`).
- An app program runs `arc relay turn`, as `transfer` does
  (`apps/transfer/client/client.go:95`). `sdk/direct` gives a helper for
  this, so no app copies it.
- If the quota is lower than the size of the body, the consumer stops
  before the attempt. It names both numbers, as `transfer` does
  (`apps/transfer/client/client.go:121`). The size is known for releases
  and for transfer. For http, the size is not known before the response,
  so the check uses the quota only.

### 3.4 How http maps a response body onto the pipe

The session stays a `server_stream` session on the relay.

| Order | On the relay (session records) | On the pipe |
| --- | --- | --- |
| 1 | Open: the HTTP request with `"direct": <Offer>` | |
| 2 | `{"type":"direct","sdp":...}` | |
| 3 | | DTLS handshake, data channel opens |
| 4 | `{"type":"response","status":200,"headers":{...}}` | |
| 5 | | The body bytes, then the write side closes |
| 6 | `{"type":"end","body_bytes":104857600}` | |
| 7 | Close: the final outcome of the provider | |

- The consumer reads the body from the pipe until the end of the stream.
  Then it compares the count with `body_bytes`. A different count is an
  error, not a short success.
- The `end` record can arrive before the last bytes on the pipe. The
  consumer waits for both.
- Trailers and SSE stay on the relay records. An SSE stream is not bulk.
- `arc session --http` writes the body to standard output, as today
  (`cmd/arc/session_http.go:27`). It gets a flag `--path auto|turn|session`.

### 3.5 How releases maps an archive onto the pipe

- `arc update` opens the `archive` session with `"direct": <Offer>`
  (`cmd/arc/update.go:341`).
- The provider sends the `direct` record, waits for the pipe, and copies the
  archive to the pipe. The final close stays on the relay.
- The channel document names the size and the SHA-256 of each archive.
  `arc update` checks both already (`cmd/arc/update.go:190`). So releases
  needs no `body_bytes` record.

### 3.6 How the user sees the path

Each command that uses a pipe writes one line on standard error at the end:

```text
path direct (srflx to srflx): 104857600 bytes in 9.0 s, 93.2 Mbit/s
path turn (relay): 104857600 bytes in 92.0 s, 9.1 Mbit/s
path session (relay frames): 1048576 bytes in 12.4 s, 0.68 Mbit/s
```

Before each new attempt, it writes the reason, as `transfer` does
(`apps/transfer/client/client.go:242`, `:246`). The numbers above are
examples, not runs.

## 4. Security

### 4.1 Identity binding of the DTLS link

- pion makes a new ECDSA P-256 certificate for each connection
  (`pion/webrtc/v4@v4.2.22/peerconnection.go:235`). Its fingerprint is in
  the SDP of the offer and of the answer.
- The SDP travels inside the session. The session is a gift wrap that the
  author signs (`core/private/private.go:136`), and `core/call` verifies the
  author of each frame ([the session spec](../sessions/SPEC.md#33-events),
  section 3.3). The relay cannot read or change the fingerprint.
- pion checks that the certificate of the other end has the fingerprint of
  the remote SDP (`pion/webrtc/v4@v4.2.22/dtlstransport.go:670`). So the
  DTLS link ends at the holder of the ARC key that signed the SDP.
- `sdk/direct` must never call `DisableCertificateFingerprintVerification`.
  A test must prove that a wrong fingerprint fails.
- Prior art: libp2p needs no extra handshake for WebRTC between two browsers,
  because the offer and the answer cross an authenticated channel
  ([libp2p webrtc.md](https://github.com/libp2p/specs/blob/master/webrtc/webrtc.md)).
  It adds a Noise handshake only when the fingerprint comes from an
  unauthenticated address
  ([webrtc-direct.md](https://github.com/libp2p/specs/blob/master/webrtc/webrtc-direct.md)).
  ARC is in the first case. iroh binds the link to a public key in the same
  way, with the key as the TLS identity
  ([iroh](https://docs.rs/crate/iroh/latest)).
- A replayed offer cannot open a pipe. The attacker does not have the DTLS
  private key of the consumer, which never leaves the process.

### 4.2 IP address exposure and a relay-only setting

- A direct path shows the public address and the LAN addresses of each side
  to the other side. The addresses are in the SDP, so the other side learns
  them also when no path opens.
- A provider shows its address to each caller that its access rules let in.
  A consumer shows its address to the provider that it calls.
- Relay-only (`Policy` `turn`): that end gathers only an address on the
  TURN server (`ICETransportPolicyRelay`,
  `apps/transfer/direct/direct.go:203`). The other side then sees only the
  address of the relay. The cost is the TURN quota of the citizen (1024 MiB
  in 24 hours on the public relay, #186) and the TURN speed: about 89 Mbit/s
  between two virtual machines, about 10.5 Mbit/s from a Mac at home
  (measured, `apps/transfer/README.md:269`, `:271`).
- A provider that is relay-only needs its own TURN credential. A provider
  program can run `arc relay turn`. No run has proved this under
  `arc serve` (unverified).
- `session` turns the pipe off. The bytes go in session frames over the
  relay, as today.

### 4.3 Forward secrecy

- The pipe has forward secrecy. Each connection has a new certificate, and
  pion DTLS uses ECDHE cipher suites
  (`pion/dtls/v3@v3.1.9/cipher_suite.go:28`). A later theft of a citizen
  key does not open recorded pipe traffic.
- The setup and the control records on the relay have no forward secrecy,
  as today ([the session spec](../sessions/SPEC.md#6-security), section 6).
  They hold the SDP, the status and the headers. HTTP headers can hold
  secrets, so a user who needs forward secrecy for headers does not get it.

## 5. Failure behavior

| Failure | What happens | What the user sees |
| --- | --- | --- |
| No direct path in the first order | A new session with the other order of the first packets | `no direct path in the first order of the packets; trying the other order` |
| No direct path in either order | A new session with a TURN address | `no direct path; trying the TURN server of the relay, so the bytes go through the relay` |
| TURN quota too low | Stop before the attempt | The quota and the size, in MiB |
| No path at all | Error. No session path in `auto`, because the relay frames are slower than TURN | `no direct path, and no path through the relay: ...; run again with --path session` |
| The provider is older and refuses `direct` | A new session with no pipe | `the provider has no pipe; using session frames over the relay` |
| The pipe breaks during the body | The session ends with an explicit error. No resumption, no replay (`AGENTS.md:50`) | `the pipe closed after N of M bytes; the outcome is not known` |
| `body_bytes` and the count differ | Error | `the body has N bytes, the provider sent M` |
| The provider has too many pipes | `busy`, before the pipe | `the app runs too many transfers now; try again later` |

Each attempt that fails is a session that did no work, so a new attempt
is not a replay. A session never moves to another path after the first
body byte.

## 6. Recommendation and the first step

1. Put the pipe in `sdk/direct`, option (a).
2. Keep the session on the relay as the control path. The pipe carries only
   the bulk bytes of that one session.
3. The consumer offers, gets the TURN credential, and picks the policy. The
   provider answers.
4. First, move the pipe to `sdk/direct` with no change in behavior.
   `transfer` uses it, and its tests pass. Add `docs/direct/SPEC.md` with
   `Status: proposed`.
5. Then, http downloads (`server_stream`) over the pipe, in
   `sdk/httpadapter` and `cmd/arc/session_http.go`. Then releases. Then the
   storage citizen, after #173. Uploads (`stream_body`) come last.

The proof of step 5 is the acceptance of #167. Run it on two machines
behind NAT. The provider serves a file of 100 MB through `arc serve` with an
HTTP app. On the consumer:

```bash
arc session --http --path auto 'http+arc://<provider>/big.bin' > big.bin
```

```bash
arc session --http --path turn 'http+arc://<provider>/big.bin' > big-turn.bin
```

```bash
shasum -a 256 big.bin big-turn.bin
```

Each run must print its path line (section 3.6). Both hashes must match
the file on the provider. Record the median of six runs of each, as the
transfer README does (`apps/transfer/README.md:237`). An automated test
runs the same download on the emulated link of
`apps/transfer/direct/link_speed_test.go`. It does not replace the run on
real machines.

## 7. Decisions

Zim accepted each recommendation of this proposal on 2026-10-11.

1. The pipe is in `sdk/direct`. `transfer` stays an app for file offers,
   links and part files. This replaces the 2026-10-05 answer that the direct
   connection is an app. (Zim, 2026-10-11.) Reason: `cmd/arc` cannot use a
   pipe that lives in an app.
2. In `auto`, a command does not fall back to session frames after a pipe
   fails. It falls back to frames only when the provider is older and has
   no pipe. (Zim, 2026-10-11.) Reason: TURN is faster than frames in each
   run that we have.
3. The relay-only setting is a flag `--path` on each command, and one
   citizen setting that sets the default. A provider program reads
   `ARC_PATH`. The default is `auto`. (Zim, 2026-10-11.) Reason: the app
   picks the path when the session starts, and the user can see and set it.
4. A provider opens a pipe for each caller that its access rules let in, so
   it shows its IP address to each of them. An operator who does not want
   this sets `ARC_PATH=turn` or `session`. (Zim, 2026-10-11.) Reason: the
   access rules already decide who can reach the provider.
5. One provider process holds at most 4 pipes at a time (estimated), the
   same kind of slot as `apps/transfer/server/server.go:234`. More pipes
   return `busy`. (Zim, 2026-10-11.) Reason: a pipe holds sockets and
   buffers, so the count must be bounded.
6. The `end` record carries only the byte count, not a SHA-256 of the body.
   (Zim, 2026-10-11.) Reason: DTLS gives integrity; a SHA-256 is useful only
   for a later resume with a `Range` request.
7. `sdk/direct` gets its own spec, `docs/direct/SPEC.md`. The session spec
   stays unchanged. (Zim, 2026-10-11.) Reason: the pipe is a separate
   contract, and the session spec does not change.
8. The storage citizen (#172) uses one pipe for one sync session, in both
   directions. (Zim, 2026-10-11.) Reason: the pipe is a byte stream in both
   directions already, so sync gets no new path type.
