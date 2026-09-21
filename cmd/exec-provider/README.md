# Exec over ARC provider

This bundle runs commands on a citizen machine through ARC request/reply
transport. The caller's public key is the login. The relay carries the
traffic, so the machine needs no open port.

The machine can pause when it is idle. The caller wakes the machine before
each request, and the machine holds a lease while it works. See
[the exec spec](../../docs/exec/SPEC.md) for the design.

Status: experimental.

WARNING: A grant equals a shell login. A granted key runs any command as the
operating-system user of the provider. It can read and change every file of
that user.

## Contents

| File | Purpose |
| --- | --- |
| `*.go` | The provider, in Go. An ARC release holds its binary, `exec-provider`. |
| `run.sh` | Runs the binary that `EXEC_PROVIDER` names. Without it, builds the provider from this checkout. |
| `citizen/init` | Writes `citizen.env` and `config.json` for one machine. |
| `citizen/citizen-up` | The start script. The caller runs it to wake the citizen. |
| `citizen/serve` | Runs `arc serve` for this bundle as a plain process. |
| `citizen/lease` | Holds the machine awake. One case for each platform. |
| `citizen/notify-dm` | Sends the result of a finished job to its owner as a direct message. |
| `arc-exec` | The caller wrapper. It wakes the citizen, then sends the request. |

## Requirements

- On the citizen machine: `arc` and `exec-provider` from an ARC release,
  `bash`, and a copy of this directory. The machine needs no Go toolchain.
- On the caller: `arc` and Python 3.11 or newer.
- A relay on a machine that does not pause. The citizen and the caller use
  the same relay.

## Set up a citizen

Do these steps on the citizen machine.

1. Install ARC. The release puts `exec-provider` in the same directory as
   `arc`:

   ```sh
   curl -fsSL https://raw.githubusercontent.com/gezibash/arc/main/install.sh | sh
   ```

2. Copy this directory to the machine, for example to `~/exec-provider`.
3. Make a key for the citizen. The command prints the name, then the public
   key. Record the name:

   ```sh
   arc keys gen
   ```

4. Write the configuration. Give one `--grant` for each caller key:

   ```sh
   ~/exec-provider/citizen/init \
     --key <citizen-key-name> \
     --relay <relay-host>:7331 \
     --relay-pubkey <relay-public-key> \
     --grant <caller-public-key> \
     --platform sprite
   ```

   Use `--platform sprite` on a Fly.io Sprite. Use `--platform none` on a
   machine that never pauses. The script prints the public key of the citizen.

   The script finds `exec-provider` in the directory of `arc`, and writes its
   path to `citizen.env`. To use another binary, set `EXEC_PROVIDER` before
   you run the script.

   Add `--notify-dm` to send the result of each finished job to its owner as
   a direct message. Install the DM tool on the citizen first:

   ```sh
   arc install <dm-provider-public-key> --yes
   ```

5. Start the citizen one time to test the configuration:

   ```sh
   ~/exec-provider/citizen/citizen-up && echo ready
   ```

CAUTION: On a Sprite, do not run `citizen/serve` or `sshd` as a Sprite
service. A running service stops the pause, and the Sprite costs compute all
the time.

## Set up a caller

1. Add a wake hook for the citizen to `~/.config/arc/wake.toml`. The hook runs
   the start script on the citizen machine.

   For a Sprite:

   ```toml
   [wake."<citizen-public-key>"]
   kind = "command"
   argv = ["sprite", "exec", "-s", "<sprite-name>", "--", "/home/sprite/exec-provider/citizen/citizen-up"]
   ```

   For a machine that you reach with SSH:

   ```toml
   [wake."<citizen-public-key>"]
   kind = "command"
   argv = ["ssh", "<host>", "~/exec-provider/citizen/citizen-up"]
   ```

   If there is no wake hook, `arc-exec` sends the request without a wake.

2. Set the relay for `arc`:

   ```sh
   export ARC_RELAY=<relay-host>:7331
   export ARC_RELAY_PUBKEY=<relay-public-key>
   ```

## Run commands

```sh
arc-exec <citizen-public-key> --script 'cd ~/arc && git status --short'
arc-exec <citizen-public-key> -- uname -a
```

`arc-exec` writes the output of the command and exits with its exit code.
Add `-v` to log each step with a time.

A command that takes longer than 115 seconds must run as a job. A job
continues after the caller disconnects:

```sh
arc-exec <citizen-public-key> --start --script 'cd ~/arc && mix test'
arc-exec <citizen-public-key> --status <job>
arc-exec <citizen-public-key> --wait --script 'cd ~/arc && mix test'
```

`--status` exits with status 75 while the job runs. After the job ends, it
exits with the exit code of the job.

## Request protocol

The provider accepts ARC exec request events with `meta.method` `EXEC`. The
body is UTF-8 JSON. The body field `action` selects the operation:

| `action` | Body | Reply |
| --- | --- | --- |
| `run` (default) | `argv` or `script`, and optional `cwd`, `stdin`, `timeout_ms` | `exit`, `stdout`, `stderr`, `timed_out`, `truncated` |
| `start` | The same fields as `run` | `job` and `state` |
| `status` | `job` | `state`, the output, and the result after the job ends |

Without `arc-exec`, send a request with `arc call`:

```sh
arc call 'exec+arc://<citizen-public-key>/' '{"argv":["uname","-a"]}'
```

## Configuration

`citizen/init` writes `config.json`. `EXEC_CONFIG` gives its absolute path.

| Field | Meaning |
| --- | --- |
| `grants` | The caller keys that can run commands |
| `cwd` | The default directory for commands |
| `limits.body_bytes` | Largest request body. Default 256 KiB. Maximum 1 MiB. |
| `limits.output_bytes` | Output budget for one reply. Default 1 MiB. Maximum 4 MiB. |
| `limits.timeout_ms` | Time limit for `run`. Default 60 seconds. Maximum 115 seconds. |
| `limits.job_timeout_ms` | Time limit for a job. Default 1 hour. Maximum 24 hours. |
| `lease` | The `hold` and `release` commands, and `interval_ms`. `init` writes it for a platform that pauses. |
| `notify` | The `argv` of a command that runs when a job ends, and an optional `timeout_ms`. `{owner}` becomes the caller key. The result goes to standard input. |
| `jobs_dir` | The directory for job output. Default `~/.arc/exec/jobs`. |

The provider does not start if the configuration is not valid.

## Tests

From the repository root:

```sh
go test ./cmd/exec-provider
```

## Limits

- The provider does not delete old jobs. Delete old directories in
  `jobs_dir` by hand.
- The provider sees every command and all output. It is not a private-compute
  boundary.
