# ARC traversal-lab runner

`scripts/traversal_lab/runner.exs` is a line-oriented process harness for the remote traversal lab.
It keeps an ARC endpoint alive across several requests so the orchestrator can
observe the selected route and lease after a relay or network change.

Run it from the repository root with the pinned toolchain:

```sh
mise exec -- mix run --no-compile --no-start scripts/traversal_lab/runner.exs -- initialize /var/lib/arc-lab/citizen
mise exec -- mix run --no-compile --no-start scripts/traversal_lab/runner.exs -- relay relay.json
mise exec -- mix run --no-compile --no-start scripts/traversal_lab/runner.exs -- provider provider.json
mise exec -- mix run --no-compile --no-start scripts/traversal_lab/runner.exs -- citizen citizen.json
```

Initialization writes a generated test identity under the supplied state root.
It prints only the identity's public key:

```json
ARC_LAB {"event":"initialized","public_key":"..."}
```

Create both identities before writing the exact peer keys into their policies.
Never retain or transmit the state-root key files.

## Configurations

`relay.json`:

```json
{"port":7331,"public_key":"64-lowercase-hex-characters"}
```

`provider.json`:

```json
{
  "state_root":"/var/lib/arc-lab/provider",
  "relay":{"host":"198.51.100.10","port":7331,"public_key":"64-lowercase-hex-characters"},
  "direct_policy":"/var/lib/arc-lab/provider/direct-policy.json",
  "serve":"exec:///absolute/path/to/provider-runtime?manifest=/absolute/path/to/manifest.json"
}
```

`citizen.json`:

```json
{
  "state_root":"/var/lib/arc-lab/citizen",
  "relay":{"host":"198.51.100.10","port":7331,"public_key":"64-lowercase-hex-characters"},
  "direct_policy":"/var/lib/arc-lab/citizen/direct-policy.json",
  "address":"binary-echo+arc://provider-public-key/main",
  "capability":"primary",
  "request_timeout_ms":10000
}
```

The policy files remain the source of direct-route consent. For the punch
profile, each contains one exact peer rule with the same capability, scheme,
path, peer public address in `dial`, and `"hole_punch": true`.

## Input and output protocol

Every process reads one JSON object per stdin line.

| Role | Accepted commands |
| --- | --- |
| relay | `{"op":"status"}`, `{"op":"shutdown"}` |
| provider | `{"op":"status"}`, `{"op":"shutdown"}` |
| citizen | `{"op":"request","body":"..."}`, `{"op":"status"}`, `{"op":"shutdown"}` |

Every response is one `ARC_LAB ` prefixed JSON line. Request reports contain
outcome, reply byte count, elapsed milliseconds, and public direct-route
state. A response of at most 65536 bytes is also exposed as `body_json` only
when it is valid JSON. Use this for synthetic SQLite assertions. Opaque replies
remain length-only so a binary echo cannot log the submitted request. Endpoint
status also includes its own relay connection's local and
relay-observed IP address and port. This lets the orchestrator compare the
observed mapping with router counters before negotiation. A successful relay
request has `"active":false`; a successful punched route has
`"selected":"punch"`. The runner never emits request bodies, identities
beyond public keys, policy content, candidate addresses, certificates, sessions,
or private keys.

## Cloud lifecycle

The first EC2 attempt was blocked by AWS account verification. A subsequent
launch succeeded after the account was cleared. See
`docs/transport/VERIFICATION-2026-09-16.md` for measured results and limitations. Use only a dedicated lab, never an existing
server. The router script requires the bootstrap's dedicated-host marker.

Run from the repository root. Use a new ignored output directory for each launch,
and a temporary SSH key. The launch state records exact resource IDs for cleanup.
The scripts default to profile `default` and region `eu-west-1`. Determine the
operator's current public address and use its exact `/32`; do not open SSH globally.

```sh
mkdir -p tmp/my-traversal-run
chmod 700 tmp/my-traversal-run
ssh-keygen -q -t ed25519 -N '' -f tmp/my-traversal-run/ssh_key
python3 scripts/traversal_lab/cloud.py create \
  --state tmp/my-traversal-run/cloud.json \
  --operator-cidr OPERATOR_IP/32 \
  --ssh-public-key tmp/my-traversal-run/ssh_key.pub
python3 scripts/traversal_lab/deploy.py \
  --state tmp/my-traversal-run/cloud.json \
  --key tmp/my-traversal-run/ssh_key --output tmp/my-traversal-run/evidence
python3 scripts/traversal_lab/campaign.py \
  --state tmp/my-traversal-run/cloud.json \
  --key tmp/my-traversal-run/ssh_key --output tmp/my-traversal-run/evidence \
  --setup --repeat 3
```

Always run cleanup, including after a partial launch or failed experiment:

```sh
python3 scripts/traversal_lab/cloud.py teardown \
  --state tmp/my-traversal-run/cloud.json
```

The host shutdown timer is a backup, not a billing cap or network-resource
cleanup mechanism. Independently check termination and absence of remaining
charged volumes/addresses after teardown. Never copy remote identity state back.

`deploy.py` packages allowlisted repository sources, including current uncommitted
changes, and records each file digest and the archive digest. It deploys through
SSH, uses mise for all language runtimes, and compiles on Linux. It rejects an
incomplete or torn-down launch manifest. Its remote host keys are recorded in a
run-specific known-hosts file.

`network.sh` uses private endpoint namespaces, source address translation and
connection tracking. Normal profiles contain no static forwarding rule. The
`listener` profile is explicitly a positive control and is never reported as hole
punching. `changed` translates peer traffic to a different port; `blocked` rejects
peer traffic while retaining the relay path. Inspect recorded mappings and rule
counters to confirm that a profile actually produced its intended conditions.

`campaign.py` creates fresh endpoint state per invocation and trial. It records
safe ARC route status, synthetic SQL replies, independent durable row counts,
unsolicited inbound probes, router counters and header-only packet text. Success
through a relay is distinguished from an active route with `selected: punch`.
After the matrix exits, run the separate fault experiments on the same hosts:

```sh
python3 scripts/traversal_lab/faults.py \
  --state tmp/my-traversal-run/cloud.json \
  --key tmp/my-traversal-run/ssh_key --output tmp/my-traversal-run/faults
```

These verify the unchanged lease deadline after relay loss and an unknown caller
outcome without write replay after a committed write loses its reply. The latter
uses a test-only wrapper around the real SQLite provider: it signals after commit,
then holds the reply until the orchestrator blocks the direct path. It does not
change the production provider or transport. Do not run matrix and fault campaigns
concurrently: they own the same router rules and relay port.
