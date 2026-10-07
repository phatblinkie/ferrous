#!/usr/bin/env bash
# Update/install the Rust dedicated server (Steam app 258550) anonymously.
# Port of the proven /mnt/storage/steamcmd/update.sh — paths parameterized.
# Usage: update.sh [validate]
set -uo pipefail
SERVER_DIR="${SERVER_DIR:-/server}"
STEAMCMD="${STEAMCMD:-/opt/steamcmd/steamcmd.sh}"

mkdir -p "$SERVER_DIR"
EXTRA=(+app_update 258550)
if [[ "${1:-}" == "validate" ]]; then
  EXTRA+=(validate)
fi

# steamcmd wants its own working dir (writes steam/ state next to itself)
cd "$(dirname "$STEAMCMD")"
exec "$STEAMCMD" +force_install_dir "$SERVER_DIR" +login anonymous "${EXTRA[@]}" +quit
