#!/usr/bin/env bash
# ferrous/rustserver entrypoint — dockerized port of the proven local setup:
#   start.sh (run args, env contract) + steamcmd update.sh + auto-update.sh
#   (buildid diff → oxide refresh) + oxide-install.sh (GitHub Oxide match).
#
# exec at the end matters twice: RustDedicated becomes PID 1 and receives the
# docker stop signal directly (STOPSIGNAL SIGINT = proven KillSignal=SIGINT,
# default stop grace 180s = proven TimeoutStopSec). Nothing wraps the game, so
# there is no supervisor to forget a forwarded signal.
#
# Server files live in SERVER_DIR (the bind-mounted data dir, default /server);
# first boot installs Steam app 258550 there (~4 GB), later boots update it.
set -euo pipefail

SERVER_DIR="${SERVER_DIR:-/server}"
SCRIPTS="${FERROUS_SCRIPTS:-/opt/ferrous}"
cd "$SERVER_DIR"

# --- env contract (proven start.sh: fail fast, never boot without a password)
: "${RCON_PASSWORD:?RCON_PASSWORD must be set (the deploy wizard sets it; see docs/api-v1.md)}"

# World / identity — proven defaults from start.sh. Seed/worldsize/level only
# apply to a fresh map (the identity's save in server/<identity>/ wins after).
SERVER_IDENTITY="${SERVER_IDENTITY:-main}"
SERVER_LEVEL="${SERVER_LEVEL:-Procedural Map}"
SERVER_SEED="${SERVER_SEED:-20261007}"
SERVER_WORLDSIZE="${SERVER_WORLDSIZE:-3500}"
SERVER_MAXPLAYERS="${SERVER_MAXPLAYERS:-50}"
AUTO_UPDATE="${AUTO_UPDATE:-true}"
OXIDE="${OXIDE:-true}"

MANIFEST="$SERVER_DIR/steamapps/appmanifest_258550.acf"
buildid() {
  [[ -f "$MANIFEST" ]] || { echo "none"; return; }
  # ACF files may be CRLF; strip \r, take the first quoted buildid (proven
  # auto-update.sh regex).
  tr -d '\r' <"$MANIFEST" | grep -m1 -oE '"buildid"[[:space:]]*"[0-9]+"' \
    | grep -oE '[0-9]+' || true
}

# --- install / update (proven auto-update.sh semantics) -----------------------
FORCE_OXIDE=""
if [[ ! -x "$SERVER_DIR/RustDedicated" ]]; then
  echo "[ferrous] first boot: installing Rust dedicated server (Steam app 258550) — this downloads ~4 GB"
  "$SCRIPTS/update.sh"
elif [[ "${AUTO_UPDATE,,}" == "true" ]]; then
  BEFORE="$(buildid)"
  echo "[ferrous] buildid before: $BEFORE"
  # steamcmd failure exits the boot (restart policy retries — matches proven
  # auto-update.sh, which refuses to continue on a failed update)
  "$SCRIPTS/update.sh"
  AFTER="$(buildid)"
  if [[ "$BEFORE" != "$AFTER" ]]; then
    echo "[ferrous] game updated: $BEFORE -> $AFTER"
    FORCE_OXIDE=true
  else
    echo "[ferrous] buildid after:  $AFTER (up to date)"
  fi
else
  echo "[ferrous] AUTO_UPDATE=$AUTO_UPDATE — skipping steamcmd"
fi

# --- oxide match (uMod) -------------------------------------------------------
if [[ "${OXIDE,,}" == "true" ]]; then
  if [[ "$FORCE_OXIDE" == "true" || ! -f "$SERVER_DIR/oxide/oxide.config.json" ]]; then
    # proven auto-update.sh: an oxide failure warns and continues (the game
    # update already reverted the patches; a stale Oxide is survivable)
    if ! "$SCRIPTS/oxide-install.sh"; then
      echo "[ferrous] WARNING: Oxide update failed; continuing without a fresh Oxide." >&2
    fi
  fi
  # InvDump plugin (source: uMod compiles .cs on load — proven plugin workflow)
  mkdir -p "$SERVER_DIR/oxide/plugins"
  if [[ ! -f "$SERVER_DIR/oxide/plugins/InvDump.cs" ]]; then
    cp "$SCRIPTS/InvDump.cs" "$SERVER_DIR/oxide/plugins/InvDump.cs"
    echo "[ferrous] seeded oxide/plugins/InvDump.cs"
  fi
else
  echo "[ferrous] OXIDE=false — vanilla mode (panel inventory will 404 gracefully)"
fi

# --- run (proven start.sh args) ----------------------------------------------
# Same library path Facepunch's runds.sh / proven start.sh use (native plugins).
export LD_LIBRARY_PATH="${LD_LIBRARY_PATH:-}:$SERVER_DIR/RustDedicated_Data/Plugins:$SERVER_DIR/RustDedicated_Data/Plugins/x86_64"

RUN_ARGS=(
  -batchmode
  -nographics
  -silent-crashes
  +server.ip 0.0.0.0
  +server.port 28015
  +server.queryport 28016
  +server.identity "$SERVER_IDENTITY"
  +server.level "$SERVER_LEVEL"
  +server.seed "$SERVER_SEED"
  +server.worldsize "$SERVER_WORLDSIZE"
  +server.maxplayers "$SERVER_MAXPLAYERS"
  # rcon on all container interfaces: the agent dials the container IP
  # (proven host bound 127.0.0.1 — in a container that must become 0.0.0.0;
  #  the port is never published, so the LAN still can't reach it)
  +rcon.ip 0.0.0.0
  +rcon.port 28016
  +rcon.password "$RCON_PASSWORD"
  +rcon.web true
)
if [[ -n "${SERVER_NAME:-}" ]]; then
  # startup convar; a server/<identity>/cfg/server.cfg present on disk overrides
  # it (proven: server.cfg is read after the command line and wins)
  RUN_ARGS+=(+server.hostname "$SERVER_NAME")
fi

echo "[ferrous] starting RustDedicated (identity=$SERVER_IDENTITY seed=$SERVER_SEED worldsize=$SERVER_WORLDSIZE maxplayers=$SERVER_MAXPLAYERS)"
exec ./RustDedicated "${RUN_ARGS[@]}"
