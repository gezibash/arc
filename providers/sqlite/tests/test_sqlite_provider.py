import json
import os
import sqlite3
import tempfile
import time
import unittest
from unittest import mock
from pathlib import Path

import sys
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import sqlite_provider as provider


ALICE = "a" * 64
BOB = "b" * 64


class SQLiteProviderTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        root = Path(self.directory.name)
        self.database_path = root / "main.db"
        self.config_path = root / "config.json"
        self.config_path.write_text(json.dumps({
            "databases": {"main": {"path": str(self.database_path), "grants": {ALICE: "write", BOB: "read"}}},
            "limits": {"rows": 2, "output_bytes": 256, "query_ms": 1000},
        }))
        self.config = provider.load_config({"SQLITE_CONFIG": str(self.config_path)})

    def tearDown(self):
        self.directory.cleanup()

    def request(self, caller, body, path="/main"):
        event = {"op": "request", "request_id": "request-1", "from": caller, "meta": {"method": "QUERY", "path": path}, "message": json.dumps(body)}
        return provider.handle_event(self.config, event)

    def reply(self, caller, body):
        response = self.request(caller, body)
        self.assertEqual("reply", response["op"], response)
        return json.loads(response["reply"])

    def test_write_read_and_blob_round_trip(self):
        self.reply(ALICE, {"sql": "CREATE TABLE notes(id INTEGER PRIMARY KEY, body BLOB)"})
        self.reply(ALICE, {"sql": "INSERT INTO notes(body) VALUES(?)", "params": [{"base64": "AAEC"}]})
        result = self.reply(BOB, {"sql": "SELECT body FROM notes"})["results"][0]
        self.assertEqual(["body"], result["columns"])
        self.assertEqual([[{"base64": "AAEC"}]], result["rows"])

    def test_read_grant_cannot_write(self):
        self.reply(ALICE, {"sql": "CREATE TABLE notes(body TEXT)"})
        response = self.request(BOB, {"sql": "INSERT INTO notes(body) VALUES('no')"})
        self.assertEqual({"op": "error", "request_id": "request-1", "error": "write_denied"}, response)

    def test_read_grant_can_use_an_atomic_batch(self):
        self.reply(ALICE, {"sql": "CREATE TABLE notes(body TEXT)"})
        self.reply(ALICE, {"sql": "INSERT INTO notes(body) VALUES('visible')"})
        response = self.reply(BOB, {"statements": [{"sql": "SELECT body FROM notes"}]})
        self.assertEqual([["visible"]], response["results"][0]["rows"])

    def test_ungranted_or_unknown_database_is_denied(self):
        self.assertEqual("unauthorized", self.request("c" * 64, {"sql": "SELECT 1"})["error"])
        self.assertEqual("not_found", self.request(ALICE, {"sql": "SELECT 1"}, "/missing")["error"])

    def test_batch_is_atomic_and_rejects_user_transactions(self):
        self.reply(ALICE, {"sql": "CREATE TABLE notes(body TEXT UNIQUE)"})
        response = self.request(ALICE, {"statements": [
            {"sql": "INSERT INTO notes(body) VALUES(?)", "params": ["first"]},
            {"sql": "INSERT INTO notes(body) VALUES(?)", "params": ["first"]},
        ]})
        self.assertEqual("query_failed", response["error"])
        self.assertEqual([[0]], self.reply(ALICE, {"sql": "SELECT count(*) FROM notes"})["results"][0]["rows"])
        response = self.request(ALICE, {"statements": [{"sql": "BEGIN"}]})
        self.assertEqual("query_denied", response["error"])

    def test_unsafe_sqlite_features_are_denied_by_authorizer(self):
        for sql in ("ATTACH DATABASE ':memory:' AS other", "DETACH DATABASE main", "PRAGMA user_version", "VACUUM INTO 'outside.db'"):
            self.assertEqual("query_denied", self.request(ALICE, {"sql": sql})["error"], sql)

    def test_result_limit_is_rejected_without_truncation(self):
        self.reply(ALICE, {"sql": "CREATE TABLE notes(body TEXT)"})
        self.reply(ALICE, {"statements": [{"sql": "INSERT INTO notes(body) VALUES(?)", "params": ["a"]}, {"sql": "INSERT INTO notes(body) VALUES(?)", "params": ["b"]}]})
        self.reply(ALICE, {"sql": "INSERT INTO notes(body) VALUES(?)", "params": ["c"]})
        self.assertEqual("result_too_large", self.request(ALICE, {"sql": "SELECT body FROM notes"})["error"])

    def test_batch_rolls_back_when_its_result_exceeds_a_limit(self):
        self.reply(ALICE, {"sql": "CREATE TABLE notes(body TEXT)"})
        self.reply(ALICE, {"sql": "INSERT INTO notes(body) VALUES('existing')"})
        self.reply(ALICE, {"sql": "INSERT INTO notes(body) VALUES('existing too')"})
        response = self.request(ALICE, {"statements": [
            {"sql": "INSERT INTO notes(body) VALUES('new')"},
            {"sql": "SELECT body FROM notes"},
        ]})
        self.assertEqual("result_too_large", response["error"])
        self.assertEqual(
            [["existing"], ["existing too"]],
            self.reply(ALICE, {"sql": "SELECT body FROM notes"})["results"][0]["rows"],
        )

    def test_single_write_returning_an_oversized_result_rolls_back(self):
        self.reply(ALICE, {"sql": "CREATE TABLE notes(body TEXT)"})
        response = self.request(ALICE, {"sql": "INSERT INTO notes(body) VALUES(?) RETURNING body", "params": ["x" * 220]})
        self.assertEqual("result_too_large", response["error"])
        self.assertEqual([[0]], self.reply(ALICE, {"sql": "SELECT count(*) FROM notes"})["results"][0]["rows"])

    def test_oversized_sqlite_value_is_rejected_before_result_allocation(self):
        response = self.request(ALICE, {"sql": "SELECT zeroblob(257)"})
        self.assertEqual("result_too_large", response["error"])

    def test_invalid_caller_is_an_access_failure(self):
        self.assertEqual("unauthorized", self.request("not-a-public-key", {"sql": "SELECT 1"})["error"])

    def test_query_timeout_is_reported(self):
        self.config.limits["query_ms"] = 1
        response = self.request(ALICE, {"sql": "WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x + 1 FROM n WHERE x < 10000000) SELECT sum(x) FROM n"})
        self.assertEqual("query_timeout", response["error"])

    def test_batch_shares_one_query_deadline(self):
        self.config.limits["query_ms"] = 30
        original = provider.execute_statement

        def delayed(*args, **kwargs):
            time.sleep(0.02)
            return original(*args, **kwargs)

        with mock.patch.object(provider, "execute_statement", side_effect=delayed):
            response = self.request(ALICE, {"statements": [{"sql": "SELECT 1"}, {"sql": "SELECT 2"}]})
        self.assertEqual("query_timeout", response["error"])

    def test_config_is_required_and_does_not_allow_relative_database_paths(self):
        with self.assertRaisesRegex(provider.ProviderError, "invalid SQLITE_CONFIG"):
            provider.load_config({})
        self.config_path.write_text(json.dumps({"databases": {"main": {"path": "relative.db", "grants": {ALICE: "read"}}}}))
        with self.assertRaisesRegex(provider.ProviderError, "invalid SQLITE_CONFIG"):
            provider.load_config({"SQLITE_CONFIG": str(self.config_path)})


if __name__ == "__main__":
    unittest.main()
