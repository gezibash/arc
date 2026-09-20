# First direct carrier: TLS over TCP

## Decision

Use TLS 1.3 over TCP for the first [optional direct promotion](PROMOTION.md)
carrier. Carry bounded ARC packets over that connection. ARC relays keep
discovery, consent, and permission renewal. Mutual hole-punch consent permits
a bounded attempt when no configured listener is reachable. An attempt that
does not succeed keeps the existing ARC relay path.

This replaces an earlier WebRTC-first choice, which needed native build
dependencies. The carrier takes a smaller reachability scope instead of a
second toolchain. WebRTC can remain a later optional carrier, for browser
peers or for wider traversal.

The first request/reply carrier is built. Matching local policy files enable
it, as [direct request/reply](DIRECT.md) describes. It does not change the
relay-first default, and it is not a general transport tunnel.

## Dependency impact

The carrier uses the `crypto/tls` package of the Go standard library. It adds
no third-party transport package, no native component, and no second
toolchain. Each end pins the certificate of the other by its SHA-256 hash,
and proves its identity with a signature over exported keying material.

## Reachability scope

| Situation | First-profile behavior |
| --- | --- |
| Provider exposes an approved reachable listener | Citizen connects directly to the provider. |
| Citizen exposes an approved reachable listener | Provider connects back; application caller/provider roles remain unchanged. |
| Peers share an explicitly permitted local network | Try the approved local address. |
| A permitted public IPv6 address is reachable | Try it, subject to firewall policy. |
| Neither side has a reachable approved listener | Try source-port reuse only with mutual hole-punch consent; otherwise keep application traffic on ARC relays. |

[Resilient path selection](PATHS.md) defines how endpoints negotiate the dialer,
probe both permitted directions, and select a verified path while retaining a
working relay route. Application caller/provider roles never determine who must
accept the network connection.

Port mappings or firewall openings, when required, are explicit operator setup.
Mutual exact `hole_punch` policy can also use each endpoint's relay-observed
source address and reusable TCP source port for short-lived simultaneous active
and passive attempts. The relay-observed endpoint remains private to the
authenticated conversation and must appear in the other owner's `dial` list.
Fixed TLS client and server roles keep the carrier handshake unambiguous. The
socket strategy follows the active/passive combination described in
[RFC 6544, Appendix B](https://www.rfc-editor.org/rfc/rfc6544#appendix-B). ARC
does not change router settings, add a STUN service, or publish a discovery
service. Endpoint-dependent NATs, firewall policy, and OS socket behavior can
reject the attempt, which falls back to relays without replaying application work.
All candidates are scoped and authenticated; an observation is not a general
reachability claim.

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
admitted route contexts. It is not a raw database port or a runtime
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
