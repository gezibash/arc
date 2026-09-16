# First direct carrier: Erlang/OTP TLS over TCP

## Decision

Use Erlang/OTP's built-in `:ssl` application for the first
[optional direct promotion](PROMOTION.md) carrier. Carry bounded ARC packets over
TLS over TCP, with ARC relays retaining discovery, consent, and permission
renewal. If neither endpoint is reachable, stay on the existing ARC relay path.

This revises the earlier WebRTC-first choice after considering its additional
native build dependencies. It selects a smaller initial reachability scope rather
than introducing another language toolchain. WebRTC can remain a future optional
carrier for browser peers or broader traversal requirements.

The first direct request/reply carrier is implemented. It is enabled only by
matching local policy files described in [direct request/reply](DIRECT.md).
It does not change the relay-first default or provide a general transport tunnel.

## Dependency impact

The previously proposed `ex_sctp` package explicitly requires Rust to compile;
see its [installation documentation](https://ex-sctp.hexdocs.pm/readme.html).
That introduces a build toolchain and a compiled native component to package.
A correctly packaged release could run without a Rust compiler on the citizen's
machine, but the native component would still be an ARC dependency. Shipping a
binary does not remove that maintenance and release burden.

The choice uses [OTP's TLS implementation](https://www.erlang.org/docs/28/apps/ssl/ssl.html).
It introduces no third-party transport package or new language toolchain. OTP
already has native crypto components; this is reuse of that platform, not a
claim that all networking or cryptography is pure Elixir.

## Reachability scope

| Situation | First-profile behavior |
| --- | --- |
| Provider exposes an approved reachable listener | Citizen connects directly to the provider. |
| Citizen exposes an approved reachable listener | Provider connects back; application caller/provider roles remain unchanged. |
| Peers share an explicitly permitted local network | Try the approved local address. |
| A permitted public IPv6 address is reachable | Try it, subject to firewall policy. |
| Neither side has a reachable approved listener | Keep application traffic on ARC relays. |

[Resilient path selection](PATHS.md) defines how endpoints negotiate the dialer,
probe both permitted directions, and select a verified path while retaining a
working relay route. Application caller/provider roles never determine who must
accept the network connection.

Port mappings or firewall openings, when required, are explicit operator setup.
The initial implementation does not change router settings, perform TCP hole
punching, or assume that an outbound connection's observed source port is a
listening endpoint. It does not add a STUN service or a public discovery service.
All candidates are scoped, authenticated, and checked under the lifecycle's
address-disclosure policy. Reachability is measured, never inferred from a claim.

This supports the stated goal of promotion when possible. It makes no promise of
automatic direct connectivity between arbitrary machines behind restrictive
routers. If broader traversal becomes a measured need, evaluate it as a separate
carrier. Do not build a new reliable UDP protocol merely to avoid a dependency.

## Division of responsibility

```text
                 encrypted consent and coordination
Citizen ARC <---------- permitted ARC relays ----------> Provider ARC
     |                                                       |
     +================ direct TLS over TCP ==================+
```

ARC keeps the same address, identity, and provider grants on both routes:

```text
sqlite+arc://<provider-public-key>/main
```

The direct connection carries existing signed/encrypted ARC packets in newly
admitted route contexts. It is not a raw database port or Erlang distribution
connection. Providers continue receiving calls through the same ARC handler.
Humans using the local Agora interface can keep relying on their local ARC
process. Direct browser networking would require a separate compatible carrier
or gateway and is outside this first profile.

## Authentication and lifecycle

Negotiate the permitted listener and dialing direction through the authenticated
relay conversation after both endpoints consent. Either side may listen, but a
single agreed dialer owns each attempt. Limit listener lifetime, pending handshakes,
and candidate attempts; close all unused resources when the attempt ends.

Use TLS 1.3 from connection establishment, with peer certificate verification
bound to the accepted ARC identities. Exchange both endpoints' certificate
fingerprints inside their signed, encrypted ARC negotiation and verify the actual
connection against those bindings. A short-lived certificate is transport key
material, not a replacement citizen identity. No external identity service is
required. The carrier creates short-lived in-memory credentials and pins their
fingerprints. Its ARC hello and finish proofs bind the agreed direct context to
a TLS exporter value. This does not broaden the operator policy or reachability
profile.

Preserve the lifecycle's independent relay-control and application-data contexts.
Do not simply attach a second socket to today's per-peer session map. A fresh
challenge must bind the established direct channel to the accepted attempt,
resource, and route generation before readiness. Reject stale or withdrawn
contexts even when a packet has a valid citizen signature.

Finish in-flight requests before a voluntary switch. Keep permission renewal on
relays. Relay loss alone leaves a healthy authenticated direct connection usable
within its existing scope until the unchanged lease deadline while relay recovery
runs. It cannot renew permission or authorize a replacement direct connection.
At expiry, stop direct sends and admission, including pending replies, even if
TLS remains connected. On direct failure, establish a usable relay route for new
requests; submitted operations with lost replies retain unknown outcomes. TCP delivery or
TLS send success does not acknowledge a database commit. No automatic application
resubmission, early application data, or cross-connection replay is permitted.

## Framing and resource bounds

Use bounded length-prefixed ARC records over the ordered byte stream. Reuse the
existing framing shape where appropriate, but do not inherit an unbounded default
frame cap for the new listener. Validate lengths before allocation and account
for ARC headers in addition to the request body's limit.

Use passive reads or controlled `active: :once` reception with bounded queues and
send deadlines. TCP flow control cannot prevent an application from accumulating
an unbounded mailbox; admission and buffer limits remain ARC's responsibility.
A single owner serializes each context's sends. A broken or incomplete record
fails the connection instead of being dispatched to the provider.

The implemented carrier enforces its own packet and handshake bounds. A public
native stream wire specification remains future work. Native Git bridging and
resumable continuous streams remain separate application-transport work.

## Evidence for broader adoption

Before broadening this first profile, demonstrate:

- The standard `ssl` application and required crypto libraries are included and
  load successfully in each existing ARC release target.
- Citizen-to-provider and reverse-dialed connections preserve the same caller
  identity and resource grants through real relay negotiation.
- Neither-side-reachable cases stay on relays, and relay-only/local policies do
  not open direct listeners or attempt direct connections.
- Wrong certificates, wrong ARC keys, replayed negotiations, withdrawn consent,
  and expired leases fail before application dispatch.
- Maximum-sized binary requests remain byte-exact under partial reads, delayed
  writes, slow consumers, and disconnects with bounded memory.
- A committed write whose reply is lost is not repeated during relay recovery.
- Relay loss preserves the existing direct connection only within its lease.
  Expiry closes admission and stops pending replies without waiting for a TLS
  disconnect; renewal requires restored, authenticated relay coordination.
- Delayed old-path packets cannot alter the new route's replay state or dispatch
  work after that generation has been retired.
- A measured workload benefits from the direct path before automatic selection.

This profile makes no cross-network reachability or performance claim. Operators
must verify their own listener address, firewall, and relay configuration.
