# Private environments on ARC

Status: proposed behavioral contract. No private-environment provider or
hardware-backed verification is implemented by this document. Operation and
field names below are proposed, not existing ARC commands or wire formats.

## 1. Purpose

A citizen chooses what runs, what it can read, and who receives its results.
Infrastructure operators supply resources without receiving permission to read
the citizen's code, working data, or private results.

A private environment is a policy and ownership boundary that can outlive a
particular computer. Compute is replaceable. Persistent data can live with
independent storage providers. ARC carries authenticated, encrypted traffic
between the participants.

This contract requires privacy from the compute host's operator, including
host administrator access. Ordinary processes, containers, and ordinary virtual
machines do not meet it. Unsupported machines must be rejected for this
contract, without silently falling back to weaker execution.

MUST and MUST NOT are conformance requirements. SHOULD identifies a default
that needs a documented reason to change. This is a design contract, not a
claim of an implemented or independently audited security guarantee.

## 2. Parties and responsibilities

| Party | Responsibility | Information it may receive |
| --- | --- | --- |
| Citizen | Owns the environment and authorizes work | Their code, data, results, and policy |
| Citizen's controller | Verifies evidence, grants access, holds key-release authority, and tracks current state | Keys and private metadata needed for that role |
| Relay | Routes ARC traffic | Routing identities, timing, traffic sizes; encrypted bodies |
| Compute operator | Allocates resources and starts or stops a protected guest | Public bootstrap software and resource requirements; encrypted workload traffic |
| Protected guest | Enforces policy and executes the authorized workload | Only the code, data, and secrets granted to that run |
| Storage provider | Stores and returns encrypted objects | Opaque object identifiers, ciphertext, sizes, access patterns, and retention instructions |

The controller is a role chosen by the citizen, not a mandatory ARC service.
For the first implementation it runs on a citizen-controlled trusted device.
Moving it elsewhere requires its own explicit trust and key-custody design.
It MUST NOT run as an ordinary process on the untrusted compute host.

Private filenames, command arguments, document names, plaintext content hashes,
and workload descriptions MUST NOT appear in public scheduling requests or
storage metadata. Object identifiers refer to ciphertext or use random values.
An operator may still infer information from resource requirements and traffic.

## 3. Guarantees and limits

### Required protections

- Relays, storage operators, other citizens, and the compute host MUST NOT
  receive plaintext workload code, input, output, or their decryption keys,
  except where the citizen explicitly names a party as an output recipient.
- The design MUST assume those infrastructure operators can collude, modify
  messages, replay saved state, substitute software, and withhold service.
- Only a guest accepted by the citizen's policy may receive workload secrets.
  Encryption MUST terminate inside that guest, or in a trusted citizen endpoint.
- Each run MUST have a distinct identity and access grant. One citizen's run
  MUST NOT be able to read another citizen's memory, files, or credentials.
- Invalid evidence, denied access, and uncertain state MUST fail closed. The
  system MUST NOT retry private work on an unprotected machine.

### Explicit trust

The citizen trusts their endpoint and controller, the approved guest software,
the selected hardware security implementation and firmware, and the roots
used to verify its evidence. Each supported hardware profile MUST document
which administrators and platform components remain trusted, its known
limitations, and how security updates or revoked endorsements are handled.

Running arbitrary citizen code requires isolation inside the guest as well:
the workload MUST NOT modify the policy enforcer or steal its signing and
storage credentials. The approved guest includes that isolation boundary,
its operating system, and its enforcement software. A private workload is
uploaded encrypted after the public bootstrap guest has been verified.

### Claims this contract does not make

- Availability: an operator can stop a guest, deny networking, or lose data.
- Anonymity: identities, timing, sizes, and access patterns are not hidden.
- Universal resistance to physical attacks or hardware side channels. Limits
  belong in the selected hardware profile; they cannot be omitted from the
  privacy claim.
- Retraction of knowledge: an authorized reader cannot be made to forget data.
- Guaranteed physical deletion of remote ciphertext or unseen copies.
- Correctness of arbitrary code or truthfulness of its conclusions. A signed
  result can identify an accepted execution without proving the program right.

## 4. Ownership, identities, and permissions

An environment has a stable, opaque `environment_id`, an owner public key,
an owner-authorized policy revision, and a controller-recorded state head.
Each execution has a fresh `run_id` and an ephemeral guest identity. An
environment's identity and saved state MUST NOT be derived from a provider's
account name, storage address, or machine identifier.

The citizen's permanent identity secret MUST NOT be copied into the guest.
The controller may hold separately delegated authority under the citizen's
policy. It MUST NOT gain authority merely because a provider supplied its
address. Guest connection and receipt-signing secrets are generated inside
the protected boundary and are not exported to the operator.

The controller issues a signed run grant, delivered inside the verified
channel. It binds at least:

| Field | Meaning |
| --- | --- |
| `environment_id`, `run_id` | The exact environment and execution |
| `issuer`, `subject` | Authorized issuer and the verified guest key |
| `policy_revision` | The owner-approved policy being enforced |
| `workload_digest` | The private code package to execute, including its dependency closure |
| `input_state` | Exact approved dataset versions and state commitment |
| `read`, `write` | Explicit object/version scopes and permitted output locations |
| `recipients` | Public keys allowed to receive plaintext results |
| `egress` | Explicit permitted destinations and operations; empty by default |
| `limits` | Memory, storage, input/output, and execution budgets |
| `authority_epoch` | Controller's current authorization generation |
| `validity`, `request_id` | Validity policy and an identifier for replay and retry handling |

These fields remain private where possible. A grant is a bounded delegation,
not a copy of the citizen's identity. Unknown permissions and undeclared
subdelegation MUST be rejected. A workload cannot widen its own grant.

Signed document encoding, signature domain separation, algorithm identifiers,
and verification rules require a separately reviewed wire specification before
implementation. Existing local host tokens are not this portable grant format.

## 5. Verification and key release

Before any private code, data key, or credential is released:

1. The controller requests resources using only public scheduling information.
2. The operator boots a policy-approved public guest image. The guest generates
   fresh connection and signing keys inside its protected boundary.
3. The controller supplies a fresh, unpredictable challenge. The guest returns
   hardware-backed evidence binding that challenge, its keys, the intended run,
   and the policy-relevant guest configuration.
4. The controller verifies the evidence chain and security status, challenge,
   guest measurements, allowed configuration, and key binding. Debug access,
   operator-controlled guest login, host-injected extensions, and plaintext
   console or memory export MUST be disabled or otherwise excluded by the
   reviewed hardware profile and enforced configuration.
5. The controller establishes an authenticated encrypted channel to the bound
   guest key and verifies possession of its secret. Only then does it deliver
   the grant, private code, and narrowly scoped data access.

A provider's signature proves who advertised a service; it does not prove
protected execution. A self-reported `private` flag is never sufficient.
Evidence for a genuine guest MUST NOT be reusable to release keys to a
different connection, run, or substituted program.

The approved guest MUST verify the uploaded workload against the private
grant before running it. Boot measurements of a generic runner do not by
themselves identify code subsequently uploaded into that runner.

The host MUST only relay ciphertext on the private path. Terminating ARC
encryption in a host process and forwarding plaintext into a protected guest
does not conform. A valid design either runs the private ARC endpoint inside
the guest or carries an independently authenticated encrypted inner channel
through a host-side ARC endpoint. The concrete channel construction remains
a wire-design decision; it MUST use reviewed cryptographic primitives.

## 6. Separate compute from persistent data

Persistent data consists of immutable encrypted objects and a private manifest
describing their names, versions, relationships, and permitted readers.
Storage providers hold those objects without their decryption keys.

- Data and manifest encryption happens on the citizen's trusted device or
  inside an accepted guest. Keys MUST NOT reach a storage provider.
- Each dataset or version uses an independent data key. Key wrapping to an
  authorized guest occurs only after verification and grant validation.
- Authenticated encryption MUST bind objects to their dataset, version, and
  logical role to detect substitution, reordering, and truncation. The precise
  chunk format and nonce management require wire-level review.
- Storage read authorization and decryption authority are separate. Permission
  to retrieve ciphertext does not confer permission to read its contents.
- Scratch space, swap, crash dumps, checkpoints, application logs, and cached
  credentials MUST stay inside protected memory or be encrypted before they
  leave it. Unsupported features MUST be disabled.
- Data export MUST include encrypted objects, the private manifest, and the
  citizen-controlled recovery material needed to use them elsewhere.

The owner can change storage providers by copying ciphertext. A new compute
run reads the same approved state only after fresh verification and access
authorization. Neither migration requires giving an infrastructure operator
the citizen's identity secret or plaintext files.

### Freshness and durable state

Authentic old data is still old data. Signatures and encryption alone do not
establish which saved version is current.

For the first implementation, the citizen's online controller is the authority
for the latest accepted state. It durably records a monotonically advancing
revision and a commitment to the encrypted manifest. The environment has a
single authorized writer epoch; competing commits use compare-and-swap against
the expected parent revision. Controller storage and recovery are part of the
trusted state-freshness boundary.

A guest MUST receive the controller's current head before opening mutable
state. After uploading encrypted changes, it proposes the new manifest and
expected parent. The controller accepts at most one successor for that parent
and writer epoch. Duplicate request identifiers MUST return the prior outcome
rather than repeat a committed effect. An acknowledgement from a storage
operator alone MUST NOT advance the authoritative head.

Before accepting a head, the controller MUST check that the required encrypted
objects can be retrieved and match their commitments under the citizen's
storage policy. This establishes observed storage at that time, not perpetual
availability. Interrupted uploads remain uncommitted. A lost reply is resolved
by querying the controller's recorded outcome.

If the controller is unavailable, new key release, mutable-state opening, and
state commits MUST stop. Previously authorized computation may continue, but
cannot claim a new committed state. Losing the trusted head means freshness is
unknown; recovery MUST surface that uncertainty rather than accept whatever
an operator calls latest. Replicated controllers and offline freshness proofs
are outside the first implementation.

## 7. Workload execution and outputs

The guest MUST enforce the grant independently of the host. It MUST verify
the caller's signed authority inside the protected boundary. An unprotected
adapter's plaintext `from` field is insufficient for this contract.

For the first implementation, a run has no general internet access. Approved
ARC destinations and operations are explicit exceptions enforced outside the
untrusted workload but inside the protected boundary. Host files, management
sockets, inherited credentials, DNS, instance metadata, and diagnostic export
are denied by default. Necessary bootstrap and evidence-verification traffic
has a separate public policy and cannot carry workload secrets.

Private results and execution logs MUST be encrypted to the named recipients
or committed as encrypted state. Public status is limited to opaque run IDs,
coarse lifecycle state, and agreed resource accounting. Errors MUST NOT expose
command lines, private paths, inputs, or stack values. The operator MUST NOT
receive plaintext command logs merely to provide an audit trail.

An encrypted execution receipt SHOULD contain the run ID, accepted guest and
policy identifiers, private workload digest, input and output commitments,
exit status, and commit outcome. The guest signs it with its verified key.
The receipt describes what the approved guest reports; it is not a general
proof of correct computation or independent proof of provider billing.

## 8. Lifecycle, revocation, and recovery

```text
requested -> booted -> verified -> authorized -> running
running -> finished -> committed
running -> failed
any nonterminal state -> cancelled / lost
```

These are controller-observed states. Resource allocation is not verification,
verification is not authorization, and a finished process is not a committed
result. A restarted guest is a new run requiring fresh evidence and authority.

Revocation advances the controller's authority epoch. The controller MUST
refuse further key release, new connections, and commits under a revoked or
expired grant. It MUST use trusted time and recorded state for those decisions,
not a host-supplied clock. Expiry cannot guarantee that a hostile host stops an
already authorized computation or destroys a paused copy of its memory.

Revocation cannot recover plaintext or keys already released to an accepted
run. Resumed old guests MUST obtain fresh controller approval before a new
commit or new data access. An output recipient requiring current authority
must check the controller before accepting an old run's receipt. Stronger
mid-run revocation is not claimed.

The first implementation restores application-level encrypted checkpoints;
it does not migrate live memory snapshots. Migration starts a fresh accepted
guest, authorizes a new writer epoch, and restores the latest committed state.
Old epochs cannot commit, including when the host resumes an old guest.

On completion the guest MUST discard transient keys and plaintext using its
supported cleanup mechanisms. Remote cleanup is best effort and cannot prove
that an operator retained no old ciphertext or snapshots. Encrypted backups,
retention, and recovery recipients remain choices in the citizen's policy.
Loss of every authorized recovery key makes the corresponding data unreadable.

## 9. Provider-facing operations

These are logical operations for the proposed contract. They do not define
new `arc` subcommands or `serve` URI schemes.

| Operation | Contract |
| --- | --- |
| Describe capacity | Return signed public capability information and supported evidence profiles; advertisement grants no trust |
| Request run | Allocate an opaque run handle from public resource requirements; retries are idempotent |
| Obtain evidence | Return fresh guest evidence and bound keys for the controller's challenge |
| Authorize run | Deliver a signed grant and encrypted workload to the verified guest |
| Exchange private data | Read approved inputs and send commands/results through the private channel |
| Commit state | Propose an encrypted manifest against the controller's expected head and writer epoch |
| Inspect status | Return only the authorized caller's coarse run state and private receipt when available |
| Cancel run | Withdraw future authority and request cleanup; do not claim physical deletion |
| Resume environment | Create a new run from the latest controller-approved encrypted checkpoint |

Storage has separate put/get and retention operations on encrypted objects.
Providers MUST enforce owner-scoped control access so one citizen cannot
inspect, cancel, or commit another citizen's run. Attempts to cross that
boundary MUST fail regardless of knowledge of a run or object identifier.

Failures must distinguish unsupported protection, invalid or stale evidence,
policy mismatch, forbidden access, stale state, controller unavailable,
resource exhaustion, and lost execution. They MUST NOT disclose private
workload details or trigger automatic downgrade.

## 10. Integration with the current repository

This is a separate provider contract with trusted client/controller work and
an implementation for an explicitly supported hardware profile. It does not
require making a cloud account the citizen's identity or making a relay a
trusted compute broker.

| Existing building block | Reuse and boundary |
| --- | --- |
| [Keypair identity](../../delivery/keys/keys.go) | Owner signatures and guest identities: secp256k1 keys with BIP-340 signatures; key ownership alone says nothing about protected execution |
| [Capability announcements](../../delivery/catalog/catalog.go) | Signed provider advertisements, kind 30272, and discovery; evidence-profile advertisement needs an explicit schema design |
| [Interface manifests](../../iface/manifest.go) | Existing provider-installed command descriptions of interface version 1; security decisions cannot be delegated to arbitrary provider-authored templates |
| [Execution adapter](../../cmd/exec-provider/main.go) | Can host public orchestration; its ordinary process and plaintext input/output are not a protected boundary |
| [Private events](../../delivery/private/private.go) | NIP-44 encryption in NIP-59 gift wraps, to one recipient; no attestation binding today, and no forward secrecy (docs/delivery/SPEC.md, section 13) |
| [Sealed drafts](../../delivery/draft/draft.go) | NIP-37 drafts, sealed to their author; data that an owner keeps for itself, not a storage format for a guest, sender authorization, freshness proof, or guest verifier |
| [Provider grants](../../provider/config.go) | Local authorization by public key; not the signed, portable run grants defined here |

The private-environment implementation MUST supply the missing verifier,
protected endpoint, policy enforcement, grant format, key-release controller,
encrypted state format, and recovery rules. Manifest normalization and client
invocation paths must be reviewed before extending them; undocumented fields
cannot be assumed to survive or enforce policy.

## 11. Acceptance criteria

The first demonstrator is a private document-processing job: read selected
encrypted documents, produce a report for the citizen, and save an encrypted
checkpoint with an independent storage provider. It uses a real supported
protected machine and the citizen's online controller.

| Check | Required result |
| --- | --- |
| Unprotected or debug-enabled host | No private code, data key, or credential is released |
| Substituted image, workload, policy, or guest key | Evidence or grant verification rejects it |
| Replayed evidence or authorization | Wrong challenge, run, subject, epoch, or request binding is rejected |
| Valid evidence routed to another endpoint | Key-possession/channel binding fails before secret release |
| Other citizen presents a known run ID | Reads, writes, control actions, and grants are denied |
| Workload attempts undeclared file, network, metadata, or host access | Guest enforcement denies it |
| Operator observes network, disks, temporary files, logs, and dumps | No workload plaintext or decryption keys appear in those observable surfaces |
| Storage modifies, mixes, truncates, or rolls back objects | Authentication, manifest validation, or controller head checks reject them |
| Host forks or resumes an old writer | At most one valid state successor is accepted; stale epochs cannot commit |
| Host pauses a guest past expiry or revocation | No new authority or commit is accepted, without claiming the prior computation stopped |
| Connection fails during commit | Retry returns the recorded outcome; no duplicate state transition |
| Controller or storage becomes unavailable | Explicit failure or uncommitted state; no claimed successful persistence |
| Compute provider is replaced | Fresh evidence and authority restore the approved checkpoint without releasing plaintext to either operator |
| Recovery material is unavailable | Explicitly unreadable state; no hidden provider recovery key |

Protocol and negative tests can run without confidential hardware, but MUST
be labeled simulations. Actual host-private execution requires real-hardware
evidence, adversarial testing of the selected profile, and review of the
cryptographic construction. Absence of plaintext in a sample capture alone
does not prove the host cannot read it.

## 12. Decisions before implementation

The following choices remain open; this contract does not select or deploy
a cloud provider, purchase machines, introduce dependencies, or change ARC's
existing public commands:

- Initial hardware profile and exactly which platform components remain trusted.
- Guest operating system and workload isolation implementation.
- Reviewed attestation verifier and evidence-to-channel binding construction.
- Canonical grant, encrypted object, checkpoint, and receipt wire formats.
- Controller persistence and key recovery implementation.
- Provider manifest extensions and the trusted client invocation experience.
- Concrete resource ceilings and the initial storage durability policy.

Keep the first scope narrow: batch execution, explicit recipients, independent
encrypted storage, an online citizen controller, and fresh-guest checkpoint
recovery. Interactive shells, shared multi-citizen guests, general internet
access, accelerators, and live snapshot migration need separate extensions.

## References

- [Azure confidential VM overview](https://learn.microsoft.com/en-au/azure/confidential-computing/confidential-vm-overview): hardware-backed isolation and its platform boundaries.
- [Guest attestation](https://learn.microsoft.com/en-us/azure/confidential-computing/guest-attestation-confidential-vms): evidence of protected hardware and guest security settings.
- [Nitro evidence verification](https://docs.aws.amazon.com/enclaves/latest/user/verify-root.html): signed evidence, challenges, and key-binding fields; a reference mechanism, not a selected backend.
- [Firecracker snapshot security assumptions](https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/snapshot-support.md): ordinary virtualization trusts the host and snapshot files.
