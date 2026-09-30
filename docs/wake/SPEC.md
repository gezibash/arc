# Wake: presence and wakeable citizens

- Status: partial
- Layers: runtime
- Owns: none
- Proof: go test -count=1 -run '^Test(Delivery|Interface)$' ./internal/proof/
- Remaining: the wake URL and the dormant record, section 3.8, and SSH access to a machine that pauses, section 4.4. These are phase 4 of section 8.1.
- Unverified: A wake after the platform dropped the memory of the machine, the start of a stopped HTTP service on a request, and SSH access through section 4.4. Nobody has measured the wake flow on the delivery layer.

A caller wakes the machine of a citizen before a live call. The package
`runtime/wake` runs the wake flow. The exec app, in
[apps/exec](../../apps/exec/README.md), is the first user.

## 1. Purpose

A citizen machine can pause when it is idle. The platform does not charge
compute for a paused machine. A caller that has permission wakes the machine
before it sends a live call. This copies how SSH reaches a machine that
pauses:

| SSH | ARC |
| --- | --- |
| The `ProxyCommand` on the client wakes the machine. Then the client connects. | The wake hook on the caller wakes the machine. Then the caller sends the request. |
| `~/.ssh/config` on the client holds the wake method. | The caller's wake configuration holds the wake method. |
| `authorized_keys` lists who can log in. | The provider's grants list who can run commands. |

The relay only carries events. It holds no cloud credentials.

This spec does not define these things:

- A relay-side queue.
- A wake that the relay does.
- An automatic retry of a command.
- TCP forwarding. `tcp+arc://` is separate work.

## 2. Terms

| Term | Meaning |
| --- | --- |
| Caller | The identity that sends a live call. |
| Citizen | The identity that serves the capability. |
| Manager | A caller that sends work to many citizens. |
| Machine | The computer that runs the citizen. |
| Pause | The platform suspends an idle machine. |
| Wake | An action that makes a paused machine run again. |
| Wake hook | An entry on the caller that tells the caller how to wake one machine. |
| Start script | A program on the machine that prepares the citizen after a wake. |
| Lease | A hold with an expiry. While a lease is live, the machine does not pause. |
| Platform service | A process that the platform starts at boot and restarts after a crash. |
| Plain process | A process that is not a platform service. |

The three roles:

| Role | Where it runs | Presence |
| --- | --- | --- |
| Relay | A long-running server | Always online |
| Manager | A long-running identity. Agent sessions start and stop. | Online. It syncs its messages. |
| Citizen | A machine that pauses | Online only while awake |

A manager sends work to citizens. A citizen runs the work and returns the
result. A citizen has its own identity, its own grants, and its own agent
instructions.

## 3. Rules

### 3.1 What the design assumes of a platform that pauses

- A pause drops each open TCP connection. Thus a relay connection does not
  survive a pause, and relay traffic cannot wake a paused machine.
- A platform action wakes the machine. The caller runs it with the caller's
  own cloud credentials.
- A lease keeps the machine awake until the lease expires. A refresh moves
  the expiry.
- A plain process does not keep the machine awake.
- A running platform service can block the pause. The design must work if it
  blocks the pause, and if it does not.

### 3.2 Design rules

1. The relay carries events. It MUST NOT hold cloud credentials.
2. The caller wakes the machine. The wake hook MUST use the caller's own
   cloud credentials.
3. The citizen MUST keep its machine awake with a lease while a command runs.
4. No process of the citizen runs as a platform service that stays active.
5. `arc serve` runs only while a lease is live. A live lease proves that the
   machine did not pause.
6. If no lease is live, a pause is possible. Then the start script MUST start
   a new `arc serve` with a new relay connection.
7. The relay runs on a different machine that does not pause.

CAUTION: Do not run `arc serve` or `sshd` as a platform service on a citizen
machine. A running platform service blocks the pause, and the machine costs
compute all the time.

### 3.3 Presence states

A caller sees a citizen in one of three states:

| State | Condition |
| --- | --- |
| `online` | A relay has a current announcement for the key. |
| `asleep` | No current announcement. The caller has a wake hook for the key. |
| `offline` | No current announcement. The caller has no wake hook. |

The caller calculates `asleep` from its own wake configuration. The relay does
not change. The `wake` package calculates the state. `arc resolve` does not
show it.

### 3.4 Wake configuration

The caller keeps wake hooks in `wake.toml` in the directory of ARC (default
`~/.config/arc/wake.toml`), one hook for each citizen key. This file has the
same role as `ProxyCommand` in `~/.ssh/config`.

```toml
[wake."<citizen-public-key>"]
kind = "command"
argv = ["<wake-program>", "<machine>", "<start-script>"]
```

- The `command` kind runs a local program. Exit status 0 means that the
  citizen is ready.
- In this example, `<wake-program>` wakes the machine through the platform,
  and runs the start script on it. For a machine that you reach with SSH,
  `argv` can be `["ssh", "<host>", "<start-script>"]`.
- The caller's own cloud credentials authorize the wake. The file MUST NOT
  store a token.
- Other platforms use the same kind with a different program. ARC does not
  change.
- `arc` runs only the `command` kind. A hook of another kind, or a hook with
  an empty `argv`, fails each request to its citizen with `wake_failed`.
- A file that is not valid TOML, or that has a bad key, fails every request
  to a citizen. Commands that send no live call, for example `arc discover`,
  do not read the hooks.

### 3.5 Timing

- The default wake timeout is 30 seconds. It includes the time of the wake
  hook.
- If the last reply from the citizen arrived less than 30 seconds ago, the
  caller skips the wake hook.
- The provider's `release` sets a lease expiry of 60 seconds. The 60-second
  expiry is the reason for the 30-second rule. See the lease of the exec app
  in [apps/exec](../../apps/exec/README.md#lease).
- `arc serve` signs its announcement again every 2 minutes. An announcement
  is current for 5 minutes after it is signed.

### 3.6 Lease program

The operator supplies a lease program for the platform of the machine. ARC
holds no platform program. A machine that never pauses needs no lease.

| Platform | Machine | Lease |
| --- | --- | --- |
| A platform that pauses | It pauses. | A hold of the platform, with an expiry |
| A machine that never pauses | It does not pause. | None |

The lease program must do these operations:

| Operation | Action |
| --- | --- |
| Hold | Create or refresh the lease, with an expiry in seconds. |
| Create | Create the lease. Report if the lease exists. |
| Delete | Delete the lease. |

To add a platform, write a lease program for it. The provider, the start
script, and the caller do not change.

### 3.7 Start script

The operator supplies the start script. It runs on the machine. It must do
these steps:

1. If the platform can pause, the script creates the lease with an expiry of
   120 seconds.
   - If the lease existed, the machine did not pause. The script refreshes
     the lease to 120 seconds.
   - If the lease did not exist, the machine can have paused. The old relay
     connection is not reliable.
2. If the lease existed, or the platform does not pause, and `arc serve`
   runs, the script exits with status 0.
3. If not, the script stops the old `arc serve` process group. Then it starts
   a new `arc serve` as a plain process.
4. The start script waits until `arc serve` writes the line `serves` to its
   log. `arc serve` writes it when two conditions are true: each relay has
   taken or refused its first watch, and at least one relay has the watch.
   If a relay refuses the watch, `arc serve` logs the relay and tries again
   every 3 seconds. A caller tries its relays in turn, so a live call goes
   through a relay that has the watch. Then the script exits with status 0.
5. If `arc serve` does not write that line within 25 seconds, the start
   script deletes the lease. Then it exits with status 1.

After a wake the network needs a moment. The start script must wait until it
can reach the relay before it starts `arc serve`.

A refresh to 120 seconds can shorten the lease of a running command. This is
safe. The provider refreshes the lease again in 60 seconds or less. If no
request arrives, the lease expires after 120 seconds. Then the machine
pauses.

### 3.8 Wake URL and dormant record

Not built. This part removes the need for local wake configuration, and for
the platform's command-line program on the caller.

Wake URL:

- A small wake service owns the HTTP port of the machine. It stays stopped
  while the machine is idle.
- If the platform wakes a machine on an HTTP request to its URL, the platform
  starts the wake service.
- The wake service runs the start script. It replies after the start script
  exits. Then it stops itself.

```toml
[wake."<citizen-public-key>"]
kind = "https"
url = "https://<machine-url>/wake"
token_env = "PLATFORM_TOKEN"
```

- With the default URL authentication, the request needs a platform token.
  The caller reads the token from the environment variable that `token_env`
  names. The file stores no token.
- With public URL authentication, each host on the Internet can wake the
  machine. The wake service must then check a wake token.
- A granted key signs the wake token. The token holds a time. The wake
  service refuses a token that is older than 120 seconds.
- If the wake token is not valid, the wake service replies with status 403.
  It does not run the start script.

Dormant record:

- Before the citizen releases its last lease, it sends a signed dormant
  record to the relay.
- The record holds the public key, the wake URL, the issue time, and an
  expiry.
- The relay keeps the record after the connection drops. A lookup of the key
  returns the record.
- As a result, each caller sees `asleep` without local configuration.
- The relay does not call the wake URL. The caller calls it.

## 4. Behavior

### 4.1 What ARC gives the wake flow

- A call is a private event. Its seal is signed by the caller, so the
  provider knows the caller's public key. See docs/delivery/SPEC.md,
  section 4.10.
- The relay carries gift wraps. It cannot read commands or output.
- A live call needs a live path now. A store-and-forward call waits in the
  outbox of the caller, and the provider answers it on its next sync.
- `arc call` waits for one reply of a live call for `--timeout`. The default
  and maximum are 120 seconds.
- A direct message waits in the outbox until the recipient acknowledges it,
  for at most 7 days. See docs/delivery/SPEC.md, section 4.2.

### 4.2 What ARC does not give the wake flow

- A relay does not keep a live gift wrap, kind 21059. If the provider is not
  present, a live call fails.
- A relay can show a paused citizen as present for up to 5 minutes, because
  its last announcement stays current (section 3.5). Thus `online` does not
  prove that the machine is awake.
- `arc call` wakes a peer only through a wake hook of the caller. Without a
  hook, it does not wake the peer and does not wait for the peer to connect.
  It fails at once with `peer_offline` when the relays have no current
  announcement of the peer.
- `arc serve` does not detect a pause of its machine.
- If a relay ends the watch of `arc serve`, `arc serve` watches again after
  3 seconds. It does not exit.

### 4.3 The wake flow

1. If the caller has no wake hook for the citizen, the caller uses the
   presence state from its relays.
   - If the state is `online`, the caller sends the request.
   - If the state is not `online`, the caller fails with `peer_offline`.
2. If the last reply from the citizen arrived less than 30 seconds ago, the
   caller skips the wake hook.
3. If not, the caller runs the wake hook. The caller also runs the hook when
   the relay shows `online`.
4. If the wake hook exits with a status that is not 0, the caller fails with
   `wake_failed`.
5. If the hook kind is `command`, exit status 0 means that the citizen is
   ready. `arc` runs no other kind, see section 3.4.
6. If the wake timeout ends, the caller fails with `wake_timeout`.
7. The caller sends the request one time. The caller does not retry the
   command.

`arc` runs this flow before each live call: `arc call`, the installed
commands whose call is live, and `arc update`. A store-and-forward call does
not wake the citizen. The wake counts toward the `--timeout` of `arc call`.

For step 1, `arc` asks its relays for the announcement of the full key. If no
relay answers, `arc` sends the request.

`arc` keeps the time of the last answer of each citizen with a hook in the
directory `wake/` beside `wake.toml`. Thus the next `arc` process skips that
hook too.

The wake hook is the only reliable sign that the machine is awake. The relay
can show `online` for a paused machine. On an awake machine, the start script
only refreshes the lease, so the hook is fast.

### 4.4 SSH access to a machine that pauses

Not built. Some tools speak SSH only. Claude Desktop is an example. The same
design rules apply: `sshd` runs as a plain process, and a lease covers each
session.

```
Host <machine>
  ProxyCommand sh -c '<wake-program> %h <sshd-up> >/dev/null 2>&1 && exec <proxy-program> %h 22'
```

The program `sshd-up` does these steps:

1. If `sshd` does not run, it starts `sshd` as a plain process.
2. It refreshes the lease `ssh` to 300 seconds.
3. If no keeper loop runs, it starts one keeper loop as a plain process.

The keeper loop does these steps each 60 seconds:

1. If a connection to port 22 is open, the loop refreshes the lease `ssh` to
   300 seconds.
2. If no connection to port 22 is open, the loop deletes the lease `ssh`.
   Then the loop stops.

`sshd-up` must write nothing to standard output. The `ProxyCommand` stream
carries the SSH protocol.

## 5. Failures

| Failure | Cause | What the user sees |
| --- | --- | --- |
| No current announcement, no hook | The citizen is not online, and the caller has no wake hook for it. | The call stops at once with `peer_offline`. |
| The hook fails | The wake hook exits with a status other than 0, or it cannot run. | The call stops with `wake_failed`. |
| A hook of another kind | The hook kind is not `command`, or its `argv` is empty. | Each request to that citizen fails with `wake_failed`. |
| A bad wake file | `wake.toml` is not valid TOML, or it has a bad key. | Every request to a citizen fails. |
| The hook is too slow | The wake timeout ends before the hook exits. | The call stops with `wake_timeout`. If the deadline of `arc call` ends the hook first, the error names the time that the hook had. |
| `arc serve` does not serve | `arc serve` does not write `serves` within 25 seconds. | The start script deletes the lease, and exits with status 1. The hook then gives `wake_failed`. |

A request timeout does not prove that the command did not run. The caller
must not retry an arbitrary command automatically.

## 6. Security

- The wake hook uses the caller's cloud credentials. The relay and the
  citizen never receive them.
- A wake costs money. A key without a wake hook cannot wake a citizen.
- A lease costs money while it is live. Each lease has an expiry, so a
  provider that stops cannot keep the machine awake.
- A public wake URL (section 3.8) lets each host on the Internet wake the
  machine for some seconds. Use the default URL authentication unless you
  accept this cost.

## 7. Compatibility

- v0.11.0 removed the wrapper `arc-exec` that ran the wake flow on the caller,
  because `arc` runs the wake flow.
- The operator supplies the start script and the lease program. The
  repository holds no platform program.
- A citizen without a wake hook keeps the older contract: it must have a
  current announcement, or the call stops with `peer_offline`.

This spec states no plan for its removal.

## 8. Proof

### 8.1 Phases

| Phase | Scope | State |
| --- | --- | --- |
| 1 | The start script, the lease in the provider, and the wake flow on the caller. | Built. The operator supplies the start script and the lease program. |
| 2 | Wake hooks and presence states: the `wake` package, `wake.toml`, `peer_offline` (step 1 of section 4.3), and the wake flow before each live call of `arc`. | Built. |
| 4 | The wake URL and signed dormant records, section 3.8. | Not built. |

Phase 3 belongs to the exec app: its jobs and its notify command. See
[apps/exec](../../apps/exec/README.md#jobs).

### 8.2 Tests

- `TestDelivery` in `internal/proof`: "arc runs the wake hook, and the woken
  provider answers", and "arc skips the hook of a provider that answered a
  moment ago".
- `TestALiveCallNeedsACurrentAnnouncementOrAWakeHook` in `cmd/arc`.
- The tests of `runtime/wake`: the presence states, `wake_failed`,
  `wake_timeout`, the 30-second rule, a record that the next process reads,
  and a hook that runs once for two requests at once.

### 8.3 Open questions

- How fast does the relay see a connection that a pause dropped?
- Does a platform start a stopped HTTP service after a wake from a deep
  pause? Does it start a platform service again after an exit with status 0?
- `arc serve` can detect a pause itself. A timer tick that arrives late shows
  a pause. Then `arc serve` connects to the relay again at once. This also
  helps a laptop that sleeps. The start script then keeps the old process.
- The dormant record needs a lifetime that survives disconnection and relay
  restart.

## Gates

This spec predates the gates. See [the grandfathered list](../GRANDFATHERED.md).
