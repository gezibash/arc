#!/usr/bin/env python3
"""Test-only reply gate after a real SQLite commit; never used by normal providers."""
import json
import os
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / 'providers' / 'sqlite'))
import sqlite_provider

original = sqlite_provider.handle_event


def delayed_reply(config, event):
    response = original(config, event)
    if response.get('op') == 'reply' and 'arc-lab-interrupted-write' in event.get('message', ''):
        receipt = Path(os.environ['ARC_LAB_RECEIPT'])
        # run_query has committed and closed the database before returning.
        with receipt.open('a') as output:
            output.write('committed\n')
            output.flush()
            os.fsync(output.fileno())
        release = Path(str(receipt) + '.release')
        deadline = time.monotonic() + 20
        while not release.exists() and time.monotonic() < deadline:
            time.sleep(0.02)
    return response


sqlite_provider.handle_event = delayed_reply
raise SystemExit(sqlite_provider.main())
