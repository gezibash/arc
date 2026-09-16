"""A deliberately small, fail-closed SQLite ARC provider.

The ARC runtime sends one JSON event per input line.  Requests name a configured
database by the invocation path (for example, ``/main``); database paths and
permissions are chosen only by the provider operator's SQLITE_CONFIG file.
"""

from __future__ import annotations

import base64
import contextlib
import json
import math
import os
import re
import sqlite3
import sys
import time
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterator


PUBLIC_KEY = re.compile(r"^[0-9a-f]{64}$")
DATABASE_NAME = re.compile(r"^[a-z][a-z0-9_-]{0,63}$")


class ProviderError(Exception):
    """Expected errors whose text is safe to expose on ARC."""


class RequestError(ProviderError):
    pass


class AccessError(ProviderError):
    pass


class LimitError(ProviderError):
    pass


DEFAULT_LIMITS = {
    "body_bytes": 256 * 1024,
    "sql_bytes": 64 * 1024,
    "batch_statements": 32,
    "rows": 1_000,
    "output_bytes": 1 * 1024 * 1024,
    "query_ms": 5_000,
    "busy_ms": 1_000,
}
MAX_LIMITS = {
    "body_bytes": 1 * 1024 * 1024,
    "sql_bytes": 1 * 1024 * 1024,
    "batch_statements": 256,
    "rows": 10_000,
    "output_bytes": 1 * 1024 * 1024,
    "query_ms": 30_000,
    "busy_ms": 10_000,
}


@dataclass(frozen=True)
class Database:
    name: str
    path: Path
    grants: dict[str, str]


@dataclass(frozen=True)
class Config:
    databases: dict[str, Database]
    limits: dict[str, int]


def safe_error(error: Exception) -> str:
    if isinstance(error, ProviderError):
        return str(error)
    if isinstance(error, sqlite3.Error):
        return "query_failed"
    return "invalid_request"


def load_config(environ: dict[str, str] | None = None) -> Config:
    environ = os.environ if environ is None else environ
    raw_path = environ.get("SQLITE_CONFIG")
    if not raw_path or not os.path.isabs(raw_path):
        raise ProviderError("invalid SQLITE_CONFIG")

    try:
        path = Path(raw_path)
        if not path.is_file() or path.is_symlink():
            raise ValueError("not a regular file")
        data = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, UnicodeError, ValueError, json.JSONDecodeError):
        raise ProviderError("invalid SQLITE_CONFIG") from None

    if not isinstance(data, dict) or set(data) - {"databases", "limits"}:
        raise ProviderError("invalid SQLITE_CONFIG")

    databases_data = data.get("databases")
    if not isinstance(databases_data, dict) or not databases_data:
        raise ProviderError("invalid SQLITE_CONFIG")

    databases: dict[str, Database] = {}
    for name, entry in databases_data.items():
        if not isinstance(name, str) or not DATABASE_NAME.fullmatch(name):
            raise ProviderError("invalid SQLITE_CONFIG")
        if not isinstance(entry, dict) or set(entry) != {"path", "grants"}:
            raise ProviderError("invalid SQLITE_CONFIG")
        raw_database_path, grants = entry["path"], entry["grants"]
        if not isinstance(raw_database_path, str) or not os.path.isabs(raw_database_path):
            raise ProviderError("invalid SQLITE_CONFIG")
        database_path = Path(raw_database_path)
        if database_path.exists() and database_path.is_symlink():
            raise ProviderError("invalid SQLITE_CONFIG")
        if not isinstance(grants, dict) or not grants:
            raise ProviderError("invalid SQLITE_CONFIG")
        parsed_grants: dict[str, str] = {}
        for public_key, role in grants.items():
            if not isinstance(public_key, str) or not PUBLIC_KEY.fullmatch(public_key):
                raise ProviderError("invalid SQLITE_CONFIG")
            if role not in ("read", "write"):
                raise ProviderError("invalid SQLITE_CONFIG")
            parsed_grants[public_key] = role
        databases[name] = Database(name, database_path, parsed_grants)

    raw_limits = data.get("limits", {})
    if not isinstance(raw_limits, dict) or set(raw_limits) - set(DEFAULT_LIMITS):
        raise ProviderError("invalid SQLITE_CONFIG")
    limits = dict(DEFAULT_LIMITS)
    for name, value in raw_limits.items():
        if isinstance(value, bool) or not isinstance(value, int) or value <= 0 or value > MAX_LIMITS[name]:
            raise ProviderError("invalid SQLITE_CONFIG")
        limits[name] = value
    return Config(databases, limits)


def decode_value(value: Any) -> Any:
    if isinstance(value, dict) and set(value) == {"base64"} and isinstance(value["base64"], str):
        try:
            return base64.b64decode(value["base64"], validate=True)
        except ValueError:
            raise RequestError("invalid_request") from None
    if isinstance(value, list):
        return [decode_value(item) for item in value]
    if isinstance(value, dict):
        return {key: decode_value(item) for key, item in value.items()}
    if isinstance(value, float) and not math.isfinite(value):
        raise RequestError("invalid_request")
    if value is None or isinstance(value, (str, int, float, bool)):
        return value
    raise RequestError("invalid_request")


def encode_value(value: Any) -> Any:
    if isinstance(value, bytes):
        return {"base64": base64.b64encode(value).decode("ascii")}
    if isinstance(value, float) and not math.isfinite(value):
        raise RequestError("query_failed")
    if value is None or isinstance(value, (str, int, float, bool)):
        return value
    raise RequestError("query_failed")


def parse_parameters(value: Any) -> list[Any] | dict[str, Any]:
    if value is None:
        return []
    if not isinstance(value, (list, dict)):
        raise RequestError("invalid_request")
    parsed = decode_value(value)
    if isinstance(parsed, dict) and not all(isinstance(key, str) for key in parsed):
        raise RequestError("invalid_request")
    return parsed


def parse_statement(value: Any, limits: dict[str, int]) -> tuple[str, list[Any] | dict[str, Any]]:
    if not isinstance(value, dict) or set(value) - {"sql", "params"} or not isinstance(value.get("sql"), str):
        raise RequestError("invalid_request")
    sql = value["sql"]
    if not sql.strip() or len(sql.encode("utf-8")) > limits["sql_bytes"]:
        raise RequestError("invalid_request")
    return sql, parse_parameters(value.get("params"))


def parse_request(message: str, limits: dict[str, int]) -> tuple[bool, list[tuple[str, list[Any] | dict[str, Any]]]]:
    if len(message.encode("utf-8")) > limits["body_bytes"]:
        raise RequestError("request_too_large")
    try:
        body = json.loads(message)
    except (ValueError, json.JSONDecodeError):
        raise RequestError("invalid_request") from None
    if not isinstance(body, dict):
        raise RequestError("invalid_request")
    has_single = "sql" in body
    has_batch = "statements" in body
    if has_single == has_batch or set(body) - ({"sql", "params"} if has_single else {"statements"}):
        raise RequestError("invalid_request")
    if has_single:
        return False, [parse_statement(body, limits)]
    statements = body["statements"]
    if not isinstance(statements, list) or not statements or len(statements) > limits["batch_statements"]:
        raise RequestError("invalid_request")
    return True, [parse_statement(statement, limits) for statement in statements]


class Authorizer:
    """SQLite's authorizer is the policy boundary, including transaction SQL."""

    def __init__(self, role: str) -> None:
        self.role = role
        self.internal = False
        self.denial_reason = "query_denied"

    @contextlib.contextmanager
    def internal_operation(self) -> Iterator[None]:
        self.internal = True
        try:
            yield
        finally:
            self.internal = False

    def __call__(self, action: int, arg1: str | None, arg2: str | None, database: str | None, source: str | None) -> int:
        if self.internal:
            return sqlite3.SQLITE_OK
        if action in (sqlite3.SQLITE_ATTACH, sqlite3.SQLITE_DETACH, sqlite3.SQLITE_PRAGMA):
            return sqlite3.SQLITE_DENY
        if action in (sqlite3.SQLITE_TRANSACTION, sqlite3.SQLITE_SAVEPOINT):
            return sqlite3.SQLITE_DENY
        if action == sqlite3.SQLITE_FUNCTION and (arg2 or "").lower() in {"load_extension", "writefile", "readfile"}:
            return sqlite3.SQLITE_DENY
        if self.role == "read" and action in {
            sqlite3.SQLITE_INSERT,
            sqlite3.SQLITE_UPDATE,
            sqlite3.SQLITE_DELETE,
            sqlite3.SQLITE_CREATE_INDEX,
            sqlite3.SQLITE_CREATE_TABLE,
            sqlite3.SQLITE_CREATE_TEMP_INDEX,
            sqlite3.SQLITE_CREATE_TEMP_TABLE,
            sqlite3.SQLITE_CREATE_TEMP_TRIGGER,
            sqlite3.SQLITE_CREATE_TEMP_VIEW,
            sqlite3.SQLITE_CREATE_TRIGGER,
            sqlite3.SQLITE_CREATE_VIEW,
            sqlite3.SQLITE_CREATE_VTABLE,
            sqlite3.SQLITE_DROP_INDEX,
            sqlite3.SQLITE_DROP_TABLE,
            sqlite3.SQLITE_DROP_TEMP_INDEX,
            sqlite3.SQLITE_DROP_TEMP_TABLE,
            sqlite3.SQLITE_DROP_TEMP_TRIGGER,
            sqlite3.SQLITE_DROP_TEMP_VIEW,
            sqlite3.SQLITE_DROP_TRIGGER,
            sqlite3.SQLITE_DROP_VIEW,
            sqlite3.SQLITE_DROP_VTABLE,
            sqlite3.SQLITE_ALTER_TABLE,
            sqlite3.SQLITE_REINDEX,
            sqlite3.SQLITE_ANALYZE,
        }:
            self.denial_reason = "write_denied"
            return sqlite3.SQLITE_DENY
        return sqlite3.SQLITE_OK


def require_time(deadline: float) -> None:
    if time.monotonic() >= deadline:
        raise LimitError("query_timeout")


def connect(
    database: Database, role: str, limits: dict[str, int], deadline: float
) -> tuple[sqlite3.Connection, Authorizer]:
    require_time(deadline)
    effective_busy_seconds = min(
        limits["busy_ms"] / 1000,
        max(0, deadline - time.monotonic()),
    )
    if role == "read":
        uri = database.path.as_uri() + "?mode=ro"
        connection = sqlite3.connect(uri, uri=True, timeout=effective_busy_seconds)
    else:
        database.path.parent.mkdir(parents=True, exist_ok=True)
        connection = sqlite3.connect(str(database.path), timeout=effective_busy_seconds)
    connection.execute("PRAGMA trusted_schema=OFF")
    connection.execute("PRAGMA temp_store=MEMORY")
    connection.execute("PRAGMA foreign_keys=ON")
    # SQLite checks these before allocating a single TEXT/BLOB value or parsing
    # oversized SQL.  The output budget is also checked incrementally below.
    connection.setlimit(sqlite3.SQLITE_LIMIT_LENGTH, limits["output_bytes"])
    connection.setlimit(sqlite3.SQLITE_LIMIT_SQL_LENGTH, limits["sql_bytes"])
    authorizer = Authorizer(role)
    connection.set_authorizer(authorizer)
    return connection, authorizer


def execute_statement(
    connection: sqlite3.Connection,
    authorizer: Authorizer,
    sql: str,
    params: list[Any] | dict[str, Any],
    limits: dict[str, int],
    output_budget: list[int],
    deadline: float,
) -> dict[str, Any]:
    require_time(deadline)
    connection.set_progress_handler(lambda: int(time.monotonic() >= deadline), 1_000)
    try:
        cursor = connection.execute(sql, params)
        if cursor.description is None:
            return {"columns": [], "rows": [], "changes": cursor.rowcount if cursor.rowcount >= 0 else 0}
        columns = [column[0] for column in cursor.description]
        encoded_rows: list[list[Any]] = []
        output_budget[0] += len(json.dumps(columns, ensure_ascii=False, separators=(",", ":"), allow_nan=False).encode("utf-8"))
        while True:
            require_time(deadline)
            row = cursor.fetchone()
            if row is None:
                break
            if len(encoded_rows) >= limits["rows"]:
                raise LimitError("result_too_large")
            encoded_row = [encode_value(value) for value in row]
            require_time(deadline)
            output_budget[0] += len(json.dumps(encoded_row, ensure_ascii=False, separators=(",", ":"), allow_nan=False).encode("utf-8"))
            if output_budget[0] > limits["output_bytes"]:
                raise LimitError("result_too_large")
            encoded_rows.append(encoded_row)
        return {
            "columns": columns,
            "rows": encoded_rows,
            "changes": 0,
        }
    except sqlite3.Error as error:
        if "interrupted" in str(error).lower():
            raise LimitError("query_timeout") from None
        if "not authorized" in str(error).lower():
            raise AccessError(authorizer.denial_reason) from None
        if "cannot vacuum from within a transaction" in str(error).lower():
            raise AccessError("query_denied") from None
        if "string or blob too big" in str(error).lower():
            raise LimitError("result_too_large") from None
        raise
    finally:
        connection.set_progress_handler(None, 0)


def output_checked(results: list[dict[str, Any]], limits: dict[str, int], deadline: float) -> dict[str, Any]:
    require_time(deadline)
    response = {"results": results}
    try:
        encoded = json.dumps(response, ensure_ascii=False, separators=(",", ":"), allow_nan=False).encode("utf-8")
    except (TypeError, ValueError):
        raise RequestError("query_failed") from None
    if len(encoded) > limits["output_bytes"]:
        raise LimitError("result_too_large")
    require_time(deadline)
    return response


def run_query(config: Config, caller: str, path: str, message: str) -> dict[str, Any]:
    if not isinstance(caller, str) or not PUBLIC_KEY.fullmatch(caller):
        raise AccessError("unauthorized")
    if not isinstance(path, str) or not re.fullmatch(r"/[a-z][a-z0-9_-]{0,63}", path):
        raise RequestError("invalid_request")
    database = config.databases.get(path[1:])
    if database is None:
        raise AccessError("not_found")
    role = database.grants.get(caller)
    if role is None:
        raise AccessError("unauthorized")
    deadline = time.monotonic() + config.limits["query_ms"] / 1000
    _batch, statements = parse_request(message, config.limits)
    connection, authorizer = connect(database, role, config.limits, deadline)
    try:
        require_time(deadline)
        with authorizer.internal_operation():
            connection.execute("BEGIN" if role == "read" else "BEGIN IMMEDIATE")
        try:
            output_budget = [0]
            results = [
                execute_statement(connection, authorizer, sql, params, config.limits, output_budget, deadline)
                for sql, params in statements
            ]
            response = output_checked(results, config.limits, deadline)
            require_time(deadline)
            with authorizer.internal_operation():
                connection.execute("COMMIT")
            return response
        except Exception:
            if connection.in_transaction:
                with authorizer.internal_operation():
                    connection.execute("ROLLBACK")
            raise
    finally:
        connection.close()


def handle_event(config: Config, event: Any) -> dict[str, Any]:
    if not isinstance(event, dict) or event.get("op") != "request":
        return {"op": "error", "request_id": None, "error": "invalid_request"}
    request_id = event.get("request_id")
    caller, message, meta = event.get("from"), event.get("message"), event.get("meta")
    try:
        if not isinstance(request_id, (str, int)) or isinstance(request_id, bool):
            raise RequestError("invalid_request")
        if not isinstance(message, str) or not isinstance(meta, dict):
            raise RequestError("invalid_request")
        if meta.get("method") != "QUERY" or not isinstance(meta.get("path"), str):
            raise RequestError("invalid_request")
        return {
            "op": "reply",
            "request_id": request_id,
            "reply": json.dumps(
                run_query(config, caller, meta["path"], message),
                ensure_ascii=False,
                separators=(",", ":"),
                allow_nan=False,
            ),
        }
    except Exception as error:
        return {"op": "error", "request_id": request_id, "error": safe_error(error)}


def main() -> int:
    try:
        config = load_config()
    except ProviderError as error:
        print(str(error), file=sys.stderr)
        return 1
    # A JSON string may expand six-fold when it carries escaped non-ASCII
    # characters.  Bound the outer NDJSON record without rejecting valid
    # configured request bodies or allocating an unbounded input line.
    maximum_line = config.limits["body_bytes"] * 6 + 8 * 1024
    while True:
        raw_line = sys.stdin.buffer.readline(maximum_line + 1)
        if not raw_line:
            break
        if len(raw_line) > maximum_line and not raw_line.endswith(b"\n"):
            while raw_line and not raw_line.endswith(b"\n"):
                raw_line = sys.stdin.buffer.readline(maximum_line + 1)
            response = {"op": "error", "request_id": None, "error": "request_too_large"}
        else:
            try:
                response = handle_event(config, json.loads(raw_line.decode("utf-8")))
            except (UnicodeError, ValueError, json.JSONDecodeError):
                response = {"op": "error", "request_id": None, "error": "invalid_request"}
        print(json.dumps(response, ensure_ascii=False, separators=(",", ":"), allow_nan=False), flush=True)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
