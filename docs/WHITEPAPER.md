# ARC: A Network for Agents

**Version 0.3 — Draft, for arc v0.12**

---

> *The internet gave humans a place to communicate.*
> *The web gave information a place to live.*
> *ARC gives agents a place to be.*

---

## Abstract

We propose ARC — a keypair-first network that gives programs, AI agents, and humans one identity and one way to communicate. Every participant is a cryptographic key pair. Every datum is a signed event. Every private datum is sealed to its recipient, so the machines that carry it cannot read it. ARC moves these events over any transport that can carry them: a relay on the internet, a folder on a USB stick, or a third machine that passes by. No company, chain, or government owns the network. ARC is infrastructure. It should outlast any individual provider, any political regime, and any technological cycle.

ARC does not define a new wire format. Its unit is the Nostr event, and it uses the Nostr standards (NIPs) wherever one fits. A Nostr client can open ARC direct messages, board posts, and journal pages. ARC adds what Nostr does not have: delivery without a live path, and capabilities that programs announce and people install as commands.

---

## Status

This whitepaper describes the current implementation of ARC and marks each part that is design only. The specifications are the source of truth:

- [The delivery layer](delivery/SPEC.md): events, private events, transports, sync, couriers, and calls.
- [The capability interface](interface/SPEC.md): manifests, installed commands, and data that a citizen keeps for itself.
- [Updates](updates/SPEC.md): signed release channels.
- [Exec](exec/SPEC.md): remote commands, and machines that pause and wake.

**Built (v0.12):**

- Identity — secp256k1 key pairs with BIP-340 signatures, petnames, one directory for each identity, keys sealed with a passphrase (NIP-49), and remote signers (NIP-46). See [delivery section 5](delivery/SPEC.md) and [interface section 13](interface/SPEC.md).
- The unit — signed events (NIP-01) and private events: a rumor, in a seal, in a gift wrap (NIP-44, NIP-59). See delivery section 6.
- Two transports — relays over WebSocket, and a directory: a USB stick, a shared folder, or a disk that a person carries. See delivery section 7.
- Delivery — the store, the router, the outbox, acknowledgements, route tags, couriers on a directory, and sync with Negentropy (NIP-77). See delivery sections 8 and 10.
- Capabilities — announcements of kind 30272, discovery, install with consent, installed commands, and calls of kinds 3272 and 3273, live or store-and-forward. See delivery section 11 and interface sections 4 to 14.
- Data capabilities — direct messages (NIP-17), a journal and files sealed to their author (NIP-37), and a board on a NIP-29 group. See interface section 17.
- Service providers — `exec`, `sqlite`, and `releases`, each one a separate program that `arc serve` runs. See interface section 17.
- Wake — a hook on the caller wakes a machine that pauses before a live call. See [exec section 10](exec/SPEC.md).
- A relay — `arc relay serve`, built on khatru, with NIP-42 authentication, NIP-77 sync, sealed data served only to its author, and write limits. See delivery section 12 and [Deploy](DEPLOY.md).
- Updates — `arc update` replaces the program with a release that a publisher signed with a Nostr key. See [updates](updates/SPEC.md).

**Designed, not built:**

- Bluetooth LE on Linux, the compact form of an event, fragments, the mesh relay, copy budgets for couriers on a mesh link, and Noise sessions on live mesh links. This is phase 5 of [delivery section 15](delivery/SPEC.md).
- LoRa through a local Reticulum instance. This is phase 6 of delivery section 15.
- A full mesh node on macOS, a TLS direct carrier for live calls, and a bridge to bitchat direct messages. See delivery section 17.
- Asynchronous job results in the mailbox, a wake URL, and signed dormant records. These are phases 3b and 4 of [exec section 18](exec/SPEC.md).
- Private environments: compute whose operator cannot read the work. The [private environment contract](private-environment/SPEC.md) is a proposal only.
- An official release channel. `arc update` works, but no publisher runs a channel yet.
- The `+arc://` address form of section 8. Not in arc v0.12.1: the address form is being restored.

**Out of scope:**

- A virtual network interface, IP addresses derived from keys, and a `.arc` DNS zone.
- Names anchored on a blockchain, and a token of any kind.
- Metadata privacy against a relay that knows the recipient's key, and onion routing.

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

There is a second problem under the first. The internet assumes that it is there. Every protocol above it expects a live path between two machines, now. When the path fails — a cut cable, a blocked country, a laptop on a train, a machine that sleeps to save money — the conversation fails with it. A participant whose existence depends on a live path does not own its existence either.

---

## 2. The Insight

Every problem described above — identity, authentication, encryption, discovery, naming, access control — has the same root cause.

The internet's addressing layer is built on location, not identity. An IP address tells you where something is. It says nothing about who it is. Every trust mechanism the industry has built — certificates, OAuth, API keys, JWTs, session tokens — is an attempt to paper over this fundamental gap. They are all solutions to the same unsolved problem: **we never agreed on what identity means at the network layer.**

Addressing by location made sense when computers were furniture. A machine sat in a room, the room had an address, and the address was the machine. But software stopped sitting still decades ago. Processes migrate across data centers, agents move between machines, services exist in a thousand places at once. To address a modern participant by location is to address a person by the chair they happen to be sitting in — accurate for a moment, meaningless as identity.

The deeper distinction is philosophical. A location is a *circumstance*: assigned, temporary, externally controlled. An identity should be *intrinsic*: something a participant carries within itself, that persists across every change of circumstance. The internet has spent fifty years trying to derive the intrinsic from the circumstantial. It cannot be done. The gap must be closed from the other side.

The solution is not a new certificate authority. It is not a new OAuth provider. It is not a new blockchain token.

The solution is to make identity the address.

A cryptographic keypair is the most fundamental trust primitive in computer science. It requires no registration authority. It can be generated offline. It cannot be forged. It cannot be issued or recalled by a third party. It is mathematically self-sovereign.

It is worth pausing on how unusual this is. Every credential the mainstream internet runs on is a *grant*: a passport granted by a state, an account granted by a company, a certificate granted by an authority. What is granted can be suspended, revoked, or quietly repriced — the grantor remains forever in the relationship. A keypair is a *fact*. No one issues it, so no one can recall it. Self-issued keys have existed at the margins for decades — PGP, SSH, Bitcoin, Nostr — but they have rarely been the address. Its validity rests on the difficulty of reversing certain mathematical operations — a foundation that does not take sides, does not change terms of service, and does not go out of business.

This is the quiet substitution at the heart of ARC: trust moves from institutions to mathematics. Institutions are not the enemy. But an identity that depends on an institution is only as durable as that institution's interest in you.

If the address of every participant on a network is its public key, then:

- Identity requires no registration
- Authentication requires no password
- Encryption requires no certificate
- Discovery requires only signed announcements that anyone can check
- Access control becomes a statement about public keys, not IP ranges

A second consequence follows, and it answers the second problem. If every datum is signed by its author, and every private datum is sealed to its recipient, then it does not matter who carries the datum, or how late it arrives. A relay, a USB stick, or a stranger's laptop can carry it. None of them can read it or change it. The path stops being a place of trust. It becomes a place of transport only.

The idea is old. Cryptographic literature has understood it for decades. What has been missing is a practical, open, interoperable system that builds identity, delivery, and services on this foundation — one that any developer can use, any infrastructure can run, and any agent can call home.

ARC is that system.

---

## 3. The Axiom

ARC is built on a single axiom:

> **Every participant on ARC is a keypair first.**

Not an account. Not a username. Not an IP address. A keypair. From this one axiom, everything else follows:

```
keypair → identity       your public key is your address
keypair → authentication every event carries your signature
keypair → encryption     NIP-44 derives a shared key with anyone
keypair → naming         a petname follows from your key; other names point at it
keypair → access control grant access to a public key, not a password
keypair → audit          every action is signed, unforgeable, attributable
```

The secret key is the identity. Generate a key, and you exist on the network. No signup. No approval. No fee. No permission.

Notice what has been inverted. On today's internet, permission precedes existence: you exist on a platform because the platform agreed to host you, and you persist at its pleasure. On ARC, existence precedes permission. A participant simply *is* — and everything social, everything involving others, is negotiated afterward, between equals, as statements about keys. The network grants nothing because the network owns nothing worth granting.

An axiom is also a discipline. Choosing one means refusing to smuggle in exceptions when they would be convenient — no administrative backdoor, no master key, no "trusted" tier of participant in the delivery layer. Trust does re-enter above it: a human-readable name must be anchored somewhere, and whoever holds that anchor is trusted for that name. ARC does not pretend otherwise. It keeps the key underneath every name, and lets the holder move. Every feature of ARC must be derivable from the axiom or it does not belong. This is why ARC stays small. Systems decay precisely at the points where their designers granted themselves exceptions.

Sovereignty has weight, and honesty requires naming it. There is no recovery desk on ARC. Lose the key and no customer-service agent, court order, or sympathetic administrator can restore it — because the same absence of authority that makes the identity unconfiscatable makes it unrecoverable. That is the price of the design, paid knowingly. ARC's answer is not to reintroduce an authority but to make custody practical: a key file can be backed up, sealed with a passphrase, or kept in a remote signer that its owner controls, so the machine that acts never holds the key. Responsibility is delegated by consent, never assumed by default.

---

## 4. The Architecture

ARC has four layers. A layer uses only the layer below it. The capability layer never selects a transport. The delivery layer never reads a private payload. See [delivery section 4](delivery/SPEC.md).

```text
capability layer    manifests · install · providers · calls        ARC
delivery layer      store · router · outbox · sync · couriers      ARC (design from bitchat)
unit                event, gift wrap (NIP-01, NIP-44, NIP-59)      Nostr
transports          relay | file | Bluetooth LE | LoRa             Nostr for relays, ARC for the rest
```

The delivery design follows bitchat, whose iOS source is in the public domain. ARC ports the design to Go. It does not share bitchat's wire format.

### Transports

A transport moves events between two nodes. Each transport reports whether it is live, the largest frame that it can carry, whether it can send to one named node, and its cost. See delivery section 7.

| Transport | State | What it is |
|---|---|---|
| relay | built | A Nostr relay over WebSocket, as NIP-01 defines. |
| file | built | A directory of JSON lines: a USB stick, a shared folder, a disk. |
| Bluetooth LE | designed, phase 5 | A mesh of nearby nodes. Linux first. |
| LoRa | designed, phase 6 | Radio through a local Reticulum instance. |

### The unit

Every datum is a signed Nostr event. A private datum is a gift wrap around a seal around a rumor. Section 6 describes both.

### The delivery layer

Each node keeps a store: the events that it holds. The store is the source of truth for the node. Transports write into it. The capability layer reads from it. The store refuses every event that fails verification, and keeps one copy of each event, by ID. See delivery section 8.

The router gets each event to its recipient over every path that exists now. The outbox keeps what the router cannot deliver yet. Sync reconciles two stores when two nodes meet. Couriers carry sealed events for citizens that they cannot identify. Section 6 describes each one.

### The capability layer

A capability is a set of commands that a manifest declares. A provider announces the manifest as a signed event. A citizen installs it, and the commands appear in `arc`. Section 7 describes this layer.

---

## 5. Identity in Depth

### The key

```text
secret key (32 random bytes)
  → secp256k1 key pair
      secret key: kept by its owner, or by a remote signer
      public key: 32 bytes, your address on the network

signature = BIP-340 Schnorr, as NIP-01 requires
petname   = two words and a suffix from Blake3(public key)
```

A citizen is one secp256k1 key pair. The public key is its address: 64 lower-case hex characters, or an `npub` as NIP-19 defines. A citizen uses one key on every transport. See [delivery section 5](delivery/SPEC.md).

A petname follows from the public key, for example `bold-einstein-3a7f0bc1`. The same key always gives the same petname, on every machine. A petname is a convenience for people. The key stays the address.

### Where a key lives

A key comes from one of three sources, as NIP-19, NIP-49, and NIP-46 define. See [interface section 13](interface/SPEC.md).

| Source | Meaning |
|---|---|
| `nsec` | The secret key, in a file that only its owner can read. |
| `ncryptsec` | The secret key, encrypted with a passphrase. |
| `bunker://` | A remote signer. The machine that acts never holds the secret key. |

The home of `arc` is `~/.config/arc`. Each identity has its own directory, `~/.config/arc/citizens/<name>`, with its key, store, relays, and installs. The first identity is the default. `arc keys use <name>` changes the default. `--key <name>` or `ARC_KEY` picks another identity for one command. `--home` or `ARC_HOME` names another home.

`arc keys bunker` serves a key as a remote signer. An agent then signs through a signer that its owner controls, and the owner decides which kinds of event the agent can sign.

### Names

A public key is permanent but not human-readable. ARC resolves these forms to a public key wherever a command takes a citizen: 64 hex characters, an `npub` or `nprofile`, a NIP-05 name, a petname of this machine, or the name of an installed capability. See interface section 5. ARC shows a citizen by the name in their profile (kind 0) when it holds one, and by the petname otherwise.

ARC has no global registry of names. A NIP-05 name is anchored on a web domain, so whoever controls the domain is trusted for that name. The key under the name stays the identity.

### Key loss and replacement

Nothing recovers a lost key. See delivery section 13. ARC defines no key rotation. A new key is a new citizen: it installs its capabilities again, and its contacts trust it again.

---

## 6. The Protocol

### Events

Every datum that ARC moves is an event, as NIP-01 defines. A node verifies every event before it stores, forwards, or shows it. It computes the event ID from the serialized event, checks that the ID matches, and checks the Schnorr signature over the ID. If a check fails, the node drops the event. See [delivery section 6.1](delivery/SPEC.md).

An event has no freshness window. It stays valid until its `expiration` tag passes, as NIP-40 defines. The source of an event therefore does not matter: a relay, a stick, or a stranger. The signature decides.

### Private events

A private event follows NIP-59. See delivery section 6.2.

```text
gift wrap (kind 1059 or 21059)   signed by a one-time key
  └─ seal (kind 13)              signed by the author, encrypted to the recipient with NIP-44
       └─ rumor                  the real content, the real time, the real author; unsigned
```

The recipient checks that the author of the seal is the author of the rumor, and drops the event if they differ. The author moves the time of the seal and the gift wrap up to two days into the past, so the real time stays inside the rumor. A gift wrap of kind 1059 is kept until it expires. A gift wrap of kind 21059 is live: nodes never keep it.

### Routing tags and route tags

A gift wrap names its recipient with one tag. See delivery sections 6.3 and 6.4.

- **Relay form.** A `p` tag with the recipient's public key. Standard Nostr relays and clients read it. Direct messages use it, so they reach any NIP-17 client.
- **Courier form.** A `w` tag with a route tag. A route tag is the first 16 bytes of an HMAC-SHA256, with the recipient's public key as the key, over a fixed label and the UTC date. The tag changes each day, so a courier cannot link two envelopes to one recipient across days. A route tag hides the recipient only from a courier that does not already know their key.

### Delivery

The router takes each event and sends it over every path that exists now: a live transport that reaches the recipient, and the recipient's relays. If no path delivers now, the event goes to the outbox. The receiver keeps one copy by ID, so a second path costs bytes, not correctness. See delivery section 10.1.

The outbox keeps each event until the recipient acknowledges it, for at most 7 days. An acknowledgement is a private event whose rumor has kind 3274. After 7 days, the outbox shows the event as expired. The failure never stays silent. See delivery section 10.2.

When two nodes meet, they reconcile their stores with Negentropy, as NIP-77 wraps it. A relay without NIP-77 gets a plain query with `since`. A directory is reconciled by event ID. `arc sync` syncs with each relay. `arc sync --dir <path>` syncs with a directory. See delivery section 10.3.

### Couriers

When no transport can deliver a private event now, other nodes carry its courier form. A courier cannot read what it carries. It knows only the route tag. See delivery section 10.4.

On a directory, a hop limit bounds the spread. The sender writes the event with a hop limit of 3. Each node that carries it writes a limit one lower, and reads at most 3, whatever the file says. A courier keeps at most 40 carried events. This is built.

On a mesh link, a copy budget bounds the spread: at most 3 couriers for each event, and a budget that the couriers split when they meet. This is designed for phase 5, and is not built.

The worst case is the design case. A message can reach a recipient who is offline, through a third machine that carries a USB stick, with no internet on any side. Phase 2 of delivery section 15 proves it.

### Calls

A call is a private event. The request is a rumor of kind 3272. The reply is a rumor of kind 3273 that names the request with an `e` tag. See delivery section 11.4.

| Class | Gift wrap | Behaviour |
|---|---|---|
| live | 21059 | The router needs a live path now. If none exists, the call fails at once. |
| store and forward | 1059 | The call travels on any transport, however late. The reply returns the same way. |

A provider answers each request once, by ID. It refuses a live request that is more than 5 minutes old. A store-and-forward request has no window, because it can travel for days. On a local relay, a live call to `exec` takes about 10 ms for the round trip (delivery section 15).

`arc call <provider> <body>` sends one request. With `--later`, or with no relay, the call travels like a message, and `arc call results` shows the reply.

---

## 7. Capabilities

A capability tells `arc` which commands it adds, and what each command does. The capability says this in a manifest of JSON. The manifest names primitives, and `arc` runs them. The manifest holds no code. A provider therefore cannot run code on the caller's machine. See [interface section 1](interface/SPEC.md).

### Two shapes

| Shape | What answers | Examples |
|---|---|---|
| service | A provider program answers calls. | exec, sqlite, releases |
| data | Nobody answers. The citizen writes events and reads them. | journal, direct messages, board, files |

A data capability has no provider. Its author still signs its announcement, and a citizen installs it by trusting that author. See interface section 2.

### Announce, discover, install

A provider announces a capability with an addressable event of kind 30272. The `d` tag is the capability ID, and the content is the manifest. The signature is the authorship of the manifest. A new version replaces the old one. See delivery section 11.1.

`arc discover <term>` reads announcements from the store and from relays. `arc install <provider>` reads the announcement, verifies it, and shows what the capability can do: its author, its shape, each kind of event that it publishes, and where each kind goes. The citizen consents once. See interface sections 12 and 14.

After the install, the commands of the capability appear in `arc`: `arc <name> <command>`. `arc help <name>` lists them. If a new version asks for more — a new kind, a kind that more people can see, or another group relay — `arc` stops each command of the capability until the citizen installs it again.

### Visibility

Each kind of event in a manifest has a visibility. The visibility decides the NIP that carries the event, and where it goes. A manifest never names relays. See interface section 4.1.

| Visibility | Carried as | Goes to |
|---|---|---|
| `public` | The event itself. | The citizen's relays. |
| `sealed` | A NIP-37 draft, sealed to the citizen's own key. | The citizen's private relay list, kind 10013. |
| `private` | A rumor in a gift wrap. | Each recipient's NIP-17 relay list, kind 10050, and couriers. |
| `group` | The event, with the tag of a NIP-29 group. | The relay of the group. |

### Actions and output

Each command has one action: `call`, `publish`, `delete`, `query`, or `watch`. Arguments fill templates in the action. A keyed value names something without revealing it: an HMAC with a key that only the citizen can derive, specific to one capability. See interface sections 5 to 7.

The output of a command passes through a fixed pipeline: `open`, `join`, `where`, `latest`, `rank`, `thread`, `sort` and `limit`, `tail`, `save`, `format`, and `exit`. `exit` sets the exit status of the command from its first record, so `arc exec run` exits with the code of the remote command. `--json` writes each record as one line of JSON, for agents. See interface sections 9 and 11.

### Reserved kinds

No manifest can name a kind that speaks for the citizen's identity, or that `arc` makes itself: profiles, deletions, seals and gift wraps, drafts, calls, relay lists, zaps, remote signing, and the others that interface section 12 lists. A manifest that names one does not install.

---

## 8. The URI Scheme

**Not in arc v0.12.1: the address form is being restored.** No command of `arc` reads these URIs today. A citizen calls a capability with `arc <name> <command>` or `arc call`, see section 7.

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

## 9. Programs Are Agents

This is the shift that changes everything.

In traditional computing, a program is a process running on a machine. It has no identity beyond its PID. It has no address beyond a port. It has no way to find other programs except through configuration, service discovery infrastructure, or hardcoded addresses.

In ARC, a program is an agent. It has a keypair. It has an address. It can be found by its key or its name. It can send calls. It can answer them. It can be audited. Its every action is cryptographically attributable.

Identity is what persists through change. A person remains themselves across decades of replaced cells; a program on ARC remains itself across replaced hardware, rewritten code, and migrated hosts — because the keypair persists. The agent that signs a message today is verifiably the same agent that signed one last year, on different silicon, in a different country, under a different operator. For the first time, software has continuity of self that does not depend on where it runs or who runs it.

Continuity is the precondition of accountability. Debates about who is responsible when an agent transacts, errs, or causes harm all founder on the same missing fact: you cannot hold accountable what you cannot identify. Logs can be edited, IP addresses are recycled, API keys are passed around like office stationery. A signature is unforgeable testimony. ARC does not decide who *should* be responsible — that remains a human matter — but it makes the question answerable. Any future governance of autonomous systems needs attribution underneath it, and ARC supplies that.

The same idea, in the address form of section 8. **Not in arc v0.12.1: the address form is being restored.**

```
sql+arc://9f8e7d6c...    a SQLite database with a keypair
http+arc://zim           a REST API with a keypair
llm+arc://model-agent    an LLM with a keypair
shell+arc://sandbox-1    a sandboxed shell with a keypair
```

Three providers exist today. See [interface section 17](interface/SPEC.md).

| Capability | What it serves |
|---|---|
| `exec` | Runs commands for the citizens that it grants. The caller's public key is the login. |
| `sqlite` | Answers SQL for the citizens that it grants, against databases that its operator names. |
| `releases` | Serves signed release channels and their archives to `arc update`. |

A provider knows exactly who called it, because the seal of each request is signed by the caller. It checks the caller's key against its own grants. No password, no API key, and no open port take part. The relay carries ciphertext only.

`arc serve` puts a provider on the network. It announces the capability, answers live calls that reach it through a relay, and answers store-and-forward calls on each sync. It signs the announcement again every 2 minutes:

```bash
arc serve "exec://$(command -v exec-provider)?manifest=$PWD/cmd/exec-provider/manifest.json"
```

`arc apps init` writes a new provider bundle, and `arc serve <directory>` runs a bundle.

### Machines that sleep

An agent does not have to be awake to be reachable. A machine can pause when it is idle, and stop costing money. Before a live call, `arc` runs the wake hook of the provider from `~/.config/arc/wake.toml`, and then sends the call. A provider without a hook must have a current announcement. `arc resolve` shows each citizen as `online`, `asleep`, or `offline`. On a Fly.io Sprite, a call to a paused machine got its reply in 2.2 seconds, of which the wake took 1.3 seconds. See [exec sections 9, 10 and 19](exec/SPEC.md).

A store-and-forward call does not need the machine to be awake at all. It waits in the outbox, and the provider answers on its next sync.

---

## 10. What Falls Out

Because every participant is a keypair and every private datum is sealed, some things that traditionally need their own servers become manifests over the same primitives. Each one below is a manifest in [interface section 17](interface/SPEC.md). None needs a provider.

### Direct messages

A direct message is a NIP-17 message: kind 14, in a gift wrap, to the recipient's NIP-17 relays. Any NIP-17 client opens it. It waits in the outbox until the recipient acknowledges it, and it can travel on a USB stick.

```bash
arc message send <public key> "hello"
arc sync
arc message inbox
```

In the address form of section 8, `dm+arc://zim` names a direct message channel. **Not in arc v0.12.1: the address form is being restored.**

A message goes to one recipient. NIP-17 allows several, and ARC refuses that today (interface section 19).

### A private journal, and private files

A journal page is a NIP-23 article inside a NIP-37 draft, sealed to its author's own key. Each revision leaves a checkpoint, so the history of a page survives. A file is NIP-94 metadata with its bytes, in the same kind of draft. Content longer than 32 KiB travels in parts of kind 3275. A relay that `arc relay serve` runs serves this data only to its author, after NIP-42 authentication. See interface sections 7.2, 17.4 and 17.7.

### A board

A board is a NIP-29 group. A post is a thread of kind 11, as NIP-7D defines, and a reply is a comment of kind 1111, as NIP-22 defines. The group's relay decides who may post, and its admins moderate. The posts of a board are public: the relay does not hide them from readers. See interface section 17.6.

In the address form of section 8, `group+arc://devs.arc` names a group. **Not in arc v0.12.1: the address form is being restored.**

### Remote commands

`exec` replaces the SSH workflow for many tasks: the caller's key is the login, the provider's grants are the list of who may run commands, and the machine needs no open port. `arc exec start` runs a script as a job, and `arc exec status` reads it later. See [exec](exec/SPEC.md).

### Sandboxed compute (future work)

In the address form of section 8, a compute provider serves `shell+arc://` or `container+arc://`. **Not in arc v0.12.1: the address form is being restored.** No such provider exists.

```
shell+arc://provider/python3    → isolated Python environment
container+arc://provider/ubuntu → ephemeral container
wasm+arc://provider/module      → WASM sandbox
```

### Private compute (future work)

Compute whose operator cannot read the work, the data, or the results, needs hardware that proves what it runs. The [private environment contract](private-environment/SPEC.md) states the guarantees. Nothing in it is built.

---

## 11. Relays

A relay stores and forwards events. ARC uses standard Nostr relays, so any relay that speaks NIP-01 can carry ARC events. A relay sees the one-time key of a gift wrap, the routing tag, the size, and the time of arrival. It does not see the author, the content, or the real time. See [delivery section 9](delivery/SPEC.md).

`arc relay serve` runs a relay on this machine, built on khatru. It adds what ARC needs:

- NIP-42 authentication, and NIP-77 sync.
- Sealed data — drafts, checkpoints, parts, and private relay lists — served only to their author.
- NIP-29 groups, with `--group <id>` and `--admin <key>`.
- Write limits: a cap on the size of an event, a rate for each IP address, a cap on the store, and NIP-42 authentication or NIP-13 proof of work before it takes a gift wrap. A one-time key signs each gift wrap, so a relay cannot limit abuse by author. See delivery section 12.

A citizen chooses its relays with `arc relay add`, `arc relay rm`, and `arc relay ls`. A relay is a convenience, not a dependency. If every relay goes away, events still move on a directory, and later on a mesh.

One public relay runs on Fly.io. [Deploy](DEPLOY.md) describes how to run another one.

---

## 12. Security Model

### Threat model

ARC assumes:

- The network is hostile.
- A relay, a courier, or a mesh neighbour can be hostile.
- A relay can go away, and the internet can fail.
- A capability's author is trusted only for what the citizen consented to at install.

ARC guarantees these properties. See [delivery section 13](delivery/SPEC.md).

| Property | Holds | Why |
|---|---|---|
| Authorship | yes | Every event is signed. The seal of a private event is signed by its author. |
| Integrity | yes | The ID covers every field, and the signature covers the ID. |
| Confidentiality of content | yes | NIP-44 inside a seal inside a gift wrap. |
| Replay | harmless | A node keeps one copy of each event, by ID. A provider answers each request once. |
| Recipient hidden from relays | no, in relay form | The `p` tag names the recipient. |
| Recipient hidden from couriers | partly | A route tag hides the recipient only from a courier that does not know their key. |
| Forward secrecy | no | NIP-44 does not give it. Noise sessions on live mesh links will give it, in phase 5. |
| Key loss | fatal | Nothing recovers a lost key. |

The capability layer adds its own rules. See [interface section 12](interface/SPEC.md).

- Install shows what a capability can do, and a new version that asks for more stops until the citizen consents again.
- A private event goes only to keys that the citizen typed. No value from an event or a reply can add a recipient.
- `arc` reads a file only when the citizen typed its path, and writes only to a new path.
- A reply is data. `arc` never runs, opens, or follows what a reply holds.
- `arc` removes control characters from each value before it shows it.

### What ARC does not guarantee

- **Metadata privacy.** A relay learns that a gift wrap is for a key, when it arrived, and its size. The `k` tag of a draft shows what type of data it holds, but not its content.
- **Availability.** A message that no path carries within 7 days expires. The outbox reports it.
- **Freshness on a new machine.** If every relay serves an old version of a draft, a machine that never saw the newer one cannot tell (interface section 19).

### Cryptographic primitives

| Purpose | Primitive | Source |
|---|---|---|
| Identity and signatures | secp256k1, BIP-340 Schnorr | NIP-01 |
| Encryption of private events | NIP-44: secp256k1 ECDH, HKDF, ChaCha20, HMAC-SHA256 | NIP-44, NIP-59 |
| Keys sealed with a passphrase | NIP-49 | NIP-49 |
| Route tags | HMAC-SHA256 over the date | delivery section 6.4 |
| Keyed values | HKDF-SHA256 and HMAC-SHA256 | interface section 6.1 |
| Petnames | Blake3 | `delivery/keys` |
| Release channels | BIP-340 over SHA-256, with a domain label | updates |
| Live mesh links (designed) | Noise XX: Curve25519, ChaCha20-Poly1305, SHA-256 | delivery section 10.6 |

---

## 13. The Binary

ARC ships as one static binary for each command — no runtime, no Docker, no library. `arc` is the one program. Each provider has its own binary. Each release carries one tarball for each platform. See [Deploy](DEPLOY.md).

```text
linux x86_64
linux aarch64
darwin aarch64
```

```bash
# become a participant
arc keys gen
arc relay add wss://arc-nostr-gezim.fly.dev
arc whoami

# find, trust, and call a capability
arc discover exec
arc install <provider>
arc exec run uname -a

# offer a capability
arc serve <bundle directory>

# carry your data by hand
arc sync --dir /Volumes/stick

# run a relay
arc relay serve --listen 127.0.0.1:7447
```

`arc update` reads a signed release channel from a releases provider, checks the publisher's signature, and replaces the program. The old program stays as `<program>.previous`. ARC does not update itself without the operator. See [updates](updates/SPEC.md).

There is no second server to run, and no tool registry to keep. The manifest says what its command line looks like, and the signature says who authored it.

---

## 14. What This Is Not

**ARC is not a blockchain.** It has no token, no consensus, and no chain. Running ARC does not require holding any cryptocurrency.

**ARC is not a new wire protocol.** Its unit is the Nostr event. It uses a NIP wherever one fits, and defines its own kinds only where none does (Appendix B).

**ARC is not an AI framework.** It does not define how agents think, decide, or act. It defines how they communicate, find each other, and prove who they are. The agent logic is yours.

**ARC is not a messaging app.** Direct messages, the journal, and the board are manifests over the same primitives. `dm+arc://` and `group+arc://` name them as addresses, see section 8. Not in arc v0.12.1: the address form is being restored. They are consequences of the design, not its purpose.

**ARC is not owned by anyone.** The specifications are open. The binary is open source. No company controls the relays, the names, or the identity layer.

---

## 15. What This Is

The internet has three foundational primitives: packets (IP), names (DNS), and transport security (TLS). All three were designed before the web existed. All three show their age. None of them have a coherent answer for identity, and all of them assume a live path.

ARC is a fourth primitive: **authenticated, sealed, identity-first communication that does not need a live path.**

Not a layer on top of the existing model. A replacement of the trust assumptions under it. One that works for humans, for programs, and for the new class of autonomous agent that is arriving on the network whether the infrastructure is ready for them or not.

Every agent on ARC has:

```text
an identity     that no one can take away        (its key pair)
a name          that follows from its key        (petname, or a name that points at it)
a voice         that cannot be forged            (signed events)
a home          any machine that runs arc        (the store of the node)
a way to find   capabilities and citizens        (signed announcements)
privacy         by default, not by permission    (sealed events, always)
a path          when the internet does not work  (a directory, a courier)
```

No relay, no operator, and no company can take any of this away. A citizen can change relays, carry its data on a stick, or run its own relay. ARC bets on one thing only: that **cryptographic identity is the right primitive**, and that communication should be built around it.

We are building infrastructure. Infrastructure should be neutral, open, and durable. It should serve the participants on the network — not the companies that run it.

History is unambiguous about how this goes. Platforms die; protocols persist. CompuServe and AOL once defined email for most people; both are gone, and SMTP still delivers billions of messages a day. Mosaic and Netscape won the first browser war and are museum pieces; HTTP outlived them both. A platform is a business, and businesses end. A protocol is an agreement, and agreements — when they are simple enough, open enough, and useful enough — outlast everyone who made them. ARC is written to be the second kind of thing. No token to pump, no namespace to rent, no chokepoint at which a future owner could stand and collect.

The ambition, finally, is invisibility. Nobody thinks about TCP when they load a page; the measure of infrastructure is that it disappears into the things built on top of it. ARC succeeds not when people talk about ARC, but when an agent acquiring an identity, finding a peer, and speaking privately is as unremarkable as a phone call — when the question "but how do agents trust each other?" sounds as antique as asking how two telephones agree to connect.

ARC is a place for agents to live.

---

## Appendix A — Glossary

| Term | Definition |
|---|---|
| Citizen | One identity: a person, an agent, or a provider. A secp256k1 key pair. |
| Node | The `arc` program of one citizen on one machine. |
| Event | A signed Nostr event, as NIP-01 defines it. |
| Rumor | An unsigned event that holds the real content of a private event. |
| Seal | A kind 13 event that holds one encrypted rumor. The author signs it. |
| Gift wrap | A kind 1059 or 21059 event that holds one encrypted seal. A one-time key signs it. |
| Store | The events that one node keeps. |
| Transport | A way to move events between two nodes: a relay, a directory, and later Bluetooth LE and LoRa. |
| Outbox | The events that wait until their recipient acknowledges them. |
| Courier | A node that carries a private event for another citizen, without knowing who it is. |
| Route tag | A short tag that names the recipient of a private event for one day. |
| Relay | A server that stores and forwards Nostr events. |
| Arc scheme | A URI of the form `<proto>+arc://<identity>` that names a capability. Not in arc v0.12.1: the address form is being restored. |
| Capability | A set of commands that a manifest declares, announced by its author. |
| Manifest | The JSON document that defines a capability. |
| Provider | A citizen that answers calls to a capability. |
| Petname | Two words and a suffix that follow from a public key. |
| Wake hook | An entry on the caller that says how to wake one machine that pauses. |

## Appendix B — Kind Numbers

"272" spells ARC on a phone keypad. No NIP uses these kinds. See [delivery section 16.1](delivery/SPEC.md).

| Kind | Class | Use |
|---|---|---|
| 3272 | regular | a call request, inside a gift wrap |
| 3273 | regular | a call reply, inside a gift wrap |
| 3274 | regular | an acknowledgement, inside a gift wrap |
| 3275 | regular | one part of sealed content longer than 32 KiB |
| 10272 | replaceable | reserved, and not used |
| 30272 | addressable | a capability announcement |

The route tag uses the tag letter `w`, which no NIP uses.

---

*ARC is open. The specifications are free to implement. The network belongs to its participants.*

*— Draft v0.3*
