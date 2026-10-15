# Direct pipe

- Status: partial
- Layers: sdk
- Owns: sdk/direct
- Proof: go test -count=1 -run '^(TestRelayPath|TestRelayPathWithoutSTUN|TestRelayQuota|TestSTUNURL|TestPipe|TestPipeClosedEarly|TestPipeOpenedThenDead|TestPipeAnswererRelayOnly|TestPipeFingerprint|TestParsePolicy|TestOffererSendsFirst|TestAnswererFirst)$' ./sdk/direct/
- Remaining: the storage citizen step of [the proposal](../proposals/direct-path.md), section 6.
- Unverified: No automated test runs two machines behind real NATs. The direct path and the relay attempt between two machines behind NAT were tested by hand only, with the transfer app and with `arc session --http` (section 8.2). A provider with the policy `turn` under `arc serve` and a consumer with `--path turn` gave a path between two machines behind NAT through the production relay in 6 of 6 runs, with the relay of v0.27.0 (section 8.2).

The package `sdk/direct` opens a pipe between two citizens. The pipe is a
WebRTC data channel. It carries bulk bytes outside the relay. One ARC call
carries the offer and the answer that set it up.

## 1. Purpose

A session over a relay carries one chunk of 16 KiB for each round trip, and
the relay limits the events of each IP address. A large body over a relay is
slow. [The proposal](../proposals/direct-path.md), section 0, gives the
numbers.

This spec defines the pipe that an app opens for bulk bytes:

- the settings of one end of the connection,
- the order of the attempts: two direct attempts, then the TURN server of
  the relay as the last resort,
- the TURN quota check before the relay attempt,
- the flow control of the sender,
- the names of the path that a command shows.

The app defines the ARC call that carries the offer and the answer, and the
bytes that go on the pipe. The [transfer app](../../apps/transfer/README.md)
is the first user. Its request protocol is in its README.

This spec does not define a session carrier. The session state machine stays
in `core/session`. The pipe does not carry session frames.

The pipe was inside the transfer app. `cmd/arc` imports no app, so a command
of `cmd/arc` could not use it.

## 2. Terms

- **End**: one side of a connection. The offerer makes the WebRTC offer. The
  answerer makes the answer.
- **Attempt**: one connection setup, with one offer and one answer. An
  attempt fails if the data channel does not open in time.
- **Direct attempt**: an attempt in which each end uses the addresses of its
  machine and the addresses that its STUN servers give.
- **Relay attempt**: an attempt in which the offerer uses only an address on
  the TURN server of its first relay.
- **Hold**: the time that the answerer waits before it gives the addresses
  of the offerer to ICE. While it holds them, it sends no packet, so the
  offerer sends first.
- **Path**: the kinds of the two addresses that ICE selected. `host` is an
  address of the machine. `srflx` is an address behind a NAT that a STUN
  server gave. `relay` is an address on a TURN server.
- **Quota**: the number of bytes that the relay still relays for a citizen
  in the current day. [The delivery spec](../delivery/SPEC.md), section
  4.12, defines it.

## 3. Rules

### 3.1 The STUN server

- The default STUN server of an end MUST be the host of its first relay, on
  UDP port 3478: `stun:<host>:3478`. A relay URL that is not `ws://` or
  `wss://` gives no server.
- The value `none` MUST give no STUN server. Another value is the STUN URL.
- With no STUN server and no relay, an end uses the addresses of its machine
  only.

### 3.2 The settings of an end

Each end MUST use these settings:

| Setting | Value | Reason |
| --- | --- | --- |
| Network | UDP on IPv4 only | STUN on IPv6 with no route added 5 s to each setup. |
| Interfaces | No interface whose name starts with `bridge`, `vmenet`, `utun`, `awdl`, `llw`, `docker`, `veth`, `br-` or `wg` | A UDP write on a bridge of a virtual machine blocked with no end, and pion then stopped all ICE work. |
| STUN gather timeout | 2 s | |
| ICE binding requests | 30 for each pair | pion stops after 8 by default, about 1.6 s. The other end can start 1 s later. |
| ICE timeouts | disconnected 4 s, failed 4 s, keepalive 1 s | An attempt with no path fails after about 8 s, not after 30 s. |
| Loopback | Only if the end asks for it | Two ends on one machine need it. |

In a relay attempt, the offerer MUST use only an address on the TURN server
(the ICE transport policy `relay`). In a direct attempt, an end MUST list its
STUN server and the STUN server that the other end named, if any.

### 3.3 Congestion control

- The SCTP send window MUST grow by 8 KiB for each round trip after a loss.
  The default of pion is one packet, about 1.2 KiB. On a link of
  92.5 Mbit/s with a round trip of 44 ms, the window is about 500 KiB, and
  it then needs about 9 s to grow back.
- A loss still halves the window. The congestion control stays on.
- An end MAY set a smallest window after a loss. This turns off the backoff.
  It is not fair to other traffic.

Section 8.2 gives the measurements behind the step.

### 3.4 Flow control of the sender

- The sender MUST write binary messages of at most 16 KiB.
- When the buffer of the data channel holds more than 8 MiB, the sender MUST
  wait until it holds 2 MiB or less.
- If the data channel closes during the wait, the sender MUST stop with an
  error.

The app defines what marks the end of the bytes. The transfer app sends the
text `end`.

### 3.5 The order of the attempts

On some routers, a direct path opens only if the machine behind that router
sends the first packet. So the offerer MUST make the attempts in this order:

1. A direct attempt with a hold of 1 s.
2. A direct attempt with no hold.
3. A relay attempt with no hold, unless the user turned it off.

In an attempt with no hold, the answerer always sends first. It starts ICE
when it sets the offer, before its reply leaves. The offerer starts ICE
only when the reply arrives. The offerer is usually the device of a person
behind the router of a home. With the hold first, that device sends first.
The hold works only if it is longer than the time that the reply needs to
reach the offerer.

An offerer MAY swap the first two attempts. An attempt fails if ICE fails
(about 8 s with no path, section 3.2), or if the data channel does not open
in 12 s plus the hold. The hold that an answerer
takes MUST be at most 3 s. Before the second attempt and before the relay
attempt, the offerer MUST write the reason on its notes:

```text
no direct path in the first order of the packets; trying the other order
no direct path; trying the TURN server of the relay, so the bytes go through the relay
```

Each attempt is a new ARC call and a new connection. An attempt that fails
transferred no bytes, so the next attempt is not a replay. An attempt whose
SCTP association connected found a path, also if the data channel closes
before this end sees it open. The other end can have started, so the
offerer MUST NOT make the next attempt. The read or the write of the pipe
then fails.

### 3.6 The relay attempt

1. The offerer gets a TURN credential and its quota from its first relay.
   An app program runs `arc relay turn`. arc signs a NIP-98 request with the
   key of the citizen.
2. If the quota is smaller than the bytes that remain plus a tenth, the
   offerer MUST stop before the attempt, with an error that names the two
   numbers in MiB. The tenth is for SCTP, DTLS and acknowledgements.
3. The offerer MUST check that its description holds an address of type
   `relay`. If not, it stops: the TURN server gave no address.
4. The offerer MUST name the TURN server to the other end as a STUN server,
   with its IPv4 address and port, such as `stun:192.0.2.1:3478`. The other
   end asks that server for its public address too. The TURN server lets in
   only packets from the addresses in the answer. An end whose own STUN
   server did not answer otherwise gives only addresses behind its NAT, and
   the attempt finds no path.
5. An answerer MUST refuse a named STUN server that is not a `stun:` URL
   with an IPv4 address and a port.

If the relay attempt fails, the error MUST start with
`no direct path, and no path through the relay`.

### 3.7 The policy, the offer and the pipe

An app that carries the offer in the open request of a session uses these
rules. [The HTTP spec](../http/SPEC.md), section 3.7, is the first user.

The policy is the choice of an end for one session:

| Policy | Offerer | Answerer |
| --- | --- | --- |
| `auto` | The order of section 3.5 | Answers with all its addresses |
| `direct` | The two direct attempts only | Answers with all its addresses |
| `turn` | The relay attempt only | Answers with only an address on the TURN server of its first relay |
| `session` | Makes no offer | Refuses each offer |

- The default policy MUST be `auto`.
- A command of `cmd/arc` takes the flag `--path`. If the flag is not set,
  it reads the file `path` in the home of the citizen. `arc path <policy>`
  writes that file.
- The sync of self data of `arc sync` and `arc serve` reads only the file
  `path`. It opens each pipe to a storage citizen through TURN only with
  `turn`, and with no TURN with `direct`. `session` acts as `auto` there,
  because a storage app needs a pipe ([the storage spec](../storage/SPEC.md),
  section 4.6).
- A provider program reads `ARC_PATH`. `arc-http` and `arc-releases` stop
  at their start if the value is not a policy. `httpadapter.New` opens no
  pipe for such a value.
- `ARC_STUN` sets the STUN server of an end, as section 3.1 defines.
  `ARC_LOOPBACK`, if set, adds the loopback address.
- An answerer with the policy `turn` runs `arc relay turn` for a credential.
  It uses the program `ARC_PROGRAM`, the home `ARC_HOME` and the key
  `ARC_IDENTITY`, which `arc serve` gives.

The offer is one JSON object:

```json
{"v": 1, "sdp": "<offer>", "hold_ms": 1000, "stun": "stun:192.0.2.1:3478"}
```

- `v` MUST be 1. `sdp` holds all the addresses of the offerer.
- `hold_ms` is the hold of section 3.5, at most 3000.
- `stun` is the STUN server of section 3.6, step 4, in a relay attempt.
- An answerer MUST refuse an offer that breaks one of these rules.

The pipe is one data channel with the label `pipe`, reliable and ordered.

- A binary message carries bytes. The writer follows section 3.4.
- The text `end` says that the writer sent its last byte. The reader then
  MUST send the text `ok`.
- The writer MUST wait for `ok` before it closes the connection. If it
  closes first, the last bytes can stay in its buffer.
- Another text stops the reader with an error that holds the text.
- If the connection closes before `end`, the reader MUST stop with an error.
  It MUST NOT take the bytes so far as all of them.
- The pipe carries bytes in each direction. Each direction has its own
  `end` and `ok`. The HTTP upload of [the HTTP spec](../http/SPEC.md),
  section 3.8, uses both directions at the same time.
- A reader that stops before `end` MUST still take the bytes off the
  channel and drop them. The data channel delivers the messages in order,
  so bytes that nobody reads also stop the `end` and the `ok` of the other
  direction.

An answerer holds at most 4 pipes at a time. It MUST refuse more before it
answers. Each pipe holds sockets and buffers.

A command writes one line on standard error after the bytes:

```text
path direct (host to host): 104857600 bytes in 2.4 s, 348.9 Mbit/s
path turn (relay): 104857600 bytes in 2.5 s, 329.6 Mbit/s
path session (relay frames): 104857600 bytes in 54.3 s, 15.5 Mbit/s
```

An upload writes the same line for the request body, after the word
`upload`:

```text
upload path direct (host to host): 104857600 bytes in 1.4 s, 587.0 Mbit/s
```

These lines are runs on the loopback address of one Mac. Section 8.2 gives
the commands.

## 4. Behavior

The offerer:

1. Makes one end with the settings of section 3.2, and a data channel.
2. Makes the offer, and waits until its addresses are gathered. The offer
   holds all of them. No end sends addresses later.
3. In a relay attempt, finds the STUN server of the TURN server
   (section 3.6).
4. Sends the offer, the hold and the STUN server in one ARC call to the app
   of the other citizen.
5. Sets the answer of the reply, and waits for the data channel.
6. Reads the path when the data channel opens.

The answerer:

1. Checks the request. The app checks its own access rules.
2. Makes one end, and adds the named STUN server.
3. With a hold, takes the addresses out of the offer, and gives them to ICE
   after the hold.
4. Makes the answer, waits until its addresses are gathered, and returns it
   in the reply.
5. Closes the end if the data channel does not open in 12 s plus the hold.

The two ends then use the data channel. The sender follows section 3.4.

## 5. Failures

| Failure | What the offerer does | What the user sees |
| --- | --- | --- |
| No path in the first direct attempt | The second direct attempt | `no direct path in the first order of the packets; trying the other order` |
| No path in either direct attempt | The relay attempt | `no direct path; trying the TURN server of the relay, so the bytes go through the relay` |
| The user turned the relay attempt off | Stops | `no direct path` |
| `arc relay turn` fails | Stops | `no direct path, and no path through the relay: arc relay turn: <reason>` |
| The quota is too small | Stops before the attempt | The quota and the need, in MiB |
| The TURN server gave no address | Stops | `no direct path, and no path through the relay: the TURN server of the relay gave no address` |
| No path in the relay attempt | Stops | `no direct path, and no path through the relay: no direct path` |
| The receiver closes the channel while the sender waits | The sender stops | `the receiver closed the channel` |

An answerer that does not see the data channel open closes its end after
12 s plus the hold, and frees its slot.

## 6. Security

- Each connection has a new certificate. Its fingerprint is in the offer and
  in the answer. The ARC call carries both, sealed and signed with the keys
  of the two citizens. A relay cannot read or change the fingerprints.
- pion checks that the certificate of the other end has the fingerprint of
  the remote description. So the DTLS link ends at the citizen who signed
  the call. `sdk/direct` MUST NOT turn off this check. `TestPipeFingerprint`
  proves that a pipe with a wrong fingerprint does not open.
- DTLS uses ECDHE, so the pipe has forward secrecy. The ARC call that
  carries the setup has the forward secrecy of the call, as today.
- A direct attempt shows the public address and the LAN addresses of each
  end to the other end, also if no path opens. An answerer shows its
  addresses to each caller that its access rules let in.
- In a relay attempt, the offerer shows only the address of the relay. The
  answerer still shows its own addresses.
- The STUN server sees the public address of each end.
- The relay operator pays for the bandwidth of a relay attempt, and sets
  the quota. The quota check of section 3.6 stops an attempt that cannot
  finish inside the quota. If the quota runs out during a transfer, the
  relay stops relaying.
- An answerer bounds the number of connections that it holds. The transfer
  app and the HTTP adapter hold at most 4.
- An answerer with the policy `turn` shows only the address of its relay.
  The relay operator pays for those bytes too.

## 7. Compatibility

- The pipe moved from `apps/transfer/direct` to `sdk/direct` with no change
  on the wire. A transfer client of an older version and one of this
  version interoperate in each direction.
- An answerer of a version before the relay attempt ignores the named STUN
  server. The relay attempt can then fail if the STUN server of that end
  does not answer.
- An app that opens a pipe defines its own request. A change to the request
  is a change of that app.
- The policy, the offer and the pipe of section 3.7 are new. The transfer
  app keeps its own request, and does not use them.
- Section 3.5 puts the attempt with the hold first. The offer carries
  `hold_ms` as before, so an older answerer takes the new order with no
  change. An older offerer makes the attempt with no hold first, and keeps
  the wait of section 5 behind a router that needs the offerer to send
  first.
- The rule of section 3.7 that a reader takes the bytes off the channel
  after it stops changes no message. An older end that has one direction
  only never meets the case.

## 8. Proof

### 8.1 Tests

- `TestRelayPath` puts two ends behind symmetric NATs on an emulated
  network, with the TURN server of `adapters/relay/turn`. The direct attempt
  finds no path. The relay attempt sends 1 MiB, the path is `relay`, and the
  relay counts at least 1 MiB against the quota.
- `TestRelayPathWithoutSTUN` gives the answerer no STUN server. The relay
  attempt finds no path until the answerer uses the STUN server of the TURN
  server.
- `TestRelayQuota` checks the refusal before a relay attempt when the quota
  is too small, and the TURN server of the attempt when it is large enough.
- `TestSTUNURL` checks the default STUN server of section 3.1.
- `TestPipe` sends 8 MiB on a pipe on the loopback address. The reader gets
  each byte and then the end, and the writer gets `ok`.
- `TestPipeClosedEarly` closes the writer during the bytes. The reader gets
  an error, not the end.
- `TestPipeOpenedThenDead` closes a channel on a connected SCTP association
  before its open handler runs. Wait returns nil, so Dial makes no next
  attempt. A channel that closes before the association connects has no
  path.
- `TestPipeAnswererRelayOnly` gives the answerer the policy `turn`, behind a
  symmetric NAT. The path is `turn (relay)`.
- `TestParsePolicy` checks the names of section 3.7.
- `TestOffererSendsFirst` models an offerer behind a router that blocks a
  pair when the first packet comes from outside. One attempt opens the
  pipe, with a hold of 1 s, and the notes stay empty.
- `TestAnswererFirst` checks the swapped order of section 3.5 and the note
  before the second attempt.
- The tests of `apps/transfer` run the order of the attempts with the built
  `arc`, a local relay and three citizens.

### 8.2 Measurements

`TestEmulatedLinkSpeed` sends bytes between two ends on a link that pion
emulates. The link has a rate limit and a drop-tail queue in the direction
of the data, and a delay in each direction. The test prints the speed and
does not check it. It runs only if `DIRECT_LINK_MIB` is set:

```sh
DIRECT_LINK_MIB=64 DIRECT_LINK_QUEUE_KIB=128 go test ./sdk/direct -run TestEmulatedLinkSpeed -count=1 -v
```

`DIRECT_LINK_MBIT`, `DIRECT_LINK_RTT_MS` and `DIRECT_LINK_CA_STEP_KIB` set
the rate, the round trip and the step. `DIRECT_LINK_CA_STEP_KIB=0` gives the
step of pion. Results on an Apple Silicon Mac, 2026-10-11, emulated link of
92.5 Mbit/s and 44 ms, 64 MiB, three runs for each cell:

| Queue | Step of pion (1.2 KiB) | Step of 8 KiB |
| --- | --- | --- |
| 64 KiB | 29.5 to 35.3 Mbit/s | 52.1 to 56.0 Mbit/s |
| 128 KiB | 31.1 to 42.2 Mbit/s | 52.2 to 63.1 Mbit/s |
| 512 KiB | 60.8 to 61.2 Mbit/s | 71.4 to 75.3 Mbit/s |

Larger steps were not safe. With a 64 KiB queue, a 16 KiB step gave 25.7 to
38.1 Mbit/s, and a 32 KiB step gave 10.6 to 12.7 Mbit/s. A larger fast
retransmit window or receive buffer did not make a transfer faster. A
smallest window of 512 KiB gave 1.1 Mbit/s with a 64 KiB queue, and
84 Mbit/s with a 512 KiB queue.

`arc session --http` measured a body of 104,857,600 bytes from `arc-http`
on one Apple Silicon Mac, 2026-10-11. Two new citizens and a local relay
ran on the machine. The pipe used the loopback address
(`ARC_LOOPBACK=1 ARC_STUN=none`), because UDP from this Mac to its own LAN
address got no answer, also from python3. The relay had no rate limit.
Three runs of each, with the SHA-256 of each body checked (measured):

| Path | Time of the body | Speed |
| --- | --- | --- |
| `--path direct` | 2.4 s in each run | 345.1 to 349.4 Mbit/s |
| `--path auto` | 2.4 s in each run, on `direct (host to host)` | 345.7 to 350.4 Mbit/s |
| `--path session` | 54.2 to 54.3 s | 15.4 to 15.5 Mbit/s |
| `--path turn`, one run, relay with TURN on `127.0.0.1` | 2.5 s | 329.6 Mbit/s |

The commands, with `arc` built from this tree:

```sh
arc --home "$W/relay" relay serve --listen 127.0.0.1:7499 --stun none
arc --home "$W/provider" keys gen
arc --home "$W/provider" relay add ws://127.0.0.1:7499
ARC_LOOPBACK=1 ARC_STUN=none arc --home "$W/provider" serve "exec://$PWD/arc-http?manifest=$PWD/cmd/arc/testdata/streaming-http/interface.json&args=$PWD/origin"
arc --home "$W/consumer" keys gen
arc --home "$W/consumer" relay add ws://127.0.0.1:7499
arc --home "$W/consumer" install <provider key> --yes
ARC_LOOPBACK=1 ARC_STUN=none arc --home "$W/consumer" session --http --mode server_stream --path direct 'http+arc://<provider key>/big' '{"query":"bytes=104857600"}' > big.bin
```

`origin` is `cmd/arc/testdata/streaming-http`. The loopback and a relay with
no rate limit flatter both paths.

An upload of the same 104,857,600 bytes went to `/upload` of `origin` on
2026-10-12, on the same Mac, in the same setup. The handler answered with
the count and the SHA-256 of the bytes that it got. Each of the nine runs
gave the SHA-256 of the file (`85a38859…ebed76`). The time runs from the
start of the send, after the pipe opened, to the `end` record (measured):

| Path | Time of the request body | Speed |
| --- | --- | --- |
| `--path direct` | 1.4 s in each run | 583.9 to 587.0 Mbit/s |
| `--path auto` | 1.4 to 1.5 s, on `direct (host to host)` | 571.7 to 592.7 Mbit/s |
| `--path session` | 31.7 to 32.6 s | 25.8 to 26.5 Mbit/s |

```sh
ARC_LOOPBACK=1 ARC_STUN=none arc --home "$W/consumer" session --http --mode duplex --path direct 'http+arc://<provider key>/upload' '{"stream_body":true}' < big.bin
```

`big.bin` holds 104,857,600 bytes: byte i is i mod 251. Through a public relay, the session
frames gave 0.17 to 0.9 Mbit/s ([the proposal](../proposals/direct-path.md),
section 0).

The transfer app measured the pipe on real links. Its README gives the
runs: a median of 97.0 Mbit/s on a direct path, and about 89 Mbit/s and
10.5 Mbit/s through the TURN server of the relay.

`arc session --http` measured a body of 104,857,600 bytes between two
machines behind NAT on 2026-10-12 and 2026-10-13. The machines were two
boat.dev sandboxes on different hosts, with `arc` and `arc-http` of the
v0.26.0 release (`arc-0.26.0-linux-x86_64.tar.gz`, SHA-256 checked). The
relay was `wss://relay.republic.sh` on Fly.io. Each machine had a private
IPv4 address (`192.168.122.x`) and a different public IPv4 address from
the STUN server of the relay. The NAT kept the port. The firewall of each
machine dropped each unsolicited incoming packet. The round trip was 2 to
3 ms between the machines, so they were in one region, and about 7 ms to
the relay. The speeds do not show a long link.

The provider served the file with `python3 -m http.server` through
`arc serve`. The file is `big.bin` of section 8.2: byte i is i mod 251.
Six runs of each path. Each body that arrived had the SHA-256 of the file
(`85a38859…ebed76`). The speed is the speed of the path line (measured):

| Provider | Consumer | Path line | Median | Range |
| --- | --- | --- | --- | --- |
| `ARC_PATH=auto` | `--path direct` | `direct (srflx to srflx)` | 810.4 Mbit/s, 96.6 MiB/s | 669.2 to 827.1 Mbit/s |
| `ARC_PATH=auto` | `--path auto` | `direct (srflx to srflx)` | 788.4 Mbit/s, 94.0 MiB/s | 683.2 to 821.9 Mbit/s |
| `ARC_PATH=auto` | `--path turn` | `turn (relay)` | 68.3 Mbit/s, 8.1 MiB/s | 56.5 to 74.0 Mbit/s |
| `ARC_PATH=turn` | `--path turn` | `turn (relay)` | 49.6 Mbit/s, 5.9 MiB/s | 33.1 to 68.7 Mbit/s |

The relay sessions with `ARC_PATH=auto` on the provider ran on 2026-10-13,
after the sandboxes moved to other hosts; the machines were again behind
NAT.

The last row ran on 2026-10-12, on two other boat.dev sandboxes from the
snapshot `arc-ready`, with `arc` and `arc-http` of the v0.27.0 release
(`arc-0.27.0-linux-x86_64.tar.gz`, SHA-256 checked against `SHA256SUMS`).
The relay ran v0.27.0. Each machine was behind NAT, and its firewall
dropped each unsolicited incoming packet. `arc-http` on the provider had
`ARC_PATH=turn` in its environment. So each end had only an address on the
TURN server, and the relay carried each packet between two of its own
relay ports. The path lines of the six runs were 57.3, 33.1, 48.0, 50.2,
48.9 and 68.7 Mbit/s. After the six runs, each end had used about 683 to
686 MB of its quota for 629,145,600 bytes of bodies.

With the relay of v0.26.0, the same row gave no path in 6 of 6 runs: each
run stopped after 8.4 to 8.5 s with `no direct path, and no path through
the relay: no direct path`. On Fly.io, the relay ports listen on a private
address behind the public address, and the network dropped a packet from
the host to its own public address. Since v0.27.0, the relay sends such a
packet to the private address
([#264](https://github.com/gezibash/arc/issues/264)).

The commands, with `P` the key of the provider:

```sh
arc --home "$W/provider" keys gen
arc --home "$W/provider" relay add wss://relay.republic.sh
ARC_PATH=auto arc --home "$W/provider" serve ./app
arc --home "$W/consumer" keys gen
arc --home "$W/consumer" relay add wss://relay.republic.sh
arc --home "$W/consumer" install "$P" --yes
arc --home "$W/consumer" session --http --mode server_stream --timeout 10m --path direct "http+arc://$P/big.bin" > out.bin
sha256sum out.bin
```

`./app` holds `manifest.json`, a copy of
`cmd/arc/testdata/streaming-http/interface.json`, and this Arcfile:

```toml
version = 2

[serve]
command = "sh"
args = ["-c", "cd <directory of big.bin> && exec python3 -m http.server --bind 127.0.0.1 $PORT"]
protocol = "http"
manifest = "./manifest.json"
```

The order of section 3.5 was measured on 2026-10-13 with `arc sync`, five
runs before and five after the change. The device was a Mac behind the
router of a home. The three storage citizens were boat sandboxes behind a
NAT, with arc v0.28.1.

| Order | First attempt | Time to each storage citizen | Wall time of `arc sync` |
| --- | --- | --- | --- |
| No hold first | No path in each run. pion traces on the Mac and on one storage citizen: no packet arrived at either end. | 14.45 s to 15.37 s | 15.8 s to 17.0 s |
| Hold first | Opened in each run, path `srflx to srflx` | 2.04 s to 2.60 s | 3.3 s to 4.6 s |

### 8.3 Unverified

- No automated test runs two machines behind real NATs.
- No automated test makes the second direct attempt after a first attempt
  with no path through a real connection. `TestOffererSendsFirst` and
  `TestAnswererFirst` check the order with an attempt function that does
  not connect.
