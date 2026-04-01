#!/usr/bin/env python3

import fcntl
import json
import os
import pty
import select
import signal
import struct
import subprocess
import sys
import termios


def emit(event):
    sys.stdout.write(json.dumps(event) + "\n")
    sys.stdout.flush()


def set_winsize(fd, rows, cols):
    winsz = struct.pack("HHHH", rows, cols, 0, 0)
    fcntl.ioctl(fd, termios.TIOCSWINSZ, winsz)


def start_shell():
    master_fd, slave_fd = pty.openpty()
    set_winsize(master_fd, 24, 80)

    env = dict(os.environ)
    env["TERM"] = "dumb"
    env["PS1"] = "ARC> "

    proc = subprocess.Popen(
        ["/bin/sh", "-i"],
        stdin=slave_fd,
        stdout=slave_fd,
        stderr=slave_fd,
        start_new_session=True,
        env=env,
        close_fds=True,
    )

    os.close(slave_fd)
    os.set_blocking(master_fd, False)

    return {
        "master_fd": master_fd,
        "proc": proc,
        "closing": False,
    }


def close_shell(session):
    if session["closing"]:
        return

    try:
        os.write(session["master_fd"], b"exit\n")
    except OSError:
        pass

    session["closing"] = True


def handle_event(payload, sessions):
    op = payload.get("op")
    app_session_id = payload.get("app_session_id")

    if op == "stream_open":
        session = start_shell()
        sessions[app_session_id] = session
        emit(
            {
                "op": "stream_data",
                "app_session_id": app_session_id,
                "channel": "stdout",
                "data": "opened pty shell\n",
            }
        )
        return

    session = sessions.get(app_session_id)
    if session is None:
        return

    if op == "stream_data":
        os.write(session["master_fd"], payload.get("message", "").encode())
        return

    if op == "stream_resize":
        cols = int(payload.get("cols", 80))
        rows = int(payload.get("rows", 24))
        set_winsize(session["master_fd"], rows, cols)

        try:
            os.killpg(session["proc"].pid, signal.SIGWINCH)
        except ProcessLookupError:
            pass

        emit(
            {
                "op": "stream_data",
                "app_session_id": app_session_id,
                "channel": "stdout",
                "data": f"resize:{cols}x{rows}\n",
            }
        )
        return

    if op == "stream_close":
        close_shell(session)


def drain_session(app_session_id, session, sessions):
    try:
        data = os.read(session["master_fd"], 4096)
    except BlockingIOError:
        data = b""
    except OSError:
        data = b""

    if data:
        emit(
            {
                "op": "stream_data",
                "app_session_id": app_session_id,
                "channel": "stdout",
                "data": data.decode(errors="replace"),
            }
        )

    code = session["proc"].poll()
    if code is not None:
        try:
            os.close(session["master_fd"])
        except OSError:
            pass

        emit(
            {
                "op": "stream_exit",
                "app_session_id": app_session_id,
                "status": code,
                "message": "shell exited",
            }
        )
        sessions.pop(app_session_id, None)


def main():
    stdin_fd = sys.stdin.fileno()
    stdin_buffer = b""
    sessions = {}

    while True:
        read_fds = [stdin_fd] + [session["master_fd"] for session in sessions.values()]
        ready, _, _ = select.select(read_fds, [], [], 0.05)

        if stdin_fd in ready:
            chunk = os.read(stdin_fd, 4096)
            if not chunk:
                break

            stdin_buffer += chunk

            while b"\n" in stdin_buffer:
                line, stdin_buffer = stdin_buffer.split(b"\n", 1)
                if not line.strip():
                    continue
                handle_event(json.loads(line.decode()), sessions)

        for app_session_id, session in list(sessions.items()):
            if session["master_fd"] in ready or session["proc"].poll() is not None:
                drain_session(app_session_id, session, sessions)

    for session in list(sessions.values()):
        close_shell(session)


if __name__ == "__main__":
    try:
        main()
    except BrokenPipeError:
        pass
