import io
import json
import subprocess
import tempfile
import time
import unittest
from pathlib import Path
from unittest import mock

import sys
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import exec_provider as provider


ALICE = "a" * 64
BOB = "b" * 64
MALLORY = "c" * 64


class ExecProviderTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.root = Path(self.directory.name)
        (self.root / "work").mkdir()
        self.config_path = self.root / "config.json"
        self.write_config({
            "grants": [ALICE, BOB],
            "cwd": str(self.root),
            "limits": {"output_bytes": 4096, "timeout_ms": 1000, "job_timeout_ms": 2000},
            "jobs_dir": str(self.root / "jobs"),
        })

    def tearDown(self):
        self.directory.cleanup()

    def write_config(self, data):
        self.config_path.write_text(json.dumps(data))
        self.config = provider.load_config({"EXEC_CONFIG": str(self.config_path)})

    def request(self, caller, body, method="EXEC", lease=None):
        event = {"op": "request", "request_id": "request-1", "from": caller, "meta": {"method": method, "path": "/"}, "message": json.dumps(body)}
        return provider.handle_event(self.config, event, lease)

    def reply(self, body, caller=ALICE, lease=None):
        response = self.request(caller, body, lease=lease)
        self.assertEqual("reply", response["op"], response)
        return json.loads(response["reply"])

    def error(self, caller, body, method="EXEC", lease=None):
        response = self.request(caller, body, method, lease)
        self.assertEqual("error", response["op"], response)
        return response["error"]

    def test_argv_runs_without_a_shell(self):
        result = self.reply({"argv": ["echo", "$HOME"]})
        self.assertEqual({"exit": 0, "stdout": "$HOME\n", "stderr": "", "timed_out": False, "truncated": False}, result)

    def test_script_runs_in_bash_and_reports_exit_code(self):
        result = self.reply({"script": "echo out; echo err >&2; exit 3"})
        self.assertEqual((3, "out\n", "err\n"), (result["exit"], result["stdout"], result["stderr"]))

    def test_stdin_and_relative_cwd(self):
        result = self.reply({"script": "cat; pwd", "stdin": "hello\n", "cwd": "work"})
        self.assertEqual(f"hello\n{(self.root / 'work').resolve()}\n", result["stdout"])

    def test_ungranted_key_is_denied_before_running(self):
        marker = self.root / "ran"
        self.assertEqual("access_denied", self.error(MALLORY, {"argv": ["touch", str(marker)]}))
        self.assertFalse(marker.exists())

    def test_timeout_kills_the_process_tree(self):
        started = time.monotonic()
        result = self.reply({"script": "sleep 30 & sleep 30", "timeout_ms": 200})
        self.assertTrue(result["timed_out"])
        self.assertLess(time.monotonic() - started, 5)

    def test_request_timeout_is_capped_by_config(self):
        result = self.reply({"script": "sleep 30", "timeout_ms": 999_999})
        self.assertTrue(result["timed_out"])

    def test_output_is_truncated_to_the_shared_budget(self):
        result = self.reply({"script": "head -c 5000 /dev/zero | tr '\\0' x; echo err >&2"})
        self.assertEqual(4096, len(result["stdout"]))
        self.assertEqual("", result["stderr"])
        self.assertTrue(result["truncated"])

    def test_invalid_requests_are_rejected(self):
        self.assertEqual("give exactly one of argv or script", self.error(ALICE, {"argv": ["true"], "script": "true"}))
        self.assertEqual("give exactly one of argv or script", self.error(ALICE, {}))
        self.assertEqual("argv must be a non-empty list of strings", self.error(ALICE, {"argv": []}))
        self.assertEqual("invalid_request", self.error(ALICE, {"argv": ["true"], "extra": 1}))
        self.assertEqual("cwd is not a directory", self.error(ALICE, {"argv": ["true"], "cwd": "missing"}))
        self.assertEqual("invalid_request", self.error(ALICE, {"argv": ["true"]}, method="QUERY"))

    def test_missing_executable_is_a_request_error(self):
        self.assertEqual("internal_error", self.error(ALICE, {"argv": ["definitely-not-a-command"]}))

    def test_config_is_fail_closed(self):
        cases = [
            {"grants": []},
            {"grants": ["not-a-key"]},
            {"grants": [ALICE], "cwd": "relative"},
            {"grants": [ALICE], "limits": {"timeout_ms": 999_999}},
            {"grants": [ALICE], "surprise": True},
        ]
        for data in cases:
            with self.subTest(data=data), self.assertRaises(provider.ProviderError):
                self.write_config(data)
        with self.assertRaises(provider.ProviderError):
            provider.load_config({})

    def test_lease_config_is_optional_and_validated(self):
        self.assertIsNone(self.config.lease)
        lease = {"hold": ["true"], "release": ["false"], "interval_ms": 60_000}
        self.write_config({"grants": [ALICE], "cwd": str(self.root), "lease": lease})
        self.assertEqual(provider.LeaseConfig(["true"], ["false"], 60_000), self.config.lease)
        cases = [
            {"hold": ["true"], "release": ["true"]},
            {"hold": [], "release": ["true"], "interval_ms": 60_000},
            {"hold": ["true"], "release": "true", "interval_ms": 60_000},
            {"hold": ["true"], "release": ["true"], "interval_ms": 999},
        ]
        for bad in cases:
            with self.subTest(lease=bad), self.assertRaises(provider.ProviderError):
                self.write_config({"grants": [ALICE], "lease": bad})


    def wait_for_job(self, job, caller=ALICE):
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline:
            status = self.reply({"action": "status", "job": job}, caller)
            if status["state"] != "running":
                return status
            time.sleep(0.05)
        self.fail("job did not end")

    def test_job_starts_at_once_and_reports_its_result(self):
        started = time.monotonic()
        job = self.reply({"action": "start", "script": "sleep 1.5; echo done; echo warn >&2; exit 4"})
        self.assertLess(time.monotonic() - started, 1.0)
        self.assertEqual("running", job["state"])
        self.assertEqual("running", self.reply({"action": "status", "job": job["job"]})["state"])
        status = self.wait_for_job(job["job"])
        self.assertEqual(("done", 4, False, "done\n", "warn\n"), (status["state"], status["exit"], status["timed_out"], status["stdout"], status["stderr"]))
        self.assertIn("ended_at", status)

    def test_job_timeout_uses_the_job_limit(self):
        # The run limit is 1 second. The job limit is 2 seconds.
        job = self.reply({"action": "start", "script": "sleep 1.5; echo slow", "timeout_ms": 999_999})
        status = self.wait_for_job(job["job"])
        self.assertEqual((False, "slow\n"), (status["timed_out"], status["stdout"]))
        job = self.reply({"action": "start", "script": "sleep 30"})
        self.assertTrue(self.wait_for_job(job["job"])["timed_out"])

    def test_job_output_returns_the_tail(self):
        job = self.reply({"action": "start", "script": "head -c 5000 /dev/zero | tr '\\0' x; echo END"})
        status = self.wait_for_job(job["job"])
        self.assertTrue(status["truncated"])
        self.assertEqual(4096, len(status["stdout"]))
        self.assertTrue(status["stdout"].endswith("xEND\n"))

    def test_job_is_visible_to_its_owner_only(self):
        job = self.reply({"action": "start", "argv": ["true"]})["job"]
        self.wait_for_job(job)
        self.assertEqual("not_found", self.error(BOB, {"action": "status", "job": job}))
        self.assertEqual("not_found", self.error(ALICE, {"action": "status", "job": "000000000000-00000000"}))
        self.assertEqual("status needs exactly one valid job", self.error(ALICE, {"action": "status", "job": "../etc"}))
        self.assertEqual("access_denied", self.error(MALLORY, {"action": "status", "job": job}))

    def test_job_with_a_dead_process_and_no_result_is_lost(self):
        directory = self.root / "jobs" / "000000000001-00000000"
        directory.mkdir(parents=True)
        for name in ("stdout", "stderr"):
            (directory / name).write_bytes(b"")
        dead = subprocess.Popen(["true"])
        dead.wait()
        (directory / "status.json").write_text(json.dumps({"owner": ALICE, "state": "running", "pid": dead.pid, "started_at": "x"}))
        self.assertEqual("lost", self.reply({"action": "status", "job": "000000000001-00000000"})["state"])

    def test_job_holds_the_lease_until_it_ends(self):
        calls = []
        lease = provider.Lease(provider.LeaseConfig(["hold"], ["release"], 60_000), run=lambda argv: calls.append(argv[0]))
        job = self.reply({"action": "start", "script": "sleep 0.3"}, lease=lease)["job"]
        self.assertEqual(["hold"], calls)
        self.wait_for_job(job)
        time.sleep(0.05)
        self.assertEqual(["hold", "release"], calls)

    def test_failed_job_start_releases_the_lease(self):
        calls = []
        lease = provider.Lease(provider.LeaseConfig(["hold"], ["release"], 60_000), run=lambda argv: calls.append(argv[0]))
        self.assertEqual("internal_error", self.error(ALICE, {"action": "start", "argv": ["definitely-not-a-command"]}, lease=lease))
        self.assertEqual(["hold", "release"], calls)

    def test_unknown_action_is_rejected(self):
        self.assertEqual("action must be run, start, or status", self.error(ALICE, {"action": "kill", "argv": ["true"]}))


class LeaseTest(unittest.TestCase):
    def make(self, interval_ms=60_000):
        calls = []
        lease = provider.Lease(provider.LeaseConfig(["hold"], ["release"], interval_ms), run=lambda argv: calls.append(argv[0]))
        return lease, calls

    def test_first_command_holds_and_last_command_releases(self):
        lease, calls = self.make()
        with lease:
            with lease:
                self.assertEqual(["hold"], calls)
            self.assertEqual(["hold"], calls)
        self.assertEqual(["hold", "release"], calls)

    def test_hold_is_refreshed_while_a_command_runs(self):
        lease, calls = self.make()
        # Shorten the interval below the 1 second config minimum for the test.
        lease.config = provider.LeaseConfig(["hold"], ["release"], 50)
        with lease:
            time.sleep(0.6)
        refreshes = calls.count("hold")
        self.assertGreaterEqual(refreshes, 3)
        self.assertEqual("release", calls[-1])
        time.sleep(0.2)
        self.assertEqual(refreshes, calls.count("hold"), "no refresh after the release")

    def test_request_runs_inside_the_lease(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        path = Path(directory.name) / "config.json"
        path.write_text(json.dumps({"grants": [ALICE], "cwd": directory.name}))
        config = provider.load_config({"EXEC_CONFIG": str(path)})
        lease, calls = self.make()
        event = {"op": "request", "request_id": 1, "from": ALICE, "meta": {"method": "EXEC"}, "message": json.dumps({"argv": ["true"]})}
        self.assertEqual("reply", provider.handle_event(config, event, lease)["op"])
        self.assertEqual(["hold", "release"], calls)
        denied = dict(event, **{"from": MALLORY})
        self.assertEqual("error", provider.handle_event(config, denied, lease)["op"])
        self.assertEqual(["hold", "release"], calls, "a denied request takes no lease")

    def test_failed_lease_command_is_logged_not_raised(self):
        with mock.patch("sys.stderr", new_callable=io.StringIO) as stderr:
            provider.run_lease_command(["false"])
            provider.run_lease_command(["definitely-not-a-command"])
        self.assertIn("lease command exited 1", stderr.getvalue())
        self.assertIn("lease command failed", stderr.getvalue())


if __name__ == "__main__":
    unittest.main()
