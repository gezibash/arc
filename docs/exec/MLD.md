# MLD: the machine lifecycle definition

Status: proposed. No code implements this document. It generalizes the Sprite
lifecycle of [the exec spec](SPEC.md), sections 6, 10, and 11.

## 1. Purpose

A citizen runs on a machine. Each platform starts, holds, and stops a machine
in its own way. The MLD is one contract for that lifecycle, so ARC supports a
new platform through one script and one capability file.

The MLD answers four questions for one machine:

1. How does a caller wake the machine?
2. How does the machine stay awake while it works?
3. What are the limits of the platform?
4. Which identity does the citizen use on that machine?

## 2. Two shapes of machine

| Shape | Example | Machine after the work | Identity |
| --- | --- | --- | --- |
| Persistent | A Fly.io Sprite, a laptop, a server | The machine pauses or stays idle. The disk survives. | The citizen key lives on the machine. |
| Per-task | A Vercel Sandbox, an E2B sandbox | The platform destroys the machine. | The machine holds a delegated key for one task. |

A persistent machine keeps one identity for its life. A per-task machine gets
a new key for each task, and a delegation binds that key to a citizen.

## 3. The machine script

Each platform has one script, `citizen/machine.<platform>`. The script
implements these verbs:

| Verb | Action | Exit status |
| --- | --- | --- |
| `info` | Print the capability file of the platform as JSON. | 0 |
| `wake` | Start or resume the machine. Prepare the citizen. Wait until the relay has the announcement. | 0 when the citizen is ready |
| `hold SECONDS` | Keep the machine awake for this time. Refresh an existing hold. | 0 |
| `release SECONDS` | Shorten the hold to this time. | 0 |
| `stop` | Stop the machine now. | 0 |

Rules:

- `wake` is idempotent. A second call on a ready citizen refreshes the hold
  and returns at once.
- On a platform that never pauses, `hold` and `release` do nothing.
- On a per-task platform, `wake` creates the machine, installs the bundle, and
  supplies the delegated key.
- The script writes nothing to standard output, except for `info`. A caller
  uses the script in an SSH `ProxyCommand`.
- Every verb fails with a status other than 0 and a message on standard error.

## 4. The capability file

`machine.<platform> info` prints one JSON object:

```json
{
  "platform": "sprite",
  "persists_disk": true,
  "persists_processes": true,
  "max_session_ms": null,
  "wake_ms": 2000,
  "identity": "resident"
}
```

| Field | Meaning |
| --- | --- |
| `platform` | The name of the platform |
| `persists_disk` | The disk survives a stop. |
| `persists_processes` | A process survives a pause. |
| `max_session_ms` | The longest life of one machine. `null` means no limit. |
| `wake_ms` | The time from `wake` to a ready citizen, as a guide |
| `identity` | `resident` or `delegated` |

The limits are the limits of the platform. ARC does not extend them. ARC
reports them, so a caller learns of a limit before the work starts, and not
after the platform stops the machine.

The provider refuses a job when `timeout_ms` is larger than `max_session_ms`.
The error is `job_exceeds_machine_limit`. The caller then picks a platform
with a larger limit, or splits the work.

## 5. Identity

### 5.1 Resident identity

The citizen key lives on the machine. The machine keeps that key for its life.
This is the model of the exec spec today. The key never leaves the machine.

### 5.2 Delegated identity

A citizen extends itself onto another machine for one task. The citizen signs
a delegation for a new key. The new key acts for the citizen until the
delegation expires.

WARNING: Do not copy a citizen key onto a per-task machine. A copy gives the
platform, and every holder of the wake configuration, the full identity of the
citizen. A delegation expires. A copied key does not.

The delegation is a signed JSON object:

```json
{
  "version": 1,
  "delegator": "<64 hex, the citizen>",
  "delegate": "<64 hex, the key on the new machine>",
  "capabilities": ["exec:run", "dm:send"],
  "issued_at": 1789890000,
  "expires_at": 1789893600,
  "signature": "<128 hex by the delegator>"
}
```

Rules:

- The signature covers `arc-delegation-v1\n` and the compact JSON of the other
  fields, with sorted keys. This matches the rule of a relay announcement.
- The maximum life of a delegation is 1 hour. A longer task needs a new
  delegation.
- `capabilities` lists what the delegate does for the delegator. An empty list
  permits nothing.
- A receiver accepts a delegated request when the delegation verifies, the
  time is inside the window, and the capability covers the request.
- A grant names a citizen. A delegation of that citizen passes the grant, for
  the listed capabilities only.
- The delegate signs its own packets with its own key. The delegate never
  holds the key of the delegator.
- Revocation is expiry. Version 1 has no revocation list. Keep the window
  short.

### 5.3 Who delegates

The citizen delegates. A manager does not delegate for a citizen, because a
manager does not hold the citizen key.

Example: a citizen on a Sprite receives a task that needs 32 CPUs. The citizen
creates a machine on another platform, generates a key there, and signs a
delegation for 20 minutes. The new machine runs the task and reports the
result. The delegation then expires.

## 6. Result of a task

The job request carries `reply_to`, a public key. The default is the caller.

- The machine sends the result to `reply_to` before it releases the hold.
- On a per-task machine, the machine sends the result before the platform
  limit ends the session.
- If the machine stops before the result leaves, the job state is `lost`. See
  [the exec spec](SPEC.md), section 12.1.

A delegate reports to `reply_to` in the same way. The DM comes from the
delegate key with its delegation attached, so the reader verifies the chain.

## 7. Caller flow

1. The caller reads the capability file of the citizen from its wake
   configuration.
2. If the work exceeds `max_session_ms`, the caller stops and reports the
   limit.
3. The caller runs `wake`.
4. The caller sends the request.
5. The machine holds itself awake while it works, and releases the hold at the
   end.

## 8. Platforms

| Platform | Shape | Wake | Hold |
| --- | --- | --- | --- |
| `sprite` | Persistent | The Sprites API through the CLI | A task of the Tasks API |
| `none` | Persistent | Start `arc serve` when it does not run | Nothing |
| `vercel` | Per-task | Create a sandbox. Supply the bundle and the delegation. | The session of the sandbox |

A new platform adds one script. The provider, the caller, and the relay do not
change.

## 9. Open questions

- Does a delegation belong in the packet header, or in the application body?
  The header suits the relay. The body suits a provider.
- A per-task machine needs the bundle and its runtime. A prepared image makes
  the wake fast. Who builds that image?
- A delegate writes to the mailbox of `reply_to`. Does the mailbox provider
  verify the delegation, or does the reader verify it?
- `max_session_ms` bounds one machine. A job that needs more time needs a
  chain of machines. That is separate work.

## 10. Phases

| Phase | Scope |
| --- | --- |
| 1 | The machine script and the capability file. Move `sprite` and `none` to them. |
| 2 | The provider refuses a job that exceeds `max_session_ms`. |
| 3 | The delegation format, and verification in the exec provider. |
| 4 | The `vercel` platform, with a delegated identity. |
