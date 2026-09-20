"""A small, fail-closed command execution ARC provider.

The ARC runtime sends one JSON event per input line.  Each ``EXEC`` request runs
one command as the provider's operating-system user.  Only public keys listed in
the operator's EXEC_CONFIG file may run commands.  Requests run in parallel, so
one slow command does not block other callers.

The body field ``action`` selects the operation:

  run     run the command and reply with its result (the default)
  start   start the command as a job and reply with its ID at once
  status  reply with the state and the output of one job

When a job ends, the provider runs the operator's notify command, if the
configuration has one. The result of the job goes to its standard input. This
sends the result to the caller, for example as an ARC direct message.
"""

from __future__ import annotations

import contextlib
import json
import os
import re
import secrets
import signal
import subprocess
import sys
import threading
import time
from dataclasses import dataclass
from pathlib import Path
from typing import Any


PUBLIC_KEY = re.compile(r"^[0-9a-f]{64}$")
JOB_ID = re.compile(r"^[0-9a-f]{12}-[0-9a-f]{8}$")
# A job whose process ended without a recorded result is lost after this time.
LOST_GRACE_S = 5


class ProviderError(Exception):
    """Expected errors whose text is safe to expose on ARC."""


class RequestError(ProviderError):
    pass


class AccessError(ProviderError):
    pass


DEFAULT_LIMITS = {
    "body_bytes": 256 * 1024,
    "output_bytes": 1 * 1024 * 1024,
    "timeout_ms": 60_000,
    "job_timeout_ms": 3_600_000,
}
# ARC request replies wait at most 120 seconds.  Stop the command before that
# so the caller gets the partial output and not a transport timeout.
MAX_LIMITS = {
    "body_bytes": 1 * 1024 * 1024,
    "output_bytes": 4 * 1024 * 1024,
    "timeout_ms": 115_000,
    "job_timeout_ms": 86_400_000,
}


@dataclass(frozen=True)
class LeaseConfig:
    hold: list[str]
    release: list[str]
    interval_ms: int


@dataclass(frozen=True)
class NotifyConfig:
    argv: list[str]
    timeout_ms: int


@dataclass(frozen=True)
class Config:
    grants: frozenset[str]
    cwd: Path
    limits: dict[str, int]
    lease: LeaseConfig | None = None
    notify: NotifyConfig | None = None
    jobs_dir: Path = Path.home() / ".arc" / "exec" / "jobs"


def safe_error(error: Exception) -> str:
    if isinstance(error, ProviderError):
        return str(error)
    return "internal_error"


def load_config(environ: dict[str, str] | None = None) -> Config:
    environ = os.environ if environ is None else environ
    raw_path = environ.get("EXEC_CONFIG")
    if not raw_path or not os.path.isabs(raw_path):
        raise ProviderError("EXEC_CONFIG must be an absolute path")
    path = Path(raw_path)
    if not path.is_file():
        raise ProviderError("EXEC_CONFIG must name a regular file")
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, UnicodeError, ValueError, json.JSONDecodeError):
        raise ProviderError("EXEC_CONFIG is not valid JSON") from None
    if not isinstance(data, dict) or set(data) - {"grants", "cwd", "limits", "lease", "notify", "jobs_dir"}:
        raise ProviderError("EXEC_CONFIG has unknown fields")

    grants = data.get("grants")
    if not isinstance(grants, list) or not grants:
        raise ProviderError("grants must be a non-empty list of public keys")
    if not all(isinstance(key, str) and PUBLIC_KEY.match(key) for key in grants):
        raise ProviderError("grants must contain 64-character lowercase hex keys")

    raw_cwd = data.get("cwd", str(Path.home()))
    if not isinstance(raw_cwd, str) or not os.path.isabs(raw_cwd) or not os.path.isdir(raw_cwd):
        raise ProviderError("cwd must be an absolute path to a directory")

    raw_limits = data.get("limits", {})
    if not isinstance(raw_limits, dict) or set(raw_limits) - set(DEFAULT_LIMITS):
        raise ProviderError("limits has unknown fields")
    limits = dict(DEFAULT_LIMITS)
    for name, value in raw_limits.items():
        if not isinstance(value, int) or isinstance(value, bool) or not 1 <= value <= MAX_LIMITS[name]:
            raise ProviderError(f"limits.{name} must be an integer from 1 to {MAX_LIMITS[name]}")
        limits[name] = value

    raw_jobs = data.get("jobs_dir", str(Config.jobs_dir))
    if not isinstance(raw_jobs, str) or not os.path.isabs(raw_jobs):
        raise ProviderError("jobs_dir must be an absolute path")

    return Config(
        grants=frozenset(grants),
        cwd=Path(raw_cwd),
        limits=limits,
        lease=parse_lease(data.get("lease")),
        notify=parse_notify(data.get("notify")),
        jobs_dir=Path(raw_jobs),
    )


def parse_lease(raw: Any) -> LeaseConfig | None:
    if raw is None:
        return None
    if not isinstance(raw, dict) or set(raw) != {"hold", "release", "interval_ms"}:
        raise ProviderError("lease must have exactly hold, release, and interval_ms")
    for name in ("hold", "release"):
        argv = raw[name]
        if not isinstance(argv, list) or not argv or not all(isinstance(part, str) for part in argv):
            raise ProviderError(f"lease.{name} must be a non-empty list of strings")
    interval = raw["interval_ms"]
    if not isinstance(interval, int) or isinstance(interval, bool) or not 1_000 <= interval <= 3_600_000:
        raise ProviderError("lease.interval_ms must be an integer from 1000 to 3600000")
    return LeaseConfig(hold=raw["hold"], release=raw["release"], interval_ms=interval)


def parse_notify(raw: Any) -> NotifyConfig | None:
    if raw is None:
        return None
    if not isinstance(raw, dict) or set(raw) - {"argv", "timeout_ms"} or "argv" not in raw:
        raise ProviderError("notify must have argv and an optional timeout_ms")
    argv = raw["argv"]
    if not isinstance(argv, list) or not argv or not all(isinstance(part, str) for part in argv):
        raise ProviderError("notify.argv must be a non-empty list of strings")
    timeout = raw.get("timeout_ms", 30_000)
    if not isinstance(timeout, int) or isinstance(timeout, bool) or not 1_000 <= timeout <= 300_000:
        raise ProviderError("notify.timeout_ms must be an integer from 1000 to 300000")
    return NotifyConfig(argv=argv, timeout_ms=timeout)


def run_notify(config: Config, owner: str, result: dict[str, Any]) -> None:
    """Tell the caller that a job ended. {owner} in argv becomes the caller key."""
    argv = [part.replace("{owner}", owner) for part in config.notify.argv]
    body = json.dumps(result, ensure_ascii=False, separators=(",", ":")).encode("utf-8")
    try:
        finished = subprocess.run(
            argv,
            input=body,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.PIPE,
            timeout=config.notify.timeout_ms / 1000,
        )
    except (OSError, subprocess.TimeoutExpired) as error:
        print(f"notify failed for job {result['job']}: {error}", file=sys.stderr, flush=True)
        return
    if finished.returncode != 0:
        detail = finished.stderr.decode("utf-8", errors="replace").strip()
        print(f"notify exited {finished.returncode} for job {result['job']}: {detail}", file=sys.stderr, flush=True)


def run_lease_command(argv: list[str]) -> None:
    # Stdout carries the ARC protocol, so lease output never reaches it.
    try:
        result = subprocess.run(argv, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, timeout=10)
    except (OSError, subprocess.TimeoutExpired) as error:
        print(f"lease command failed: {error}", file=sys.stderr, flush=True)
        return
    if result.returncode != 0:
        detail = result.stderr.decode("utf-8", errors="replace").strip()
        print(f"lease command exited {result.returncode}: {detail}", file=sys.stderr, flush=True)


class Lease:
    """Keeps the machine awake while one or more commands run.

    The first command runs ``hold``. A background thread runs ``hold`` again
    after each interval while commands run. The last command runs ``release``.
    Every lease command runs under one lock, so a late refresh can never follow
    a release.
    """

    def __init__(self, config: LeaseConfig, run=run_lease_command) -> None:
        self.config = config
        self.run = run
        self.active = 0
        self.changed = threading.Condition()
        threading.Thread(target=self._refresh, daemon=True).start()

    def __enter__(self) -> None:
        with self.changed:
            self.active += 1
            if self.active == 1:
                self.run(self.config.hold)
                self.changed.notify()

    def __exit__(self, *_exc: Any) -> None:
        with self.changed:
            self.active -= 1
            if self.active == 0:
                self.run(self.config.release)
                self.changed.notify()

    def _refresh(self) -> None:
        with self.changed:
            while True:
                while self.active == 0:
                    self.changed.wait()
                # Changes wake the wait early. The check below skips a refresh
                # when the last command ended in the meantime.
                self.changed.wait(self.config.interval_ms / 1000)
                if self.active > 0:
                    self.run(self.config.hold)


@dataclass(frozen=True)
class Command:
    argv: list[str]
    cwd: Path
    stdin: bytes
    timeout_ms: int


def parse_request(config: Config, message: str) -> tuple[str, Any]:
    """Return ("run", Command), ("start", Command), or ("status", job ID)."""
    if len(message.encode("utf-8")) > config.limits["body_bytes"]:
        raise RequestError("request_too_large")
    try:
        body = json.loads(message)
    except (ValueError, json.JSONDecodeError):
        raise RequestError("invalid_json") from None
    if not isinstance(body, dict):
        raise RequestError("invalid_request")
    action = body.pop("action", "run")
    if action == "status":
        if set(body) != {"job"} or not isinstance(body["job"], str) or not JOB_ID.match(body["job"]):
            raise RequestError("status needs exactly one valid job")
        return action, body["job"]
    if action not in ("run", "start"):
        raise RequestError("action must be run, start, or status")
    limit = config.limits["job_timeout_ms" if action == "start" else "timeout_ms"]
    return action, parse_command(config, body, limit)


def parse_command(config: Config, body: dict[str, Any], timeout_limit_ms: int) -> Command:
    if set(body) - {"argv", "script", "cwd", "stdin", "timeout_ms"}:
        raise RequestError("invalid_request")

    argv, script = body.get("argv"), body.get("script")
    if (argv is None) == (script is None):
        raise RequestError("give exactly one of argv or script")
    if argv is not None:
        if not isinstance(argv, list) or not argv or not all(isinstance(part, str) for part in argv):
            raise RequestError("argv must be a non-empty list of strings")
    else:
        if not isinstance(script, str) or not script:
            raise RequestError("script must be a non-empty string")
        argv = ["bash", "-lc", script]

    cwd = config.cwd
    if "cwd" in body:
        if not isinstance(body["cwd"], str):
            raise RequestError("cwd must be a string")
        cwd = config.cwd / os.path.expanduser(body["cwd"])
        if not cwd.is_dir():
            raise RequestError("cwd is not a directory")

    stdin = body.get("stdin", "")
    if not isinstance(stdin, str):
        raise RequestError("stdin must be a string")

    timeout_ms = body.get("timeout_ms", timeout_limit_ms)
    if not isinstance(timeout_ms, int) or isinstance(timeout_ms, bool) or timeout_ms < 1:
        raise RequestError("timeout_ms must be a positive integer")

    return Command(
        argv=argv,
        cwd=cwd,
        stdin=stdin.encode("utf-8"),
        timeout_ms=min(timeout_ms, timeout_limit_ms),
    )


def clip(data: bytes, limit: int) -> tuple[str, bool]:
    if len(data) <= limit:
        return data.decode("utf-8", errors="replace"), False
    return data[:limit].decode("utf-8", errors="replace"), True


def run_command(config: Config, command: Command) -> dict[str, Any]:
    # A new session puts the command and its children in one process group,
    # so a timeout stops the whole tree.
    process = subprocess.Popen(
        command.argv,
        cwd=command.cwd,
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        start_new_session=True,
    )
    timed_out = False
    try:
        stdout, stderr = process.communicate(command.stdin, timeout=command.timeout_ms / 1000)
    except subprocess.TimeoutExpired:
        timed_out = True
        os.killpg(process.pid, signal.SIGKILL)
        stdout, stderr = process.communicate()

    # Stdout and stderr share one output budget.
    limit = config.limits["output_bytes"]
    out, out_clipped = clip(stdout, limit)
    err, err_clipped = clip(stderr, max(0, limit - len(stdout)))
    return {
        "exit": process.returncode,
        "stdout": out,
        "stderr": err,
        "timed_out": timed_out,
        "truncated": out_clipped or err_clipped,
    }


def now() -> str:
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())


def write_status(directory: Path, status: dict[str, Any]) -> None:
    # Rename is atomic, so a status reader never sees a partial file.
    temporary = directory / "status.json.tmp"
    temporary.write_text(json.dumps(status), encoding="utf-8")
    temporary.replace(directory / "status.json")


def start_job(config: Config, caller: str, command: Command, lease: Any) -> dict[str, Any]:
    """Start the command in the background. The job holds the lease until it ends."""
    job = f"{int(time.time() * 1000):012x}-{secrets.token_hex(4)}"
    directory = config.jobs_dir / job
    directory.mkdir(parents=True)
    hold = contextlib.ExitStack()
    hold.enter_context(lease or contextlib.nullcontext())
    try:
        with open(directory / "stdout", "wb") as stdout, open(directory / "stderr", "wb") as stderr:
            process = subprocess.Popen(
                command.argv,
                cwd=command.cwd,
                stdin=subprocess.PIPE,
                stdout=stdout,
                stderr=stderr,
                start_new_session=True,
            )
    except OSError:
        hold.close()
        raise
    status = {"owner": caller, "state": "running", "pid": process.pid, "started_at": now()}
    write_status(directory, status)

    def wait() -> None:
        with hold:
            timed_out = False
            try:
                process.communicate(command.stdin, timeout=command.timeout_ms / 1000)
            except subprocess.TimeoutExpired:
                timed_out = True
                os.killpg(process.pid, signal.SIGKILL)
                process.wait()
            write_status(directory, dict(status, state="done", exit=process.returncode, timed_out=timed_out, ended_at=now()))
            # The notify command runs before the lease ends, so the machine
            # stays awake until the caller has the result.
            if config.notify:
                run_notify(config, caller, job_status(config, caller, job))

    threading.Thread(target=wait, daemon=True).start()
    return {"job": job, "state": "running"}


def tail(path: Path, limit: int) -> tuple[bytes, bool]:
    with open(path, "rb") as file:
        size = file.seek(0, os.SEEK_END)
        file.seek(max(0, size - limit))
        return file.read(), size > limit


def pid_alive(pid: int) -> bool:
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
    except PermissionError:
        return True
    return True


def job_status(config: Config, caller: str, job: str) -> dict[str, Any]:
    directory = config.jobs_dir / job
    try:
        status = json.loads((directory / "status.json").read_text(encoding="utf-8"))
    except FileNotFoundError:
        raise RequestError("not_found") from None
    # Another caller's job looks the same as a missing job.
    if status.get("owner") != caller:
        raise RequestError("not_found")
    if status["state"] == "running" and not pid_alive(status["pid"]):
        # The provider writes the result just after the process ends. Report
        # "lost" only when no result arrived inside that window.
        written = (directory / "status.json").stat().st_mtime
        if time.time() - written > LOST_GRACE_S:
            status["state"] = "lost"

    # The end of the output matters most for a long job. Return the tails.
    limit = config.limits["output_bytes"]
    stdout, out_clipped = tail(directory / "stdout", limit)
    stderr, err_clipped = tail(directory / "stderr", max(0, limit - len(stdout)))
    reply = {key: status[key] for key in ("state", "started_at", "ended_at", "exit", "timed_out") if key in status}
    return dict(
        reply,
        job=job,
        stdout=stdout.decode("utf-8", errors="replace"),
        stderr=stderr.decode("utf-8", errors="replace"),
        truncated=out_clipped or err_clipped,
    )


def handle_event(config: Config, event: Any, lease: Any = None) -> dict[str, Any]:
    if not isinstance(event, dict) or event.get("op") != "request":
        return {"op": "error", "request_id": None, "error": "invalid_request"}
    request_id = event.get("request_id")
    caller, message, meta = event.get("from"), event.get("message"), event.get("meta")
    try:
        if not isinstance(request_id, (str, int)) or isinstance(request_id, bool):
            raise RequestError("invalid_request")
        if not isinstance(message, str) or not isinstance(meta, dict):
            raise RequestError("invalid_request")
        if meta.get("method") != "EXEC":
            raise RequestError("invalid_request")
        if caller not in config.grants:
            raise AccessError("access_denied")
        action, target = parse_request(config, message)
        if action == "start":
            result = start_job(config, caller, target, lease)
        elif action == "status":
            result = job_status(config, caller, target)
        else:
            with lease or contextlib.nullcontext():
                result = run_command(config, target)
        return {
            "op": "reply",
            "request_id": request_id,
            "reply": json.dumps(result, ensure_ascii=False, separators=(",", ":")),
        }
    except (OSError, ProviderError) as error:
        return {"op": "error", "request_id": request_id, "error": safe_error(error)}


def main() -> int:
    try:
        config = load_config()
    except ProviderError as error:
        print(str(error), file=sys.stderr)
        return 1

    lease = Lease(config.lease) if config.lease else None
    write_lock = threading.Lock()

    def respond(response: dict[str, Any]) -> None:
        line = json.dumps(response, ensure_ascii=False, separators=(",", ":"))
        with write_lock:
            print(line, flush=True)

    def serve(raw_line: bytes) -> None:
        try:
            respond(handle_event(config, json.loads(raw_line.decode("utf-8")), lease))
        except (UnicodeError, ValueError, json.JSONDecodeError):
            respond({"op": "error", "request_id": None, "error": "invalid_request"})

    # A JSON string may expand six-fold when it carries escaped non-ASCII
    # characters.  Bound the outer NDJSON record to match.
    maximum_line = config.limits["body_bytes"] * 6 + 8 * 1024
    while True:
        raw_line = sys.stdin.buffer.readline(maximum_line + 1)
        if not raw_line:
            break
        if len(raw_line) > maximum_line and not raw_line.endswith(b"\n"):
            while raw_line and not raw_line.endswith(b"\n"):
                raw_line = sys.stdin.buffer.readline(maximum_line + 1)
            respond({"op": "error", "request_id": None, "error": "request_too_large"})
            continue
        threading.Thread(target=serve, args=(raw_line,), daemon=True).start()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
