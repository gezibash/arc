# Exec app

The manifest generates caller commands. `server/` owns process policy and
execution. `cmd/arc-exec` is the executable entry point.

This bundle runs commands on a citizen machine through ARC request/reply
transport. The caller's public key is the login. The relay carries the
traffic, so the machine needs no open port.

The machine can pause when it is idle. The caller wakes the machine before
each request, and the machine holds a lease while it works. See
[the wake spec](../../docs/wake/SPEC.md) for the design of the wake.

Status: experimental. Request/reply, grants, the lease, jobs and the notify
command are built.

WARNING: A grant equals a shell login. A granted key runs any command as the
operating-system user of the service. It can read and change every file of
that user.

## Contents

| File | Purpose |
| --- | --- |
| `server/*.go` | The service implementation, in Go. An ARC release holds its binary, `arc-exec`. |
| `run.sh` | Runs the binary that `EXEC_PROVIDER` names. Without it, builds the service from this checkout. |
| `manifest.json` | The interface that the citizen announces. |

This directory holds no start script and no lease program. The operator
supplies them for the platform of the machine. The wake spec states what each
one must do, in sections 3.6 and 3.7.

## Requirements

- On the citizen machine: `arc` and `arc-exec` from an ARC release, and a
  copy of `manifest.json`. The machine needs no Go toolchain.
- On the caller: `arc`.
- A Nostr relay on a machine that does not pause. The citizen and the caller
  use the same relay.

## Set up a citizen

Do these steps on the citizen machine.

1. Install ARC. The release puts `arc-exec` in the same directory as
   `arc`:

   ```sh
   curl -fsSL https://raw.githubusercontent.com/gezibash/arc/main/install.sh | sh
   ```

2. Copy `manifest.json` to the machine, for example to `~/arc-exec`.
3. Make an identity for the citizen, and add the relay. The first command
   prints the name, then the public key:

   ```sh
   arc keys gen
   arc relay add wss://<relay-host>
   ```

4. Write the configuration to `~/arc-exec/config.json`. Give one grant for
   each caller key. [Configuration](#configuration) lists each field:

   ```json
   {
     "grants": ["<caller-public-key>"],
     "cwd": "/home/<user>"
   }
   ```

   On a machine that pauses, add the `lease` object, see [Lease](#lease).
   To send the result of each finished job to its owner, add the `notify`
   object, see [Jobs](#jobs).

5. Start the citizen one time to test the configuration:

   ```sh
   EXEC_CONFIG=~/arc-exec/config.json \
     arc serve "exec://$(command -v arc-exec)?manifest=$HOME/arc-exec/manifest.json"
   ```

   `arc serve` writes `serves` when at least one relay holds the
   announcement and listens for calls. If a relay does not take the watch,
   `arc serve` logs it and tries again.

6. On a machine that pauses, write a start script that runs this command.
   Section 3.7 of the wake spec lists its steps.

CAUTION: On a machine that pauses, do not run `arc serve` or `sshd` as a
platform service. A running platform service blocks the pause, and the
machine costs compute all the time.

## Set up a caller

1. Add a wake hook for the citizen to `wake.toml` in the arc home, by
   default `~/.config/arc/wake.toml`. The hook runs the start script on
   the citizen machine. Before each live call to the citizen, `arc` runs the
   hook: `arc call`, and the commands that `arc install` adds. The hook
   runs a program of the platform that wakes the machine, with your own
   cloud credentials, and runs the start script. See section 3.4 of the wake
   spec.

   For a machine that you reach with SSH:

   ```toml
   [wake."<citizen-public-key>"]
   kind = "command"
   argv = ["ssh", "<host>", "<start-script>"]
   ```

   The wake follows these rules:

   - `arc serve` signs its announcement again every 2 minutes. An
     announcement is current while it is at most 5 minutes old.
   - If there is no wake hook, the citizen must have a current announcement
     on the relay. If it has none, the call stops at once with
     `peer_offline`.
   - After an answer from the citizen, `arc` skips the hook for 30 seconds.
     A citizen that answered did not pause.
   - If the hook exits with a status other than 0, the call stops with
     `wake_failed`. If the hook runs longer than 30 seconds, the call stops
     with `wake_timeout`.

2. Add the relay of the citizen, and install the capability:

   ```sh
   arc relay add wss://<relay-host>
   arc install <citizen-public-key> --yes
   ```

## Run commands

After `arc install <citizen-public-key>`, the capability adds three commands:

```sh
arc exec run uname -a
arc exec run sh -c 'cd ~/arc && git status --short'
arc exec start 'cd ~/arc && go test ./...'
arc exec status <job>
```

`run` is a live call. It writes the output of the command, and exits with the
exit code of the command. A code that is missing, below 0 or above 255 gives
the status 1. An error of `arc`, for example no relay, also exits 1, and
writes a message that starts with `arc:`.

A command that takes longer than 115 seconds must run as a job. A job
continues after the caller disconnects. `start` is a store-and-forward call:
it waits in the outbox, and the reply names the job. Run `arc sync`, then
`arc call results` shows the job. `status` shows the state of the job, and
its output after it ends. It exits 75 while the job runs, 1 when the job is
lost, and with the exit code of the job after it ends.

## Request protocol

The service accepts ARC exec request events with `meta.method` `EXEC`. Its
scheme is `exec`. The body is UTF-8 JSON. The body field `action` selects the
operation:

| `action` | Body | Reply |
| --- | --- | --- |
| `run` (default) | `argv` or `script`, and optional `cwd`, `stdin`, `timeout_ms` | `exit`, `stdout`, `stderr`, `timed_out`, `truncated` |
| `start` | The same fields as `run` | `job` and `state` |
| `status` | `job` | `state`, the output, and the result after the job ends |

With `arc`, send a request with `arc call`:

```sh
arc call 'exec+arc://<citizen-public-key>/' '{"argv":["uname","-a"]}'
```

```sh
arc call 'exec+arc://<citizen-public-key>/' \
  '{"script":"cd ~/arc && git status --short"}'
```

`arc call` shows the reply as `arc exec` does: the output of the command, the
job of `start`, and the exit status of the command, or 75 while a job runs.
`--raw` writes the reply JSON as it came.

A `run` or `start` body contains exactly one of `argv` or `script`:

| Field | Meaning |
| --- | --- |
| `argv` | A list of strings. The provider runs it without a shell. |
| `script` | A string. The provider runs it with `bash -lc`. |
| `cwd` | Optional. A directory relative to the configured `cwd`. |
| `stdin` | Optional. Text that the provider writes to standard input. |
| `timeout_ms` | Optional. The configured limit caps this value. |

The reply of `run` is UTF-8 JSON:

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
- The timeout limit is at most 115 seconds. `arc call` has a 120-second
  budget by default; `--timeout` can shorten it. Caller cancellation and
  deadlines also stop the command group. Detached jobs keep their own timeout.
- A request timeout does not prove that the command did not run. The caller
  must not retry an arbitrary command automatically.

## Configuration

The operator writes `config.json`. `EXEC_CONFIG` gives its absolute path:

```json
{
  "grants": ["<64 lowercase hex characters>"],
  "cwd": "/home/<user>",
  "limits": {"body_bytes": 262144, "output_bytes": 1048576, "timeout_ms": 60000}
}
```

| Field | Meaning |
| --- | --- |
| `grants` | The caller keys that can run commands |
| `cwd` | The default directory for commands |
| `limits.body_bytes` | Largest request body. Default 256 KiB. Maximum 1 MiB. |
| `limits.output_bytes` | Output budget for one reply. Default 1 MiB. Maximum 4 MiB. |
| `limits.timeout_ms` | Time limit for `run`. Default 60 seconds. Maximum 115 seconds. |
| `limits.job_timeout_ms` | Time limit for a job. Default 1 hour. Maximum 24 hours. |
| `lease` | The `hold` and `release` commands, and `interval_ms`. Add it on a machine that pauses. See [Lease](#lease). |
| `notify` | The `argv` of a command that runs when a job ends, and an optional `timeout_ms`. `{owner}` becomes the caller key. The result goes to standard input. |
| `jobs_dir` | The directory for job output. Default `~/.arc/exec/jobs`. |

The service does not start if the configuration is not valid.

## Lease

The provider keeps the machine awake while a command runs. The operator adds
a `lease` object to `EXEC_CONFIG`:

```json
{
  "grants": ["<64 lowercase hex characters>"],
  "cwd": "/home/<user>",
  "lease": {
    "hold": ["/home/<user>/bin/lease", "hold", "300"],
    "release": ["/home/<user>/bin/lease", "hold", "60"],
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
- In this example, `release` sets an expiry of 60 seconds. It does not delete
  the lease. A caller can then send the next request without a new wake. The
  60-second expiry is the reason for the 30-second rule of the wake flow, see
  section 3.5 of the wake spec.
- If a lease command fails, the provider writes the error to its log. The
  command of the caller continues.
- If the provider stops without a `release`, the lease expires after
  300 seconds at most. Then the machine can pause.

The `hold` and `release` values are programs of the platform. The provider
has no code that is specific to a platform. The wake spec, section 3.6,
states what the lease program must do.

## Jobs

A command that takes longer than 115 seconds needs a job. An agent turn is
an example. A job continues after the caller disconnects.

The body field `action` selects the operation, see
[Request protocol](#request-protocol). The installed commands
`arc exec start` and `arc exec status` send these bodies. With `arc call`:

```sh
arc call 'exec+arc://<citizen-public-key>/' \
  '{"action":"start","script":"cd ~/arc && go test ./..."}'
arc call 'exec+arc://<citizen-public-key>/' \
  '{"action":"status","job":"01a0bb786e86-6325d28e"}'
```

`start` replies at once with `{"job":"<id>","state":"running"}`. `status`
takes the body `{"action":"status","job":"<id>"}`. The `status` reply is
UTF-8 JSON:

```json
{"job":"...","state":"done","started_at":"...","ended_at":"...","exit":0,"timed_out":false,"stdout":"...","stderr":"...","truncated":false}
```

| `state` | Meaning |
| --- | --- |
| `running` | The process runs. |
| `done` | The process ended. The reply has `exit`, `timed_out`, and `ended_at`. |
| `lost` | The process ended, but no provider recorded the result within 5 seconds. The provider stopped during the job. |

Rules:

- The caller that starts a job owns the job. Only the owner can read the
  status.
- If another key asks for the job, the provider returns `not_found`. The
  reply does not show that the job exists.
- The limit `job_timeout_ms` caps the time of a job. The default is
  1 hour. The maximum is 24 hours.
- On timeout, the provider stops the whole process group of the job.
- The job holds the lease from the start to the end of the job.
- The provider writes the output of the job to files. The `status` reply
  gives the end of each file within the output budget. The end of the output
  holds the result of most jobs.
- The provider keeps each job in its own directory below `jobs_dir`. The
  default is `~/.arc/exec/jobs`. The provider does not delete old jobs.
- The wake flow applies to each live call. `arc exec start` is a
  store-and-forward call, so it does not wake the machine. The provider
  answers it when `arc serve` next syncs.

### Result to the caller

When a job ends, the provider runs the notify command of the operator. The
operator adds a `notify` object to `EXEC_CONFIG`:

```json
{
  "notify": {
    "argv": ["/home/<user>/bin/notify", "{owner}"],
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

A notify command can send the result to the caller as a direct message. It
runs this command:

```sh
arc message send <owner-public-key>
```

The message body is the `status` reply of the job, from the standard input.
The message is a NIP-17 direct message, sealed to the key of the caller
before it leaves the machine. It waits in the outbox of the citizen until the
caller acknowledges it.

- The caller reads the result with `arc sync` and `arc message inbox` when it
  is active. The caller does not need to be online when the job ends.
- A message holds at most 32 KiB.

## Tests

From the repository root:

```sh
go test ./apps/exec/server
```

## Limits

- The service does not delete old jobs. Delete old directories in
  `jobs_dir` by hand.
- The service sees every command and all output. It is not a private-compute
  boundary. See [private environments](../../docs/proposals/private-environment.md).
- A grant gives full command access as the provider's user. Give grants only
  to keys that you trust with a shell.
- Grants are per provider. A manager with a grant on one citizen has no
  access to another citizen.
- A `shell+arc://` scheme and TCP forwarding (`tcp+arc://`) are separate
  work. Terminals use the sessions below.


## Streaming processes and terminals

Serve `manifest.json`, then use the shared session CLI:

```sh
arc session --exec --mode server_stream 'exec+arc://<service>/' \
  '{"argv":["sh","-c","echo first; sleep 1; echo second"]}'
printf 'hello\n' | arc session --exec 'exec+arc://<service>/' '{"argv":["cat"]}'
arc session --tty 'exec+arc://<service>/' '{"argv":["bash","--noprofile","--norc"]}'
```

`--exec` forwards stdin and displays stdout/stderr separately. The command exits
with the process status after core reports successful session completion.
`--tty` allocates a service PTY, enters local raw mode, forwards terminal input
(including control characters), and sends resize events. Terminal output combines
stdout and stderr. The local terminal is restored when the command ends.

Sessions accept `run` only. Initial command fields and grants remain unchanged;
`pty`, `rows`, and `cols` are additional terminal options. Input/output use bounded
NDJSON records. Input records are `stdin` with base64 `data`, and `resize` with
positive `rows`/`cols`. Output records are `stdout`, `stderr`, and final `exit`.

Existing `limits.timeout_ms` and `limits.output_bytes` apply to live processes;
requesting a longer core session does not override them. Exceeding the output
budget fails the session and stops the process group. Cancellation also stops the
process group and releases the machine lease. Pipe input EOF closes stdin; PTY
EOF sends the terminal EOF character. Detached `start`/`status` jobs retain their
existing request/reply and queued-delivery behavior.
