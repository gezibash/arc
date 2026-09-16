#!/usr/bin/env bash
set -euo pipefail
cd /opt/arc/providers/sqlite
exec /home/ubuntu/.local/bin/mise exec -- python /opt/arc/scripts/traversal_lab/delayed_sqlite.py
