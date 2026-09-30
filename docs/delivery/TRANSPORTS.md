# How to implement an ARC transport

This contract applies to transport implementations and their prerequisite PRs.
It defines completion for contributors and reviewers.

## 1. Define the supported workflow

A transport moves signed Nostr events between ARC participants, including
consumers, providers, relays and a citizen's devices.
It connects to the existing delivery core and normal ARC commands.
A byte echo, codec, or radio probe proves only its own component.

Before implementation, state:

- The medium and supported operating systems.
- The ARC commands that will use it, on both endpoints.
- Support for stored-event sync, live delivery, and forwarding.
- The configuration, identity selection, and required permissions.
- The acceptance commands and expected results.

Do not invent a parallel command suite for ordinary messages or capability calls.
A separate diagnostic command is permitted. It does not replace CLI integration.

## 2. Use the existing contract

The current Go interfaces are in
[`core/transport/transport.go`](../../core/transport/transport.go).
Use the code contract when the delivery specification describes a future design.

| Interface | Required behavior |
| --- | --- |
| `Transport` | Implement `Name`, `Send`, and `Fetch` for signed events and filters. |
| `Live` | Add `Watch` when the transport claims live delivery. Preserve its readiness and cancellation contract. |
| `Carrier` | Add `SendHops` when the transport carries events with courier hop limits. |
| `Reconciler` | Add efficient set reconciliation only when the transport supports it. Preserve the documented fetch fallback. |

Do not return false success from an unsupported operation to satisfy an interface.
If the medium requires a contract change, specify and test that change with its callers.
Do not create a competing transport abstraction.

Preserve the event's signed fields, identity, and signature during encoding and fragmentation.
Reuse [`compact`](../../core/compact/compact.go) and
[`frame`](../../core/frame/frame.go) when their formats fit the link.
Compact encoding does not encrypt or authenticate an event.

## 3. Integrate both endpoints

Trace one operation from CLI configuration to the receiver's result.
Inspect these integration points as required by the declared workflow:

- [`cmd/arc/main.go`](../../cmd/arc/main.go): session configuration and sync targets.
- [`cmd/arc/capability.go`](../../cmd/arc/capability.go): discovery, calls, and provider watches.
- [`core/node`](../../core/node/node.go): publish, fetch, sync, and verified watches.
- [`core/mail`](../../core/mail/mail.go): queued delivery and acknowledgements.
- [`core/call`](../../core/call/call.go): requests, replies, and duplicate handling.

Live consumers select capability interfaces: `watchAll` uses `transport.Live`
in `cmd/arc/capability.go`, and live calls use `call.Exchanger`. Core sessions
use `transport.Live` through `core/call.OpenSession`; providers and consumers
share `core/session` rather than adding adapter-specific state machines.
An adapter that compiles does not prove those paths can use it.
Change only the necessary callers, and preserve existing relay and directory behavior.

Keep verification in the existing core paths.
Stored events pass through `Store.Save`; live calls use `store.Verify` and private-event validation.
Never dispatch an event directly from a radio callback or compact decoder.
Preserve recipient checks, signature checks, consent, and provider access rules.

Reuse the existing outbox, acknowledgements, and call identifiers.
A retry must not create a new logical request and repeat its side effects.
Transport acceptance does not prove recipient delivery or successful execution.

## 4. Specify failures and security

- Bound connection attempts, queues, buffers, fragments, and incomplete assemblies.
- Honor cancellation. Release subscriptions, connections, timers, and device registrations.
- Define disconnect, reconnect, timeout, expiry, and partial-transfer behavior.
- Return explicit errors when a live destination is unavailable.
- Preserve queued work according to the existing durable-delivery rules.
- Do not silently use an internet relay when the selected transport fails.
- Distinguish event encryption, link encryption, peer identity, and forward secrecy.
- Do not treat a Bluetooth address or successful pairing as an ARC identity.

Use the specified authentication protocol for a secure live link.
Do not invent cryptography inside an adapter.
State what the carrier and intermediate nodes can observe.
An encrypted link does not prove end-to-end forward secrecy across intermediaries.

Forwarding is a separate capability from direct delivery.
A direct transport does not need a mesh to be useful.
If the transport claims a mesh, test its hop limits, duplicate suppression, and forwarding policy.

## 5. Prove the supported behavior

| Scope | Required evidence |
| --- | --- |
| Every usable transport | A real signed event crosses the adapter and reaches the receiving ARC core unchanged. Invalid events fail verification. |
| Normal CLI use | Two separate ARC homes complete the declared user workflow through this transport. An unintended fallback cannot make the test pass. |
| Private delivery | Only the intended recipient opens the content. The carrier does not require plaintext. |
| Stored-event sync | Missing events transfer, filters work, and repeated sync does not duplicate application results. |
| Durable delivery | An interruption leaves work pending. A later transfer and acknowledgement or reply complete it correctly. |
| Live calls | Discovery and a provider request/reply succeed. An unavailable provider fails clearly. Retries preserve request identity. |
| Fragments | Reordered, duplicate, missing, conflicting, expired, and oversized fragments have bounded outcomes. |
| Mesh claims | A reaches C through B while A cannot reach C directly. The receiver processes the event once. |
| Device support | Real devices prove discovery, transfer, cancellation, disconnect, and restart on each claimed platform. |

Apply the rows that match the declared scope. List unsupported capabilities explicitly.
Test the same user workflow as an existing transport when both claim that capability.
Use existing delivery tests and [`internal/proof/delivery_test.go`](../../internal/proof/delivery_test.go) as examples.
Add transport-specific integration proof to CI where the environment supports it.

Run formatting, vet, build, and relevant tests for the changed paths.
Use the race detector for concurrent code and cross-build platform-specific adapters.
A mock test proves the mock scenario. A cross-build proves compilation.
Neither proves interoperability with a real radio or operating-system service.
Record device models, OS versions, commands, and results for hardware tests.

## 6. Report the actual completion level

| Status | Meaning |
| --- | --- |
| Component only | A codec, framing layer, or diagnostic works. Normal ARC commands cannot yet use the new path. |
| Integrated, verification pending | ARC uses the adapter, but required endpoint or hardware evidence is missing. |
| Usable for the declared scope | Normal ARC workflows pass through the actual medium, with the required failure and security checks. |

Small prerequisite PRs are permitted. State their component scope in the title and description.
Link the integration issue or plan, and list the remaining acceptance criteria.
Do not mark the parent transport task complete when only a prerequisite passes.
Do not silently reduce a request for a transport to a request for a probe.

Before approval, the reviewer checks the CLI path, the core verification path,
the applicable evidence, and the stated limits.
Keep the specification, implementation plan, user guide, and PR description consistent.
