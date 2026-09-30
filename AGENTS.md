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
