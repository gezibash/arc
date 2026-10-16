# ARC repository instructions

## Package boundaries

Read [the architecture map](docs/ARCHITECTURE.md) before changing a package boundary.

- `core/` owns ARC event rules, identity and cryptography, delivery state,
  request/reply behavior, the provider protocol, and the interfaces they consume.
- Core production code imports no ARC package outside `core/`. It does not open
  files, connect to networks, spawn processes, or depend on a database driver.
  Inject readers, writers, signing contracts, stores and transport interfaces.
- `sdk/` is the kit for app authors: the provider runtime, session types,
  standard streams, configuration helpers, strict JSON, and the HTTP and
  process I/O mappings. It imports only `core/` and `sdk/`. A change to `sdk/`
  is a change for each app in another repository.
- `adapters/` implements concrete I/O for core and the runtime. Relay,
  directory, subprocess hosting, key-file and Bolt implementations belong
  here. An adapter may depend on core and the SDK; it must not depend on
  `runtime/`, `apps/`, or `cmd/`.
- `runtime/` owns citizen workflows, consent, routing choices, manifests,
  installed commands, bundle management, wake policy and updates. Concrete
  adapters are selected by composition code, such as `runtime/citizen.Open`.
- `apps/<name>/` owns one complete app: manifest, Arcfile, service
  implementation in `server/`, its program in `cmd/arc-<name>`, and its
  documentation. Manifest-driven data apps need no program.
- An app imports only `sdk/` and its own packages. It must build in another
  repository. If an app needs a helper, add the helper to `sdk/`. Do not
  import `core/`, `adapters/`, `runtime/`, `internal/` or another app.
- `cmd/arc` is the arc program. It imports no app.
- The runtime and adapters must not import concrete `apps/` packages.
  Apps use the SDK to compose with other apps.
- Define an interface at the layer that consumes its effect. Only interfaces
  consumed by ARC protocol rules belong in core. Application effects, such as
  full-text search, keep their interfaces in runtime code. Shared request
  and result types can live in `internal/` so adapters need no runtime import.
- Add interfaces for real effects; do not add interfaces for pure functions or
  duplicate an existing contract. Reuse alone does not make a feature core ARC.
- HTTP over ARC is an application-protocol adapter. A future HTTP event carrier
  is a delivery adapter. Both stay outside core.
- Shared interaction modes and session behavior live in `core/session`, with
  authenticated event delivery in `core/call` and provider dispatch in
  `sdk/provider`, which gives apps the session types under its own name. Reuse this machinery for consumers and providers alike.
  Adapters must not implement a second session state machine.
- Read [the session contract](docs/sessions/SPEC.md) before changing interactions.
  HTTP, SSE and WebSocket mappings belong in `sdk/httpadapter`. Do not require every
  delivery adapter to support live calls or streams. Keep REPL variables, SQL
  transaction state and other domain state inside their apps.
- Session identity is distinct from a transport connection. Preserve explicit
  disconnect errors, bounded flow control, half-close and final outcomes.
  Never add implicit replay or resumption after an uncertain result.
- Database adapters preserve atomic transactions and existing on-disk layouts.
  Keep signature verification and mail recovery decisions in core.
- Run `mise run boundaries` after changing package dependencies. It also runs
  in `go test ./...` and CI. Tests may import concrete adapters to exercise the
  real integration paths; production core must not do so.

## Citizens, the relay and roles

ARC has only citizens and the relay. Model each feature on that spine.

- A citizen is an identity with one or more devices. The citizen key lives
  on a signer and on a 24-word seed. Each device has its own key.
- The relay only coordinates. It keeps mail until it expires, carries live
  calls, announcements and rendezvous, and answers STUN. It carries bulk
  bytes through TURN only as the last resort.
- The relay holds no cloud credentials. The one exception is the ring key
  that a citizen registers for its own rings
  ([relay rings](docs/rings/SPEC.md)). A ring carries no mail, no author
  and no content.
- The relay does not keep self data or bulk bytes. Bulk bytes go over the
  direct pipe of `sdk/direct`.
- Storage and compute are roles that a citizen serves. Do not add a party
  that is not a citizen or the relay.
- A storage citizen is a citizen that runs the storage app. It holds
  encrypted bytes for other citizens and cannot read them. It knows an
  owner only by an account key for that one storage citizen.
- A citizen can use more than one storage citizen, so that one storage
  citizen cannot lose its data. A storage citizen can be a paid service
  later.
- Self data, such as the journal and files, stays on the device. Sync sends
  encrypted blobs to storage citizens, never to the relay.

## Specs

Read [how to write a spec](docs/SPEC-TEMPLATE.md) before you write or change
a spec, and before you change a package under `core/` or `sdk/`.

- Each package under `core/` and `sdk/` belongs to a spec, through the `Owns`
  item of its header.
- A change to a normative section of a `built` spec is a protocol change.
  State how an older client behaves, and ship a breaking commit.
- A new event kind needs a row in [the kind registry](docs/KINDS.md), a reason
  for its class, and a spec section.
- [The grandfathered list](docs/GRANDFATHERED.md) holds the packages that no
  spec owns. It is debt. Never add an entry.
- A proposal lives under `docs/proposals/`, and says `Status: proposed`. A
  document that says `built` names the test that proves it.
- An app that uses only `sdk/` needs no spec here. It documents itself in its
  own README.
- Run `mise run specs` after you change a spec, a kind, or a package under
  `core/` or `sdk/`.

## Transport work

Before you add or change a transport, read
[the transport implementation contract](docs/delivery/TRANSPORTS.md).
Follow it for implementation, tests, documentation, and PR review.

- A transport moves signed ARC events through the existing delivery core.
- A codec, radio probe, echo service, or successful build is not a usable transport.
- Complete the requested ARC workflow through the actual transport and normal CLI.
- Check the callers as well as the adapter. Remove concrete relay assumptions
  only where the new transport needs integration.
- Reuse identity, verification, encryption, mail, and call behavior from core.
  Do not create a second implementation inside the adapter.
- Declare support for sync, live calls, and forwarding separately.
  Do not require a directory transport to support live calls.
- Keep a prerequisite PR explicitly partial. Link its integration work and name
  the remaining acceptance criteria. Do not present it as transport completion.
- Distinguish automated checks from tests on real devices.
  Report missing hardware proof before you claim the transport works.

## Technical language

Use short sentences and consistent terms in specifications and status reports.
Use ASD-STE100 writing rules where practical. Do not claim dictionary certification.
State what works, the evidence, and what remains unverified.
