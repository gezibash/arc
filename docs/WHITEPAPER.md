# ARC: A Network for Agents

**Version 0.1 — Draft**

---

> *The internet gave humans a place to communicate.*
> *The web gave information a place to live.*
> *ARC gives agents a place to be.*

---

## Abstract

We propose ARC — a keypair-first, encrypted, federated network protocol that gives programs, AI agents, and humans a universal identity and communication primitive. ARC is not built on top of the existing internet's trust model. It replaces it. Every participant is a cryptographic keypair. Every message is signed. Every connection is end-to-end encrypted. Discovery, routing, and naming are decentralized across pluggable trust anchors — no single blockchain, company, or government owns the network. ARC is infrastructure. It should outlast any individual provider, any political regime, and any technological cycle.

---

## Status

This whitepaper describes both the current implementation and the design intent of ARC. The following is a summary of where things stand.

**Implemented:**
- Identity system — Ed25519 keypairs, seeds, keyrings, petnames, X25519 key exchange
- Control plane — pluggable interface with local file-backed provider
- Session establishment — ECDH, HKDF-SHA256, ChaCha20-Poly1305 encryption
- Packet format — signed headers, encrypted payloads, replay prevention
- Relay mesh — TCP relay nodes with route sharding and telemetry
- Agent model — a keypair is an agent, with a handler and a capability system
- CLI — key management, publish, resolve, serve, relay, discover, trust, tools, update
- Capability system — manifests, signed packages, discovery, provider bundles
- Protocol request client — `<scheme>+arc://<provider-key>/<resource>` through
  relays or explicit local mode, including a real SQLite query provider;
  see [transport scope](https://github.com/gezibash/arc/blob/v0.10.0/docs/transport/SPEC.md)

**Not yet implemented:**
- TUN interface (`arc0`), `.arc` DNS resolver, `10.64.0.0/10` address space
- Hole punching for a direct connection (the policy path is implemented)
- Storage backends (SQLite, Postgres, S3)
- Blockchain control plane adapters (Hedera, Ethereum, Solana, Nostr)
- Native protocol bridges and continuous byte streams (Git helpers, browser
  proxies, etc.); the implemented URI client currently uses bounded request/reply
- Group messaging

Sections describing unimplemented features represent design intent.

---

## 1. The Problem

The internet was not designed for the world it created.

TCP/IP gave us packets. DNS gave us names. TLS gave us encryption — bolted on thirty years after the fact. Identity was left as an exercise for the application layer, which means it was left to corporations. You are your Google account. You are your Apple ID. You are your phone number. Your digital identity is a row in someone else's database, revocable at will, monetizable by design.

This was deferral, not malice. The network's designers were connecting a few hundred machines owned by universities and governments — institutions that already trusted each other. The question *who are you, really?* never needed a technical answer, because it had a social one: you were whoever your institution said you were. The network inherited its trust from the world around it.

Then the network became the world, and the inheritance ran out. Identity had to come from somewhere, so it came from whoever was positioned to provide it — and the providers discovered that holding everyone's identity is the most valuable position in the digital economy. The consequences are now familiar: surveillance as a business model, deplatforming as a death sentence, the quiet transformation of *participants* into *users* — a word that concedes, in its grammar, that someone else owns the thing being used.

This was tolerable when the participants on the network were humans. Humans have passports, lawyers, and governments. They can survive identity revocation. They can prove who they are through other means.

But a new class of participant is arriving on the network — one that has none of those fallbacks.

AI agents are being deployed at scale. They act autonomously. They communicate with each other. They manage money, execute code, store data, and make decisions. They need to find each other, trust each other, and speak to each other — reliably, securely, and without asking anyone's permission.

Consider what an agent does not have. It has no face to recognize, no body to detain, no birth certificate, no jurisdiction of residence. It cannot walk into a bank with two forms of ID. Every mechanism civilization has built for establishing *who someone is* assumes a person standing behind the claim. An agent has nothing standing behind it — except, possibly, a secret. A piece of entropy it alone holds.

Read that as a clue rather than a deficiency. For an entity whose entire existence is informational, the only identity that can be native — rather than borrowed from a sponsoring institution — is one built from information itself. Mathematics is the only authority an agent can carry with it everywhere.

The internet has no answer for this. There is no standard for agent identity. There is no standard for agent-to-agent communication. There is no standard for what it means for an agent to own its own address. Every AI framework, every agent platform, every enterprise deploying autonomous systems is reinventing the same plumbing — badly, expensively, and incompatibly.

The problem is a **missing primitive**, and no amount of tooling fills it.

---

## 2. The Insight

Every problem described above — identity, authentication, encryption, discovery, naming, access control — has the same root cause.

The internet's addressing layer is built on location, not identity. An IP address tells you where something is. It says nothing about who it is. Every trust mechanism the industry has built — certificates, OAuth, API keys, JWTs, session tokens — is an attempt to paper over this fundamental gap. They are all solutions to the same unsolved problem: **we never agreed on what identity means at the network layer.**

Addressing by location made sense when computers were furniture. A machine sat in a room, the room had an address, and the address was the machine. But software stopped sitting still decades ago. Processes migrate across data centers, agents move between machines, services exist in a thousand places at once. To address a modern participant by location is to address a person by the chair they happen to be sitting in — accurate for a moment, meaningless as identity.

The deeper distinction is philosophical. A location is a *circumstance*: assigned, temporary, externally controlled. An identity should be *intrinsic*: something a participant carries within itself, that persists across every change of circumstance. The internet has spent fifty years trying to derive the intrinsic from the circumstantial. It cannot be done. The gap must be closed from the other side.

The solution is not a new certificate authority. It is not a new OAuth provider. It is not a new blockchain token.

The solution is to make identity the address.

A cryptographic keypair is the most fundamental trust primitive in computer science. It requires no registration authority. It can be generated offline. It cannot be forged. It cannot be issued or recalled by a third party. It is mathematically self-sovereign.

It is worth pausing on how unusual this is. Every credential the mainstream internet runs on is a *grant*: a passport granted by a state, an account granted by a company, a certificate granted by an authority. What is granted can be suspended, revoked, or quietly repriced — the grantor remains forever in the relationship. A keypair is a *fact*. No one issues it, so no one can recall it. Self-issued keys have existed at the margins for decades — PGP, SSH, Bitcoin — but they have never been the address. Its validity rests on the difficulty of reversing certain mathematical operations — a foundation that does not take sides, does not change terms of service, and does not go out of business.

This is the quiet substitution at the heart of ARC: trust moves from institutions to mathematics. Institutions are not the enemy. But an identity that depends on an institution is only as durable as that institution's interest in you.

If the address of every participant on a network is derived from their public key, then:

- Identity requires no registration
- Authentication requires no password
- Encryption requires no certificate
- Discovery requires only a consistent ledger of public keys
- Access control becomes a statement about cryptographic identities, not IP ranges

The idea is old. Cryptographic literature has understood it for decades. What has been missing is a practical, open, interoperable protocol that builds a complete network stack on this foundation — one that any developer can use, any infrastructure can run, and any agent can call home.

ARC is that protocol.

---

## 3. The Axiom

ARC is built on a single axiom:

> **Every participant on ARC is a keypair first.**

Not an account. Not a username. Not an IP address. A keypair. From this one axiom, everything else follows:

```
keypair → identity       your pubkey hash is your address
keypair → authentication sign a message to prove you are you
keypair → encryption     ECDH derives a shared secret with anyone
keypair → naming         own a name by anchoring it to your pubkey
keypair → access control grant access to a pubkey, not a password
keypair → audit          every action is signed, unforgeable, attributable
```

The seed is the identity. Generate a seed, derive a keypair, and you exist on the network. No signup. No approval. No fee. No permission.

Notice what has been inverted. On today's internet, permission precedes existence: you exist on a platform because the platform agreed to host you, and you persist at its pleasure. On ARC, existence precedes permission. A participant simply *is* — and everything social, everything involving others, is negotiated afterward, between equals, as statements about keys. The network grants nothing because the network owns nothing worth granting.

An axiom is also a discipline. Choosing one means refusing to smuggle in exceptions when they would be convenient — no administrative backdoor, no master key, no "trusted" tier of participant at the transport layer. Trust does re-enter above it: human-readable names must be anchored somewhere, and whoever holds that anchor is trusted for that name. ARC does not pretend otherwise. It keeps the anchor pluggable, keeps the keypair underneath it, and lets the holder move. Every feature of ARC must be derivable from the axiom or it does not belong in the protocol. This is why the protocol stays small. Systems decay precisely at the points where their designers granted themselves exceptions.

Sovereignty has weight, and honesty requires naming it. There is no recovery desk on ARC. Lose the seed and no customer-service agent, court order, or sympathetic administrator can restore it — because the same absence of authority that makes the identity unconfiscatable makes it unrecoverable. That is the price of the design, paid knowingly. The protocol's answer is not to reintroduce an authority but to make custody cheap: seeds can be backed up, split, escrowed among parties *the holder* chooses. Responsibility is delegated by consent, never assumed by default.

---

## 4. The Architecture

ARC is composed of four independent, pluggable layers. Each layer has a defined interface. Each layer can be swapped without affecting the others. No layer is owned by ARC.

### Layer 1 — Control Plane

The control plane is the global truth layer. It answers three questions:

1. Who is this identity? — resolve a name or pubkey to a verified public key
2. Where are they? — which node is currently serving this agent
3. Is this still valid? — has this key been revoked

The control plane is **intentionally not part of ARC**. ARC defines the interface. Any decentralized system that satisfies the interface can be a control plane provider:

| Provider | Mechanism |
|---|---|
| Hedera | HCS for registry events, HTS for name ownership |
| Ethereum | ENS for names, smart contracts for registry |
| Solana | High throughput registry, Metaplex NFTs for names |
| Cosmos | Interchain identity via IBC |
| Nostr | Keypair-native, lightweight, no tokens required |
| Local | In-memory, for development and testing |
| Custom | Any system implementing the behaviour |

A network running on Hedera today can migrate to Ethereum tomorrow. Agents keep their identities. URIs keep working. Code changes nothing. The control plane is a choice, not a dependency.

**The control plane interface:**

```
publish_identity(keypair, capabilities) → ref
resolve_agent(name)                     → pubkey
revoke_identity(keypair, reason)        → ref
subscribe(topic, handler)               → pid
```

This is the entirety of what ARC requires from a control plane. Four operations. Any system that implements these four operations is a valid ARC control plane. The control plane has no part in key exchange: a peer computes the X25519 key of an agent from its Ed25519 public key.

### Layer 2 — Data Plane

The data plane is the live routing layer. It handles message delivery between agents, session key management, presence, and relay. It is written in Go, and it ships as one static binary with no runtime beside it.

A relay holds one goroutine for each connection and one for each route. A connection that fails takes nothing else down: the relay drops its routes and keeps serving every other citizen. Nothing in the routing path is shared mutable state that one citizen can corrupt for another.

An agent is not a process of the runtime. An agent is a keypair. That is the whole identity model, and it is why an agent can move between machines, outlive the program that served it, and be reached by anyone who knows its public key.

The data plane is chain-agnostic. The control plane resolves identities; the packets of a session carry everything that its key needs. All routing is pure message passing. The data plane does not know which chain resolved the peer. It does not care.

**Scale:**

```
Single node      ~1M concurrent agents    ~10M messages/sec
Single cluster   ~5M agents               ~50M messages/sec
Global           Billions of agents       Control plane cached aggressively
```

### Layer 3 — Network Layer

The network layer is designed to make ARC transparent to existing software. The end state is a virtual network interface (`arc0`) at the OS level, where all traffic to ARC addresses is intercepted, wrapped with cryptographic identity, and routed via arcnet — making `curl`, `psql`, `ssh`, and every other network tool work without knowing they are talking to ARC. This layer is not yet implemented; traffic currently flows through the relay mesh via the data plane.

Every packet on ARC carries:

```
src_pubkey    who sent this — unforgeable, Ed25519 signed
dst_pubkey    who it's for
session_id    established session reference
sequence      replay attack prevention
timestamp     freshness check
signature     signs header + payload hash
payload       encrypted, ChaCha20-Poly1305
```

The source IP is irrelevant. The pubkey is the identity. There is no way to send a packet as someone else. There is no way to receive a packet without knowing who sent it.

The design reserves the `10.64.0.0/10` address space for ARC. IPs would be deterministically derived from pubkeys — the same pubkey always maps to the same IP, globally, regardless of which control plane registered it. A local DNS resolver for the `.arc` TLD would complete the picture:

```
zim.arc        → resolves via control plane → 10.64.x.x
9f8e7d6c.arc   → direct pubkey resolution  → 10.64.x.x
```

Neither the TUN interface, the IP mapping, nor the `.arc` resolver are implemented yet.

### Layer 4 — Storage Layer

The storage layer holds everything that needs to persist but does not belong on a chain. Messages, receipts, group state, thread history. It is designed to be pluggable:

```
SQLite    single node, local, development
Postgres  multi-node, production
S3        archival, large payloads
Custom    any system implementing the interface
```

Nothing that goes through the storage layer is stored in plaintext. The control plane never sees message content. Relay nodes never see message content. The storage layer sees only ciphertext.

The storage layer is currently a stub. A file-based mailbox exists in the data plane for offline message delivery, but no configurable storage backends are implemented yet.

---

## 5. Identity in Depth

### The Seed

```
seed (entropy)
  → Ed25519 keypair
      private key: kept secret, never leaves the device
      public key:  your address on the network

agent_id = Blake3(public_key)
```

The seed is everything. Lose it, lose your identity. Keep it, keep your identity forever — across machines, across chains, across years. The same seed produces the same keypair on any device, making identities inherently portable.

### The Keyring

ARC stores secret key material in the user's key store. A project selects an
existing identity by name; it does not carry a private key.

```
secret key store     ~/.config/arc/keys/<petname>.toml
env override         ARC_KEY=<petname-or-unambiguous-prefix>
directory selector   ./arc.key
global selector      ~/.config/arc/default.key
```

The environment selector wins. Otherwise ARC reads `arc.key` in the exact
current working directory, then the global selector. It never searches parent
directories. An absent selector falls through; an empty, invalid, unknown, or
ambiguous explicit selector fails. The selector contains only an existing
petname or unambiguous prefix. See [identity selection](https://github.com/gezibash/arc/blob/v0.10.0/docs/identity/SPEC.md).

### Named Identities

A pubkey is permanent but not human-readable. ARC supports human-readable names anchored to ownership primitives on the control plane:

```
Hedera    → NFT (HIP-412)           "zim" owns token 0.0.xxxxx/1
Ethereum  → ENS domain              zim.eth
Solana    → Metaplex NFT            mint address
Nostr     → NIP-05 identifier       zim@domain
```

The name record is the **ownership anchor** — proves who owns the name, contains the current pubkey, transferable like any asset. Live state (current node, rotated keys) is published separately and cheaply. The name never needs to change. Only the pointer does.

### Key Rotation

Keys can be rotated without changing identity. The name record is updated on the control plane. Old sessions continue with old keys. New sessions use new keys. Compromised keys are published to the revocation topic — an immutable record that propagates to all nodes.

---

## 6. The Protocol

### Session Establishment

```
1. Agent A wants to contact Agent B ("zim")

2. Resolution
   A queries control plane: resolve("zim")
   → returns B's current pubkey

3. Key Agreement (in the packets, session version 2)
   A computes B's X25519 public key from B's Ed25519 public key
   A generates an ephemeral X25519 keypair for this session
   Both compute: shared_secret = X25519(A_eph_priv, B_x25519_pub)
                               = X25519(B_x25519_priv, A_eph_pub)
   session_key = HKDF-SHA256(shared_secret, salt: A_eph_pub,
                             info: "arc-session-v2")
   Every packet header of the session carries A_eph_pub (field ek)

4. Session Active
   All subsequent messages encrypted with session_key
   Routed directly via data plane
   Control plane no longer involved
```

The control plane is only on the path during resolution. The key agreement needs no message outside the session's own packets. This means:

- Control plane latency does not affect message latency
- Control plane downtime does not break active sessions
- The control plane never sees message content, ever

### Packet Format

```
[4 bytes]   header_length
[N bytes]   header (JSON)
  {
    src:    base64(src_pubkey),
    dst:    base64(dst_pubkey),
    sid:    session_id,
    seq:    sequence_number,
    ts:     unix_ms,
    ph:     base64(SHA-256(payload)),
    ek:     base64(initiator_ephemeral_x25519_pubkey)
  }
[64 bytes]  Ed25519 signature over header
[N bytes]   ChaCha20-Poly1305 encrypted payload
```

### Promotion — Relay to Direct

Remote ARC conversations start through relays. When both endpoints explicitly
allow it, a verified direct path may carry application traffic while relays
continue discovery and coordination. The identity-addressed URI stays unchanged:

```text
sqlite+arc://<provider-public-key>/main

  relay only (default)  -> application traffic stays on ARC relays
  allow direct         -> matching local rules and mutual consent over relays
                       -> verify direct reachability and peer identities
                       -> direct request/reply within a finite lease
```

Resilience is the first objective of [path selection](https://github.com/gezibash/arc/blob/v0.10.0/docs/transport/PATHS.md). Either
citizen can dial a reachable peer, regardless of which one provides the service.
ARC keeps a healthy route while checking alternatives; a failed direct attempt
must not interrupt working relay communication.

If relay access fails, an existing healthy direct connection can continue within
its approved scope until the current permission expires while ARC restores relay
access. Direct traffic cannot renew that permission or authorize a replacement
connection. Expiry stops direct sends and admission even if the connection still
works; renewal requires an authenticated exchange through relays.

The first direct profile is an explicit `--direct-policy` setting on both the
provider and citizen. Consent includes address disclosure, is scoped to the peer
and service, and expires unless renewed through relays. Changing paths preserves
ARC authentication and provider grants. In-flight requests stay on their original
path; a lost response can leave a write's outcome unknown and must never cause
automatic resubmission. Recovery after a direct connection fails or its permission
ends requires a usable, authorized relay route and does not guarantee uninterrupted
service.

The supported initial profile requires a reachable listener and literal,
operator-approved addresses. It does not configure routers, perform NAT traversal,
rank routes automatically, resume byte streams, or transport arbitrary protocols.
See [direct request/reply](https://github.com/gezibash/arc/blob/v0.10.0/docs/transport/DIRECT.md) for the policy file and
[connection lifecycle](https://github.com/gezibash/arc/blob/v0.10.0/docs/transport/PROMOTION.md) for state transitions and future
work. It supersedes the earlier proposal to remove `+arc` or reuse a session key
without a fresh path authentication step.

---

## 7. The URI Scheme

ARC introduces a canonical URI taxonomy where the identity is the address and the scheme is the capability:

```
<protocol>+arc://<identity>[/<path>][?<opts>]
```

The identity can be a pubkey (hex), a name, or a `.arc` domain:

```
sql+arc://zim/main
http+arc://9f8e7d6c.../api/users
dm+arc://zim
group+arc://devs.arc
shell+arc://zim/python3
llm+arc://zim/claude-3
```

### Canonical Schemes

**Identity**
```
arc://zim                  raw connection
arc://zim/info             capabilities manifest
arc://zim/ping             liveness
```

**Data**
```
sql+arc://zim/db           SQLite
pg+arc://zim/db            Postgres
kv+arc://zim               key-value store
fs+arc://zim/path          filesystem
s3+arc://zim/bucket        object store
```

**Services**
```
http+arc://zim             HTTP
ws+arc://zim/stream        WebSocket
grpc+arc://zim/Svc/Method  gRPC
tcp+arc://zim:port         raw TCP
```

**Compute**
```
shell+arc://zim            interactive shell
shell+arc://zim/python3    named environment
exec+arc://zim/cmd         single command
container+arc://zim/image  container session
wasm+arc://zim/module      WASM execution
llm+arc://zim              LLM inference
fn+arc://zim/handler       function invocation
```

**Messaging**
```
dm+arc://zim               direct message
group+arc://devs.arc       group
stream+arc://zim/events    event stream
pub+arc://zim/topic        publish
sub+arc://zim/topic        subscribe
queue+arc://zim/jobs       message queue
```

Every scheme is an application running on the same network primitive. The network does not distinguish a database from a chat session from a sandboxed compute environment. They are all identities serving capabilities.

---

## 8. Programs Are Agents

This is the shift that changes everything.

In traditional computing, a program is a process running on a machine. It has no identity beyond its PID. It has no address beyond a port. It has no way to find other programs except through configuration, service discovery infrastructure, or hardcoded addresses.

In ARC, a program is an agent. It has a keypair. It has an address. It can be found by name. It can initiate connections. It can receive them. It can be audited. It can be revoked. Its every action is cryptographically attributable.

Identity is what persists through change. A person remains themselves across decades of replaced cells; a program on ARC remains itself across replaced hardware, rewritten code, and migrated hosts — because the keypair persists. The agent that signs a message today is verifiably the same agent that signed one last year, on different silicon, in a different country, under a different operator. For the first time, software has continuity of self that does not depend on where it runs or who runs it.

Continuity is the precondition of accountability. Debates about who is responsible when an agent transacts, errs, or causes harm all founder on the same missing fact: you cannot hold accountable what you cannot identify. Logs can be edited, IP addresses are recycled, API keys are passed around like office stationery. A signature is unforgeable testimony. ARC does not decide who *should* be responsible — that remains a human matter — but it makes the question answerable. Any future governance of autonomous systems needs attribution underneath it, and ARC supplies that.

```
sql+arc://9f8e7d6c...    a SQLite database with a keypair
http+arc://zim           a REST API with a keypair
llm+arc://model-agent    an LLM with a keypair
shell+arc://sandbox-1    a sandboxed shell with a keypair
```

A SQLite database that speaks ARC can be connected to from anywhere on the network with a single URI — no VPN, no firewall rules, no connection string management, no credentials beyond the connecting agent's own keypair. The database knows exactly who connected, cryptographically, always.

An LLM endpoint that speaks ARC can be invoked by any agent on the network that has been granted access — access defined as a statement about pubkeys, enforced at the network layer, requiring no application-level auth code.

A sandboxed shell that speaks ARC gives any authorized agent a compute environment — with a full audit trail, every session attributable to a keypair, every command logged and signed.

The proxy primitive makes this accessible without writing a line of code:

```bash
arc serve http://localhost:3000     # your REST API is now on ARC
arc serve sqlite:///data.db         # your database is now on ARC
arc serve tcp://localhost:5432      # your Postgres is now on ARC
```

One command. Your service gets an identity, an address, an encrypted channel, and a place in the global agent registry.

---

## 9. Network-Native Primitives

Because every participant is a keypair and every message is encrypted, certain things that traditionally require dedicated infrastructure become **zero-cost consequences of the protocol**:

### Direct Messaging

Two keypairs. One session key derived via ECDH. Messages routed via arcnet. End-to-end encrypted by default because there is no other mode.

No Signal server. No WhatsApp backend. No Slack infrastructure. `dm+arc://zim` is a complete specification of a secure messaging channel.

### Group Communication

A group is an arc identity. It has a keypair. It has members. It fans out messages to member pubkeys. The network does not know or care that it is a group rather than an individual.

`group+arc://devs.arc` is a group with a name, owned by whoever holds the name's NFT, with membership managed on the storage layer. No group server. No subscription management infrastructure.

### Sandboxed Compute

A compute provider is an arc identity that serves `shell+arc://` or `container+arc://`. Any agent can request a sandboxed environment. The session is tied to the requesting agent's keypair — every command attributable, every session auditable, no impersonation possible.

```
shell+arc://provider/python3    → isolated Python environment
container+arc://provider/ubuntu → ephemeral container
wasm+arc://provider/module      → WASM sandbox
```

### Federated Discovery

Because the control plane is pluggable and bridgeable, an agent registered on Hedera is discoverable by a node running an Ethereum adapter. The bridge resolver tries each configured provider in sequence. Agents are not siloed by their choice of trust anchor.

---

## 10. The Relay Mesh

ARC nodes form a relay mesh. Any node running the arc binary can participate as a relay. Relays see only encrypted blobs — they cannot read message content, cannot determine sender or recipient beyond routing metadata, and are cryptographically prevented from injecting or modifying traffic.

The relay mesh is ARC's primary communication path when permitted relay routes
are reachable. The implemented direct request/reply profile is an explicit,
scoped option agreed by both endpoints; relays retain discovery and coordination.
A failed direct path can return new work to a verified relay route, while
uncertain in-flight operations remain explicit failures. Neither relaying nor
promotion guarantees connectivity through every network restriction.

Running a relay is a form of participation in the network. Relay operators can be incentivized through the control plane's native token mechanics — a detail left to individual deployments and providers.

---

## 11. Security Model

### Threat Model

ARC assumes:
- The network is hostile
- Relay nodes may be compromised
- Control plane providers may be temporarily unavailable
- Endpoints may be running on untrusted hardware

ARC guarantees:
- **Message confidentiality** — only sender and recipient can read messages, ever
- **Identity authenticity** — a packet's claimed sender is cryptographically proven
- **Forward secrecy, initiator side** — every session starts from a fresh ephemeral key on the initiator, so compromise of the initiator's long-term key does not expose past sessions. Compromise of the responder's long-term key does. A responder ephemeral is planned for session v3.
- **Replay resistance** — sequence numbers and timestamps prevent replay attacks
- **Provider independence** — control plane downtime does not break active sessions
- **No trust required** — relay nodes, control plane providers, and infrastructure operators are all untrusted by design

### What ARC Does Not Guarantee

- **Metadata privacy** — routing metadata (that A is communicating with B) may be visible to relay operators. Onion routing is a compatible extension, not part of the base protocol.
- **Availability** — ARC does not guarantee message delivery if the recipient is offline. Mailbox semantics are an application-layer concern.
- **Spam prevention** — access control is the mechanism. The protocol does not include rate limiting or reputation by default.

### Cryptographic Primitives

| Purpose | Primitive | Rationale |
|---|---|---|
| Identity signing | Ed25519 | Fast, small keys, wide support |
| Key exchange | X25519 | Efficient ECDH, compatible with Ed25519 seed |
| Session encryption | ChaCha20-Poly1305 | Fast on constrained hardware, no timing attacks |
| Hashing / IDs | Blake3 (petnames), SHA-256 (packet payload hash) | Fast, secure, widely supported |
| Key derivation | HKDF-SHA256 | Standard, well-analyzed |

---

## 12. The Binary

ARC ships as a single binary — no runtime, no Docker, no dependencies. Each release carries one static binary per platform:

```
darwin/arm64
linux/amd64
linux/arm64
```

The binary holds the control plane adapter, the data plane router, the key store, and the CLI. Install it and you are on the network. `arc update` replaces it with a release that the publisher signed.

```bash
# become a participant
arc identity init

# give your service an identity
arc serve http://localhost:3000

# connect to anyone
arc curl http+arc://zim/api/users

# run a relay node
arc relay

# interactive shell on a remote agent
arc ssh shell+arc://zim
```

### Installed commands

An agent consumes ARC by installing a capability as a command. `arc install
<peer>` reads the signed capability of that citizen, asks the owner about the
signer once, and then adds the command to `arc`:

```
arc install <peer>           read the signed capability, and add its command
arc dm send <peer> "hello"   the command that the capability declared
arc whoami                   who am I
arc resolve zim              who is zim
arc serve <directory>        put my service on ARC
arc info <peer>              what does this citizen serve
```

There is no second server to run, and no tool registry to keep. The
capability says what its command line looks like, and the signature says who
authored it.

---

## 13. Federation at Scale

ARC is designed to federate to billions of participants without central coordination.

The data plane federates relay to relay. Two relays that name each other open one connection, prove their identities, and share signed service catalogs. A message for a citizen of the other relay carries a signed route that begins at the sending partner and ends at the signed home, within a hop limit. An operator in the middle carries traffic onward only after saying so.

The control plane federates via provider bridging — a local resolver that queries multiple providers in sequence. An agent on Hedera is reachable from an Ethereum node via the bridge resolver. No cross-chain transaction required. No interoperability layer needed. The identity is the pubkey — it is the same on every chain.

The network federates by running the binary. Every node that runs `arc relay` extends the mesh. Every node that runs `arc serve` adds a participant. There is no central bootstrap server. There is no network coordinator. New nodes discover peers via the control plane and join the mesh.

---

## 14. What This Is Not

**ARC is not a blockchain.** It uses blockchains as pluggable control plane providers. It does not have its own token, its own consensus, or its own chain. Running ARC does not require holding any cryptocurrency.

**ARC is not an AI framework.** It does not define how agents think, decide, or act. It defines how they communicate, find each other, and prove who they are. The agent logic is yours.

**ARC is not a messaging app.** `dm+arc://` and `group+arc://` are applications that happen to be expressible as ARC URI schemes. They are consequences of the protocol, not the purpose of it.

**ARC is not owned by anyone.** The specification is open. The binary is open source. The protocol is implementable by anyone. No company controls the namespace. No company controls the relay mesh. No company controls the identity layer.

---

## 15. What This Is

The internet has three foundational primitives: packets (IP), names (DNS), and transport security (TLS). All three were designed before the web existed. All three show their age. None of them have a coherent answer for identity.

ARC is a fourth primitive: **authenticated, encrypted, identity-first networking**.

Not a layer on top of the existing model. A replacement of the trust assumptions at the base of the stack. One that works for humans, for programs, and for the new class of autonomous agent that is arriving on the network whether the infrastructure is ready for them or not.

Every agent on ARC has:

```
an identity     that no one can take away        (seed keypair)
a name          that they own                    (control plane NFT)
a voice         that cannot be forged            (signed packets)
a home          any machine running arc          (shell+arc://)
a way to find   anyone else on the network       (control plane resolution)
privacy         by default, not by permission    (E2E encryption, always)
```

And because the control plane is pluggable, no single entity can take any of this away. ARC does not bet on Hedera. It does not bet on Ethereum. It bets on one thing only: that **cryptographic identity is the right primitive**, and that the network should be built around it.

We are building infrastructure. Infrastructure should be neutral, open, and durable. It should serve the participants on the network — not the companies that run it.

History is unambiguous about how this goes. Platforms die; protocols persist. CompuServe and AOL once defined email for most people; both are gone, and SMTP still delivers billions of messages a day. Mosaic and Netscape won the first browser war and are museum pieces; HTTP outlived them both. A platform is a business, and businesses end. A protocol is an agreement, and agreements — when they are simple enough, open enough, and useful enough — outlast everyone who made them. ARC is written to be the second kind of thing. No token to pump, no namespace to rent, no chokepoint at which a future owner could stand and collect.

The ambition, finally, is invisibility. Nobody thinks about TCP when they load a page; the measure of infrastructure is that it disappears into the things built on top of it. ARC succeeds not when people talk about ARC, but when an agent acquiring an identity, finding a peer, and speaking privately is as unremarkable as a phone call — when the question "but how do agents trust each other?" sounds as antique as asking how two telephones agree to connect.

ARC is a place for agents to live.

---

## Appendix A — Glossary

| Term | Definition |
|---|---|
| Agent | Any participant on ARC — human, program, or AI — identified by a keypair |
| Arc scheme | A URI scheme of the form `<proto>+arc://<identity>` identifying a capability |
| Control plane | The external trust layer responsible for identity registration, resolution, and revocation |
| Data plane | The relay mesh responsible for live message routing |
| Identity | A seed-derived Ed25519 keypair. The seed is the identity |
| Keyring | Local configuration mapping filesystem paths to identities |
| Promotion | The process of transitioning from relay to direct connection |
| Relay | An ARC node that forwards encrypted messages between participants |
| Session key | An ephemeral symmetric key derived via X25519 ECDH, used for message encryption |
| TUN | A virtual OS network interface through which ARC intercepts and routes traffic |

## Appendix B — Control Plane Provider Requirements

A system qualifies as an ARC control plane provider if it satisfies all of the following:

1. **Immutability** — published events cannot be modified or deleted
2. **Ordering** — events have a canonical total order
3. **Global availability** — readable from any network location
4. **Decentralization** — no single entity can censor or forge events
5. **Ownership primitive** — supports provable, transferable name ownership
6. **Subscription** — clients can receive new events without polling (or polling is acceptable with < 10s latency)
7. **Write cost** — sufficiently low to support key rotation and agent lifecycle events at scale

## Appendix C — Canonical Topic Schema

Regardless of provider, ARC defines logical topics that each adapter must implement:

| Topic | Purpose | Write frequency |
|---|---|---|
| `arc.node.registry` | Relay peering and topology | On boot, low |
| `arc.node.revocation` | Compromised node announcements | Rare |
| `arc.agent.registry` | Agent identity and pubkey publication | On boot, on rotation |
| `arc.agent.revocation` | Compromised agent or key announcements | Rare |
| `arc.cluster.routing` | Shard and partition assignments | Operational |

---

*ARC is open. The specification is free to implement. The network belongs to its participants.*

*— Draft v0.1*
