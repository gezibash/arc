# Direct pipe

- Status: partial
- Layers: sdk
- Owns: sdk/direct
- Proof: go test -count=1 -run '^(TestRelayPath|TestRelayPathWithoutSTUN|TestRelayQuota|TestSTUNURL)$' ./sdk/direct/
- Remaining: the citizen policy, the shared offer and answer, and the pipe type of section 3.7; the http, releases, storage citizen and upload steps of [the proposal](../proposals/direct-path.md), section 6; a test that a wrong certificate fingerprint fails, section 6.
- Unverified: No automated test runs two machines behind real NATs. The relay attempt on real machines was tested by hand only, with the transfer app. No test proves that pion refuses a wrong certificate fingerprint.

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

1. A direct attempt with no hold.
2. A direct attempt with a hold of 1 s.
3. A relay attempt with no hold, unless the user turned it off.

An offerer MAY swap the first two attempts. An attempt fails if the data
channel does not open in 12 s plus the hold. The hold that an answerer
takes MUST be at most 3 s. Before the second attempt and before the relay
attempt, the offerer MUST write the reason on its notes:

```text
no direct path in the first order of the packets; trying the other order
no direct path; trying the TURN server of the relay, so the bytes go through the relay
```

Each attempt is a new ARC call and a new connection. An attempt that fails
transferred no bytes, so the next attempt is not a replay.

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

### 3.7 The citizen policy, the shared offer and the pipe type

Not built. [The proposal](../proposals/direct-path.md), section 3.1, gives
the planned API: the policy `auto`, `turn` or `session`, a shared `Offer`
and `Answer` for the open request of a session, and a `Pipe` type that names
no pion type. Today an app defines its own request, and uses the pion types
of the data channel.

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
  the call. `sdk/direct` MUST NOT turn off this check. Not built: a test
  that a wrong fingerprint fails.
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
  app holds at most 4.

## 7. Compatibility

- The pipe moved from `apps/transfer/direct` to `sdk/direct` with no change
  on the wire. A transfer client of an older version and one of this
  version interoperate in each direction.
- An answerer of a version before the relay attempt ignores the named STUN
  server. The relay attempt can then fail if the STUN server of that end
  does not answer.
- An app that opens a pipe defines its own request. A change to the request
  is a change of that app.

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

The transfer app measured the pipe on real links. Its README gives the
runs: a median of 97.0 Mbit/s on a direct path, and about 89 Mbit/s and
10.5 Mbit/s through the TURN server of the relay.

### 8.3 Unverified

- No automated test runs two machines behind real NATs.
- No test proves that a wrong certificate fingerprint fails.
- No automated test makes the second direct attempt after a first attempt
  with no path.
