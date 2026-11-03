#!/usr/bin/env bash
# Runs the server in the foreground. Configured by SQLITE_PATH, JWT_SECRET,
# HOST and PORT only.
set -euo pipefail
exec "$(dirname "$0")/bin/server"
