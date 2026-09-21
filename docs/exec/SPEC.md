# Exec: remote commands and wakeable citizens on ARC

Status: proposed. Phases 1 and 3a of section 18 exist in `cmd/exec-provider`:
the provider (section 8), the start script (section 10.4), the lease
(section 11), jobs (sections 12.1 to 12.3), and the wrapper `arc-exec`. The
other sections describe work that does not exist yet.

## 1. Purpose

Exec replaces the SSH workflow with ARC. A caller runs a command on the
machine of a citizen. The caller's public key is the login. The relay carries
the traffic, so the machine needs no open port.

A citizen machine can pause when it is idle. The platform does not charge
compute for a paused machine. A caller that has permission wakes the machine
before it sends the command. This copies how SSH reaches a machine that pauses:

| SSH | Exec on ARC |
| --- | --- |
| The `ProxyCommand` on the client wakes the machine. Then the client connects. | The wake hook on the caller wakes the machine. Then the caller sends the request. |
| `~/.ssh/config` on the client holds the wake method. | The caller's wake configuration holds the wake method. |
| `authorized_keys` lists who can log in. | The provider's grants list who can run commands. |

The relay stays a router. It holds no cloud credentials and no queue.

## 2. Terms

| Term | Meaning |
| --- | --- |
| Caller | The identity that sends an exec request. |
| Citizen | The identity that serves the exec capability. |
| Manager | A caller that sends work to many citizens. |
| Machine | The computer that runs the citizen. A Fly.io Sprite is one example. |
| Pause | The platform suspends an idle machine. |
| Wake | An action that makes a paused machine run again. |
| Wake hook | An entry on the caller that tells the caller how to wake one machine. |
| Start script | A program on the machine that prepares the citizen after a wake. |
| Lease | A hold with an expiry. While a lease is live, the machine does not pause. |
| Platform service | A process that the platform starts at boot and restarts after a crash. |
| Plain process | A process that is not a platform service. |

## 3. Roles

| Role | Where it runs | Presence |
| --- | --- | --- |
| Relay | A long-running server | Always online |
| Mailbox provider | A long-running server | Always online |
| Manager | A long-running identity. Agent sessions start and stop. | Online. It reads its mailbox. |
| Citizen | A machine that pauses, for example a Sprite | Online only while awake |

A manager sends work to citizens. A citizen runs the work and returns the
result. A citizen has its own identity, its own grants, and its own
agent instructions.

## 4. What ARC already gives exec

- Every request reaches the provider with the caller's public key in `from`.
  The exec runtime verifies the signature first.
- The relay forwards ciphertext. It cannot read commands or output.
- The exec runtime supports request/reply, long-lived streams, and events
  that a provider starts. See `provider/command.go`.
- `arc call` waits for one reply for `--timeout` seconds. The default is 30.
- A DM mailbox stores sealed messages for a key that is offline. See
  [DM](../dm/SPEC.md).

## 5. What ARC does not give exec

- The relay drops a packet when the recipient is offline. There is no queue.
- Disconnection and expiry remove a relay announcement. An announcement
  expires at most 180 seconds after issue. See
  [discovery](../discovery/SPEC.md). The relay cannot show a paused citizen.
- `arc call` does not wake a peer and does not wait for a peer to connect.
- `arc serve` does not detect a pause of its machine.
- `arc serve` connects to the relay one time. If that connection fails or
  ends, `arc serve` exits with status 1.
- A request timeout does not prove that the command did not run. The caller
  must not retry an arbitrary command automatically.

## 6. What a Sprite does

The first citizen machines are Fly.io Sprites. These facts set the shape of
the design. There are three sources:

- The Fly.io guide
  [Keeping a Sprite Running](https://docs.sprites.dev/keeping-sprites-running/).
- The documents on the Sprite: `/.sprite/docs/services.md` and
  `/.sprite/docs/agent-context.md`, runtime version `0.0.1-rc48`.
- The measurements in section 19.

### 6.1 Pause

- A Sprite pauses in two stages: `warm`, then `cold`.
- In the `warm` stage, the platform suspends the machine. Processes keep
  their state. The compute charge stops.
- In the `cold` stage, the platform drops the memory. All processes stop.
- A pause drops each open TCP connection. This is true in the two stages.
- As a result, a relay connection does not survive a pause. Relay traffic
  cannot wake a Sprite.
- The guide does not say when a `warm` Sprite becomes `cold`.

### 6.2 Wake

- A call to the Sprites API wakes a Sprite. `sprite exec` and `sprite proxy`
  are examples.
- An HTTP request to the Sprite URL wakes a Sprite.
- The guide gives a wake time of 100 to 500 milliseconds from `warm`. It gives
  1 to 2 seconds from `cold`.
- After a wake from `warm`, plain processes continue. After a wake from
  `cold`, only platform services start again.

### 6.3 Stay awake

- A Sprite runs while it services an HTTP request.
- A Sprite runs while a session writes output.
- A Sprite runs while a task of the Tasks API is live. A task is a named hold
  with an expiry.
- The maximum expiry of a task is 1 hour. A refresh moves the expiry. An
  expired task releases the Sprite.
- The Tasks API listens on the socket `/.sprite/api.sock`. The command
  `sprite-env curl` sends a request to this socket.
- A plain process does not keep a Sprite awake.

### 6.4 Platform services

- The documents on the Sprite say that a running platform service blocks the
  pause. The measurements agree.
- The Fly.io guide says that a platform service with the URL wake uses no
  compute while the Sprite is idle.
- These two sources disagree. The design must work in the two cases.
- One platform service can own the HTTP port. The documents say that the
  platform starts a stopped service when a request arrives. No test
  confirmed this behavior.

### 6.5 Price

- Compute costs 0.07 USD for each vCPU-hour and 0.04375 USD for each GB-hour
  of memory. The platform charges for each second.
- A Sprite that never pauses, with 1.6 GB of memory in use, costs about
  50 USD for each month.

## 7. Design rules

1. The relay routes packets. It holds no cloud credentials and no queue.
2. The caller wakes the machine. The wake hook uses the caller's own cloud
   credentials.
3. The citizen keeps its machine awake while a command runs. It uses a lease.
4. No process of the citizen runs as a platform service that stays active.
5. `arc serve` runs only while a lease is live. A live lease proves that the
   machine did not pause.
6. If no lease is live, a pause is possible. Then the start script starts a
   new `arc serve` with a new relay connection.
7. The relay runs on a different machine that does not pause.

CAUTION: Do not run `arc serve` or `sshd` as a platform service on a citizen
machine. A running platform service blocks the pause, and the machine costs
about 50 USD for each month.

## 8. Command request (prototype)

The provider bundle is `cmd/exec-provider`. Its scheme is `exec`. Its method is
`EXEC`.

```sh
arc call 'exec+arc://<citizen-public-key>/' \
  '{"script":"cd ~/arc && git status --short"}'
```

The request body is UTF-8 JSON. It contains exactly one of `argv` or `script`.

| Field | Meaning |
| --- | --- |
| `argv` | A list of strings. The provider runs it without a shell. |
| `script` | A string. The provider runs it with `bash -lc`. |
| `cwd` | Optional. A directory relative to the configured `cwd`. |
| `stdin` | Optional. Text that the provider writes to standard input. |
| `timeout_ms` | Optional. The configured limit caps this value. |

The reply is UTF-8 JSON:

```json
{"exit":0,"stdout":"...","stderr":"...","timed_out":false,"truncated":false}
```

Rules:

- If the caller key is not in `grants`, the provider returns `access_denied`.
  The command does not run.
- A granted key runs commands as the provider's operating-system user.
  A grant is equal to a shell login.
- The provider runs each request in its own thread. A slow command does not
  block other callers.
- On timeout, the provider stops the whole process group. The reply sets
  `timed_out` to `true`.
- Standard output and standard error share one output budget. The provider
  cuts the output at the budget and sets `truncated` to `true`.
- The timeout limit is at most 115 seconds. `arc call` waits 30 seconds by
  default. For a longer command, the caller passes a larger `--timeout`, for
  example `--timeout 120`.

The operator writes `EXEC_CONFIG`, an absolute path to a JSON file:

```json
{
  "grants": ["<64 lowercase hex characters>"],
  "cwd": "/home/sprite",
  "limits": {"body_bytes": 262144, "output_bytes": 1048576, "timeout_ms": 60000}
}
```

The provider does not start if the configuration is not valid.

## 9. Presence states

A caller sees a citizen in one of three states:

| State | Condition |
| --- | --- |
| `online` | The relay has a current announcement for the key. |
| `asleep` | No current announcement. The caller has a wake hook for the key. |
| `offline` | No current announcement. The caller has no wake hook. |

The caller calculates `asleep` from its own wake configuration. The relay does
not change.

Note: After a pause, the relay can hold the last announcement until it
expires. Thus `online` does not prove that the machine is awake.

## 10. Wake

### 10.1 Wake configuration

The caller keeps wake hooks in a local file, one hook for each citizen key.
This file has the same role as `ProxyCommand` in `~/.ssh/config`.

```toml
[wake."<citizen-public-key>"]
kind = "command"
argv = ["sprite", "exec", "-s", "<sprite-name>", "--", "/home/sprite/exec-provider/citizen/citizen-up"]
```

- The `command` kind runs a local program. Exit status 0 means that the
  citizen is ready.
- In this example, `sprite exec` wakes the Sprite and runs the start script.
- The caller's own cloud credentials authorize the wake. The file stores
  no token.
- Other platforms use the same kind with a different program. ARC does not
  change.

### 10.2 Wake flow

1. If the caller has no wake hook for the citizen, the caller uses the
   presence state from the relay.
   - If the state is `online`, the caller sends the request.
   - If the state is not `online`, the caller fails with `peer_offline`.
2. If the last reply from the citizen arrived less than 30 seconds ago, the
   caller skips the wake hook.
3. If not, the caller runs the wake hook. The caller also runs the hook when
   the relay shows `online`.
4. If the wake hook exits with a status that is not 0, the caller fails with
   `wake_failed`.
5. If the hook kind is `command`, exit status 0 means that the citizen is
   ready. For another kind, the caller polls the relay until a current
   announcement exists.
6. If the wake timeout ends, the caller fails with `wake_timeout`.
7. The caller sends the request one time. The caller does not retry the
   command.

The default wake timeout is 30 seconds. It includes the time of the wake hook.

The wake hook is the only reliable sign that the machine is awake. The relay
can show `online` for a paused machine (section 9). On an awake machine, the
start script only refreshes the lease, so the hook is fast.

### 10.3 Platform and lease script

The bundle supports two platforms. The setting `CITIZEN_PLATFORM` in
`citizen.env` selects the platform.

| Platform | Machine | Lease |
| --- | --- | --- |
| `sprite` | A Fly.io Sprite. It pauses. | The task `arc` of the Sprites Tasks API |
| `none` | A machine that never pauses | None. Each lease command does nothing. |

The script `citizen/lease` gives one interface for the two platforms:

| Command | Action |
| --- | --- |
| `lease hold SECONDS` | Create or refresh the lease. |
| `lease create SECONDS` | Create the lease. Exit with status 3 if the lease exists. |
| `lease delete` | Delete the lease. |
| `lease pauses` | Exit with status 0 if the platform can pause the machine. |

To add a platform, add one case to `citizen/lease`. The provider, the start
script, and the caller do not change.

### 10.4 Start script

The start script is `citizen/citizen-up` in the bundle. It runs on the
machine. It does these steps:

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
4. The start script waits until the relay has the announcement of the
   citizen. It reads the relay directory with the probe identity. Then it
   exits with status 0.
5. If the relay has no announcement after 20 seconds, the start script
   deletes the lease. Then it exits with status 1.

A refresh to 120 seconds can shorten the lease of a running command. This is
safe. The provider refreshes the lease again in 60 seconds or less
(section 11).

If no request arrives, the lease expires after 120 seconds. Then the machine
pauses.

The citizen machine holds two identities:

- The citizen identity serves the capability.
- The probe identity reads the relay directory for the readiness check.

A relay keeps one connection for each identity. A query with the citizen key
replaces the relay connection of `arc serve`, and the announcement of the
citizen goes away. `citizen/init` creates the probe identity.

`arc serve` connects to the relay one time, at startup. After a wake the
network needs a moment, so `citizen/serve` waits for the relay first.

## 11. Lease in the provider

The provider keeps the machine awake while a command runs. The operator adds
a `lease` object to `EXEC_CONFIG`:

```json
{
  "grants": ["<64 lowercase hex characters>"],
  "cwd": "/home/sprite",
  "lease": {
    "hold": ["/home/sprite/exec-provider/citizen/lease", "hold", "300"],
    "release": ["/home/sprite/exec-provider/citizen/lease", "hold", "60"],
    "interval_ms": 60000
  }
}
```

Rules:

- If the configuration has no `lease` object, the provider runs no lease
  command. This is correct for a machine that does not pause.
- When the first command starts, the provider runs `hold`.
- While one or more commands run, the provider runs `hold` again after each
  `interval_ms`.
- When the last command ends, the provider runs `release`.
- On a Sprite, `release` sets an expiry of 60 seconds. It does not delete the
  lease. A caller can then send the next request without a new wake.
- The 60-second expiry is the reason for the 30-second rule in section 10.2.
- If a lease command fails, the provider writes the error to its log. The
  command of the caller continues.
- If the provider stops without a `release`, the lease expires after
  300 seconds at most. Then the machine can pause.

The `hold` and `release` values are programs of the platform. The provider
has no code that is specific to Sprites.

## 12. Asynchronous jobs

A command that takes longer than 115 seconds needs a job. An agent turn is
an example. A job continues after the caller disconnects.

### 12.1 Actions

The body field `action` selects the operation. The method stays `EXEC`.

| `action` | Body | Reply |
| --- | --- | --- |
| `run` (default) | The fields of section 8 | The result of section 8 |
| `start` | The fields of section 8 | `{"job":"<id>","state":"running"}`, at once |
| `status` | `{"action":"status","job":"<id>"}` | The state and the output of the job |

```sh
arc call 'exec+arc://<citizen-public-key>/' \
  '{"action":"start","script":"cd ~/arc && go test ./..."}'
arc call 'exec+arc://<citizen-public-key>/' \
  '{"action":"status","job":"01a0bb786e86-6325d28e"}'
```

The `status` reply is UTF-8 JSON:

```json
{"job":"...","state":"done","started_at":"...","ended_at":"...","exit":0,"timed_out":false,"stdout":"...","stderr":"...","truncated":false}
```

| `state` | Meaning |
| --- | --- |
| `running` | The process runs. |
| `done` | The process ended. The reply has `exit`, `timed_out`, and `ended_at`. |
| `lost` | The process ended, but no provider recorded the result within 5 seconds. The provider stopped during the job. |

### 12.2 Rules

- The caller that starts a job owns the job. Only the owner can read the
  status.
- If another key asks for the job, the provider returns `not_found`. The
  reply does not show that the job exists.
- The limit `job_timeout_ms` caps the time of a job. The default is
  1 hour. The maximum is 24 hours.
- On timeout, the provider stops the whole process group of the job.
- The job holds the lease of section 11 from the start to the end of the job.
- The provider writes the output of the job to files. The `status` reply
  gives the end of each file within the output budget. The end of the output
  holds the result of most jobs.
- The provider keeps each job in its own directory below `jobs_dir`. The
  default is `~/.arc/exec/jobs`. The provider does not delete old jobs.
- The wake flow of section 10.2 applies to `start` and to `status`.

### 12.3 Result to the caller

When a job ends, the provider runs the notify command of the operator. The
operator adds a `notify` object to `EXEC_CONFIG`:

```json
{
  "notify": {
    "argv": ["/home/sprite/exec-provider/citizen/notify-dm", "{owner}"],
    "timeout_ms": 30000
  }
}
```

Rules:

- If the configuration has no `notify` object, the provider runs no command.
- In `argv`, the provider replaces `{owner}` with the public key of the
  caller that started the job.
- The provider writes the `status` reply of the job to the standard input of
  the command.
- The provider runs the command before it releases the lease. The machine
  stays awake until the caller has the result.
- If the command fails, the provider writes the error to its log. The state
  of the job does not change.
- The default timeout is 30 seconds. The maximum is 300 seconds.

### 12.4 Result in the mailbox

The script `citizen/notify-dm` sends the result to the mailbox of the caller:

```sh
arc dm send <owner-public-key>
```

The DM body is the `status` reply of the job. The DM is sealed to the key of
the caller before it leaves the machine.

Requirements:

- The mailbox provider runs on a long-running server. The relay does not
  store the result.
- The citizen installs the DM tool one time:
  `arc install <dm-provider-public-key> primary --yes`.
- `citizen/init --notify-dm` writes the `notify` object for this script.

The caller reads the mailbox when it is active. The caller does not need to
be online when the job ends.

## 13. SSH access to a machine that pauses

Some tools speak SSH only. Claude Desktop is an example. The same design
rules apply: `sshd` runs as a plain process, and a lease covers each session.

```
Host <sprite-name>
  User sprite
  ProxyCommand sh -c '~/.local/bin/sprite exec -s %h -- /home/sprite/bin/sshd-up >/dev/null 2>&1 && exec ~/.local/bin/sprite proxy -s %h -W 22'
```

The program `/home/sprite/bin/sshd-up` does these steps:

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

## 14. Wake URL and dormant record

This section removes the need for local wake configuration and for the
Sprites CLI on the caller.

### 14.1 Wake URL

- A small wake service owns the HTTP port of the machine. It stays stopped
  while the machine is idle.
- An HTTP request to the Sprite URL wakes the Sprite. The platform starts the
  wake service (section 6.4).
- The wake service runs the start script. It replies after the start script
  exits. Then it stops itself.

```toml
[wake."<citizen-public-key>"]
kind = "https"
url = "https://<sprite-name>-<org>.sprites.app/wake"
token_env = "SPRITES_TOKEN"
```

- With the default URL authentication, the request needs a Sprites token.
  The caller reads the token from the environment variable that `token_env`
  names. The file stores no token.
- With public URL authentication, each host on the Internet can wake the
  machine. The wake service must then check a wake token.
- A granted key signs the wake token. The token holds a time. The wake
  service refuses a token that is older than 120 seconds.
- If the wake token is not valid, the wake service replies with status 403.
  It does not run the start script.

### 14.2 Dormant record

- Before the citizen releases its last lease, it sends a signed dormant
  record to the relay.
- The record holds the public key, the wake URL, the issue time, and an
  expiry.
- The relay keeps the record after the connection drops. A lookup of the key
  returns the record.
- As a result, each caller sees `asleep` without local configuration.
- The relay does not call the wake URL. The caller calls it.

## 15. Security

- A grant gives full command access as the provider's user. Give grants only
  to keys that you trust with a shell.
- Grants are per provider. A manager with a grant on one citizen has no access
  to another citizen.
- The wake hook uses the caller's cloud credentials. The relay and the
  citizen never receive them.
- A wake costs money. A key without a wake hook cannot wake a citizen.
- A lease costs money while it is live. Each lease has an expiry, so a
  provider that stops cannot keep the machine awake.
- A public wake URL lets each host on the Internet wake the machine for some
  seconds. Use the default URL authentication unless you accept this cost.
- The provider sees every command and all output. It is not a private-compute
  boundary. See [private environments](../private-environment/SPEC.md).

## 16. Non-goals

- Interactive terminals. `shell+arc://` with a PTY is separate work.
- TCP forwarding. `tcp+arc://` is separate work.
- A relay-side queue.
- A wake that the relay does.
- Automatic retry of commands.

## 17. Open questions

- Does an open `sprite proxy` session keep a Sprite awake without a lease?
- When does a `warm` Sprite become `cold`?
- How fast does the relay see a connection that a pause dropped?
- Does the platform start a stopped HTTP service after a wake from `cold`?
  Does it start a platform service again after an exit with status 0?
- `arc serve` can detect a pause itself. A timer tick that arrives late shows
  a pause. Then `arc serve` connects to the relay again at once. This also
  helps a laptop that sleeps. The start script then keeps the old process.
- Where does the wake configuration file live, and does `arc lists` own it?
- The dormant record needs a lifetime that survives disconnection and relay
  restart.

## 18. Phases

| Phase | Scope |
| --- | --- |
| 0 | Prototype provider, request/reply, grants. Done. |
| 1 | Start script, lease in the provider, and a wrapper script on the caller that runs the wake flow. No change to ARC core. Done: `cmd/exec-provider/citizen/`, the `lease` object, and `cmd/exec-provider/arc-exec`. |
| 2 | Wake hooks and the `asleep` state in `arc call`. |
| 3a | Asynchronous jobs: `start`, `status`, and `arc-exec --start`, `--status`, `--wait`. Done. |
| 3b | The notify command (section 12.3) and the DM script (section 12.4). |
| 4 | Wake URL and signed dormant records on the relay. |

## 19. Verified

Measured on 2026-09-19 with one Fly.io Sprite (8 CPUs, 8 GB, runtime
`0.0.1-rc48`) and the prototype provider. The relay ran on a Fly.io Machine
(`shared-cpu-1x`, 512 MB, region `ams`), unless the row says differently.

To detect a pause, a loop on the Sprite wrote the time each second. A gap
between two times shows a pause.

| Check | Result |
| --- | --- |
| Command round trip, Mac to Sprite, relay on the Sprite, machine awake | 1 second |
| Command round trip, Mac to Sprite, machine awake | 0.7 seconds |
| Request from a key without a grant | `access_denied`. A unit test shows that the command does not run. |
| `claude -p` turn, then `--resume` of the same session | Both completed. The first turn reported about 10 seconds of API time. |
| Two platform services active (`sshd` and `arc serve`), 8 minutes idle | No pause |
| One platform service active (`sshd`), 4 minutes idle | No pause |
| No platform service active, idle | The first stall came 8 seconds after the last session ended. The full pause came after 22 seconds. |
| Plain process across a pause of 229 seconds | The process continued after the wake |
| `sprite exec` to the paused Sprite | Command output after 0.7 seconds |
| Lease of 180 seconds, no platform service, idle | No pause while the lease was live |
| Lease expiry, then idle | The pause started at the second of the lease expiry |
| `arc-exec`, paused Sprite, wake hook, start script, request, reply | 2.2 seconds. The wake hook took 1.3 seconds. |
| `arc-exec` less than 30 seconds after a reply | 0.8 seconds. No wake hook. |
| `arc-exec` with a live lease, reply older than 30 seconds | 1.4 seconds. The start script kept `arc serve`. |
| Job of 150 seconds, caller disconnected after `start` | `start` replied in 0.7 seconds. The job ran from 21:00:27 to 21:02:57 UTC, so the Sprite did not pause. |
| `status` of the job, 24 seconds after the job ended | `done`, exit 0, full output. The start script kept `arc serve`. |
| `claude -p` turn as a job with `arc-exec --wait`, at the same time as the job of 150 seconds | Correct answer after 12 seconds |
| `status` field of the Sprites API | It showed `cold` for 2 to 3 seconds at a time while platform services were active. It is not a reliable sign of a pause. |

Not verified:

- A wake from the `cold` stage. The test did not show which stage the pause
  reached.
- The start of a stopped HTTP service on a request.
- SSH access through the design of section 13.
