# Cloud traversal verification: 2026-09-16

## Result

**The controlled EC2 experiment passed.** The network matrix completed 28 trials
and 84 SQLite requests. Every returned result matched expectations, and every
trial's stored marker appeared exactly once when read independently on the
provider host. Separate relay-outage and interrupted-write experiments also passed.

This establishes behavior behind the measured Linux routers. It does not establish
universal compatibility with home routers, mobile carriers or enterprise networks.

## Measured outcomes

| Conditions | Trials | Observed result |
| --- | ---: | --- |
| Both routers reject unsolicited inbound traffic; stable mappings | 4/4 | Authenticated `punch` route; correct SQL write/read |
| Stable mappings with 40 ms delay and 2% loss on each router's return interface | 3/3 | Authenticated `punch` route; correct SQL write/read |
| Routers change source ports for peer destinations | 3/3 | Relay fallback; correct SQL write/read |
| Routers block all peer traffic while allowing relay traffic | 3/3 | Relay fallback; correct SQL write/read |
| Neither owner grants direct permission | 3/3 | Relay transport; no direct route |
| Only one owner grants direct permission | 3/3 | Relay fallback; no direct route |
| Peer address is outside the caller's exact allowlist | 3/3 | Relay fallback; no direct route |
| Only the provider has an approved forwarded listener | 3/3 | Direct route selected `provider` |
| Only the citizen has an approved forwarded listener | 3/3 | Reverse-dialed direct route selected `caller` |

The impaired profile's delay and loss are configured test conditions, not inferred
from application latency. These small samples are feasibility checks, not estimates
of success rates across the public internet.

### Relay outage

After verifying a punched route, the relay process was stopped. Both endpoints
reported that their relay connection was unavailable. A SQL read still returned
correctly over the active direct route. The caller then refused requests after
its original remaining lease expired; the reply was `relay_not_connected`.
The test measured expiry from the original deadline, not a refreshed deadline.

### Lost reply after a committed write

A test-only wrapper around the actual SQLite provider signalled after the database
transaction committed and held its reply. Both routers then dropped the existing
direct path before the wrapper released that reply.

The caller returned `outcome_unknown` after 10,001 ms. An independent database
read found exactly one marker, and the wrapper recorded exactly one execution.
The application operation was not automatically replayed through the relay.

An unknown outcome is intentional: a caller must not blindly retry a write that
may already have committed. This test does not establish instant recovery of a
broken direct connection or automatic reconciliation of an uncertain operation.

## Topology and evidence

The default AWS account ran three dedicated `t3.small` instances in `eu-west-1`:
relay, citizen router/endpoint, and provider router/endpoint. Each endpoint ran
inside its own private network namespace at `10.200.0.2`, behind a separate EC2
host translating traffic to its VPC address. There was no direct route between
the private namespaces. The cloud security group isolated the lab; Linux rules
controlled the changing traversal conditions.

Punched trials had no static forwarding rule. Each trial first confirmed that
unsolicited peer-host probes could not reach the relay-observed endpoint. Recorded
packet headers show simultaneous outgoing SYNs from both endpoints. Connection
tracking confirms that the relay-observed source ports were reused for the peer
connection, and ARC reports `selected: punch`. Successful application replies
alone were never treated as evidence of punching.

The ordinary/reverse listener controls used an explicit forwarded port. They are
reported separately and do not count as hole-punching successes.

- [Machine-readable results](evidence/2026-09-16/summary.json)
- [Stable router state](evidence/2026-09-16/stable-citizen-network.txt)
- [Simultaneous-opening packet headers](evidence/2026-09-16/stable-citizen-headers.txt)
- [Changed mapping state](evidence/2026-09-16/changed-citizen-network.txt)
- [Relay-outage record](evidence/2026-09-16/relay-loss.json)
- [Interrupted-write record](evidence/2026-09-16/reply-loss.json)
- [Core source digest and file hashes](evidence/2026-09-16/source.json)

## Timing observations

Cold first requests include discovery, negotiation and provider startup. They are
not isolated promotion timings. Stable punched trials took 1,024–1,615 ms for the
first request. Attempts that fell back after incompatible mappings or blocked
peers took 3,429–4,060 ms. Subsequent successful reads/writes were faster, but these
same-region samples should not be used as internet performance estimates.

Failed upgrades currently add a noticeable initial wait. Reducing that wait while
preserving the no-replay rule is a separate optimization.

## Build and validation

The tested working tree was based on `750e1ff`, including the uncommitted lint and
traversal changes. Core source hashes were identical across all deployed snapshots;
only the test harness changed during calibration. No ARC implementation change
was needed to pass these remote experiments.

All hosts ran Linux `7.0.0-1012-aws`, Erlang/OTP `28` (`erts-16.4`), Elixir `1.19.5`
and Python `3.12.13`. Language runtimes were installed through mise and ARC was
compiled on the hosts. Local `mix lint` passed, including strict Credo and
warnings-as-errors compilation. Test-script syntax and runner formatting passed.

During calibration, disabled-direct status reporting in the test runner and a
listener-control firewall flag were corrected. Router cleanup was strengthened
to cover original and translated reply tuples. These setup failures occurred
before the affected SQL trials; completed results were retained. The implementation
and assertions were not weakened to obtain passes.

Remaining coverage includes real home/mobile networks, different router vendors,
IPv6, multi-relay coordination during promotion, certificate-mismatch attacks in
this remote topology, and sustained load. Existing local tests cover additional
carrier/security cases; those are distinct from the remote evidence here.

## Lifecycle and cost

Run ID: `arc-20260916-traversal-verified`. All hosts had an active two-hour shutdown
timer configured to terminate the instance. The test processes stopped before
operator teardown. Cleanup verification is recorded in
[cleanup.json](evidence/2026-09-16/cleanup.json).

An earlier launch was interrupted by AWS `PendingVerification` after creating two
instances. Those instances and their dedicated network were removed. The successful
run began after the user confirmed that verification had cleared.

Before launch, AWS Pricing reported USD 0.0228/hour per Linux `t3.small` and USD
0.088/GiB-month for `gp3` in Ireland. AWS publishes a public IPv4 charge of USD
0.005/address-hour ([VPC pricing](https://aws.amazon.com/vpc/pricing/)). The planned
three-host, two-hour run with 12 GiB roots was estimated at about USD 0.18 before
transfer and taxes. **The final billed amount has not been checked.**

Only new tagged lab resources were changed. Run manifests and additional packet
header evidence remain in the ignored `tmp/traversal-20260916/` directory. No
identity secrets or private keys are included in the retained public evidence.
Trailing whitespace was trimmed from retained command text for publication;
the raw command output remains with the ignored run records.
