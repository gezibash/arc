# Remote traversal verification lab

## Status

The controlled EC2 run on 2026-09-16 completed 28 network trials, 84 SQLite
requests, and separate relay-outage and interrupted-write experiments. The
measured conditions passed. See [the results](VERIFICATION-2026-09-16.md) for
packet evidence, cleanup verification and limits.

The scripts are in [`scripts/traversal_lab`](../../scripts/traversal_lab/README.md).
This plan also includes broader scenarios that remain future work. A successful
controlled lab does not establish universal consumer-router compatibility.

## Topology

Use three temporary EC2 instances in a dedicated test network:

- A publicly reachable ARC relay, restricted to the test endpoints.
- A citizen host, running ARC in a private Linux network namespace behind a
  configurable Linux router on that host.
- A provider host with its own equivalent private namespace and router.

The routers apply address translation and filtering. The citizen and provider
have no direct route between their private namespaces. Each connects outward to
the relay. They advertise only the address observed by that relay, and exchange
direct candidates through their authenticated ARC conversation.

```text
Citizen -- Router A --+-- ARC relay --+-- Router B -- Provider
                     |               |
                     +-- direct -----+
                         if admitted
```

Use new test identities and synthetic data. Deploy the same source snapshot on
every machine, including the current uncommitted traversal changes. Record the
revision and snapshot digest; building only the current commit would omit them.

The outer cloud firewall isolates the experiment from unrelated machines. The
Linux routers supply the changing test conditions. This separates cloud access
control from the address-translation behavior under test. Start with one relay;
federated relay coordination is a subsequent, distinct experiment.

## Evidence before application tests

- Record the operating system, the kernel, and the Go toolchain version.
- Verify ordinary outbound connections to the relay work.
- Verify an unsolicited inbound connection fails before coordination. A
  permanently forwarded port would invalidate a hole-punching claim.
- Compare each relay-observed endpoint against router connection state. Confirm
  the chosen translation profile actually preserves or changes ports as intended.
- Capture packet headers on both sides of each router, and retain filter counters
  and ARC route status. Never collect identity secrets or decrypted user traffic.
- Confirm a promoted route selects `punch` and authenticates both identities.
  A successful request alone is insufficient: it may have used the relay.

## Scenarios

| Conditions | Required evidence |
| --- | --- |
| Direct permission absent | Requests succeed through the relay; no punched connection is admitted. |
| Only provider can receive ordinary inbound connections | Approved provider listener promotes and returns the correct result. |
| Only citizen can receive ordinary inbound connections | Reverse dialing promotes without changing provider permissions. |
| Both routers reject unsolicited inbound traffic; outgoing peer traffic is permitted and mappings are stable | Measure whether coordinated TCP opening succeeds. Correlate both outgoing attempts, translated endpoints, authentication, and selected route. |
| Routers choose different source ports for relay and peer destinations | Failed direct attempts leave relay requests working within their deadlines. No static forwarding rule may be added to manufacture a success. |
| Routers permit the relay but deny peer traffic | Requests remain relay-backed; attempts release their sockets and workers. |
| One owner declines, the address is unapproved, or the certificate is wrong | No direct application request is admitted. |
| Relay disappears after successful promotion | In-scope direct requests continue only until the unchanged lease expires. |
| Direct path fails after a write is submitted | Record the provider's durable receipt; the operation is never automatically repeated. The caller may report an unknown outcome. |
| Delay, loss, restarts, or mapping changes | Record success rate, time to promotion, time to fallback, and resource cleanup. A late candidate cannot replace an admitted generation. |

Repeat attempts with fresh agents, identities and router connection state. Keep
outcomes per network profile, including failures. Report the denominator and
latency distribution rather than presenting one successful connection as a
general reachability result.

For outage tests, explicitly drop traffic in the lab router or close the relevant
connection. Removing an EC2 security-group rule may leave an established tracked
connection alive: see [AWS connection tracking](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/security-group-connection-tracking.html).

Use the SQLite provider for a real write/read exercise after basic byte transport
works. Give each write a unique operation marker and independently check its
durable count. Reuse existing owner-scoped provider grants and direct-policy files.

## Lifecycle and cost controls

Before launch, select the AWS profile and region, verify access, and calculate
the bounded run estimate from current instance, disk and public-address prices.
Keep the selected account and all resource IDs in a local run manifest.

Create only dedicated, tagged lab resources. Avoid changes to existing networking
or services. Use a short lifetime, a shutdown timer installed before setup work,
and instance-initiated shutdown configured to terminate the test instances. An
operator teardown must remove all resources created by that run and verify the
instances, disks and charged addresses are gone. A shutdown timer is not a
guaranteed billing cap.

## Interpretation

This lab can establish behavior under specific, measured cloud and Linux-router
conditions. It cannot establish universal compatibility with consumer routers,
carrier-grade translation, enterprise networks, or every operating system.
Follow it with endpoints on separate real networks, such as home broadband and
a mobile hotspot. Correct fallback is a required result whenever a direct path
cannot be established.
