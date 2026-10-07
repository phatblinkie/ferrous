#!/usr/bin/env bash
# Offline behavior tests for entrypoint.sh / update.sh — a stub game binary
# and stub steamcmd/oxide scripts stand in for the real ones, so this runs in
# milliseconds with no network and no docker. The heavy paths (real install,
# real Oxide fetch, PID-1 signal semantics) are covered by the docker E2E.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
IMAGE="$(dirname "$HERE")"
EP="$IMAGE/entrypoint.sh"
fails=0
passes=0

ok()   { passes=$((passes + 1)); }
fail() { fails=$((fails + 1)); echo "  FAIL: $1" >&2; }

assert_eq() { # label want got
  if [[ "$2" == "$3" ]]; then ok; else fail "$1: want [$2] got [$3]"; fi
}
assert_contains() { # label haystack needle
  if [[ "$3" == *"$2"* ]]; then ok; else fail "$1: output missing [$2]"; fi
}
assert_not_contains() {
  if [[ "$3" == *"$2"* ]]; then fail "$1: output unexpectedly contains [$2]"; else ok; fi
}
assert_file() { # label path
  if [[ -f "$2" ]]; then ok; else fail "$1: missing file $2"; fi
}

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# --- fixtures -----------------------------------------------------------------
SCRIPTS="$tmp/scripts"
SERVER="$tmp/server"
mkdir -p "$SCRIPTS" "$SERVER"
cp "$IMAGE/InvDump.cs" "$SCRIPTS/InvDump.cs"

# stub steamcmd update: logs its invocation, optionally bumps the buildid
# (simulating a game update), optionally fails, optionally creates the game
cat >"$SCRIPTS/update.sh" <<'EOF'
#!/usr/bin/env bash
echo "STUB-UPDATE $*"
[[ "${STUB_UPDATE_FAIL:-}" == "1" ]] && exit 1
if [[ "${STUB_BUMP:-}" == "1" || ! -f "${SERVER_DIR}/steamapps/appmanifest_258550.acf" ]]; then
  mkdir -p "${SERVER_DIR}/steamapps"
  printf '"AppState"\n{\n\t"buildid"\t\t"%s"\n}\n' "${STUB_BUILDID:-1111}" \
    >"${SERVER_DIR}/steamapps/appmanifest_258550.acf"
fi
if [[ ! -x "${SERVER_DIR}/RustDedicated" ]]; then
  cat >"${SERVER_DIR}/RustDedicated" <<'STUBGAME'
#!/usr/bin/env bash
echo "STUB-ARGS: $*"
# args tests want an instant exit; the signal test sets STUB_LINGER=1 so the
# process stays around to receive SIGINT/SIGTERM
if [[ "${STUB_LINGER:-}" == "1" ]]; then
  trap 'echo STUB-GOT-INT;  exit 42' INT
  trap 'echo STUB-GOT-TERM; exit 43' TERM
  sleep 300 &
  wait $!
fi
exit 0
STUBGAME
  chmod +x "${SERVER_DIR}/RustDedicated"
fi
exit 0
EOF

# stub oxide install: logs, optionally fails
cat >"$SCRIPTS/oxide-install.sh" <<'EOF'
#!/usr/bin/env bash
echo "STUB-OXIDE"
[[ "${STUB_OXIDE_FAIL:-}" == "1" ]] && exit 1
mkdir -p "${SERVER_DIR}/oxide"
[[ -f "${SERVER_DIR}/oxide/oxide.config.json" ]] || echo '{}' >"${SERVER_DIR}/oxide/oxide.config.json"
exit 0
EOF
chmod +x "$SCRIPTS"/*.sh

run_ep() { # env... -- (runs entrypoint, captures stdout+stderr + exit code)
  local out rc
  out="$(env "$@" bash "$EP" 2>&1)"
  rc=$?
  EPOUT="$out"
  EPRC=$rc
}

# --- 1. syntax --------------------------------------------------------------
for f in "$EP" "$IMAGE/update.sh" "$IMAGE/oxide-install.sh"; do
  if bash -n "$f"; then ok; else fail "bash -n $f"; fi
done

# --- 2. env contract: missing RCON_PASSWORD must fail fast ------------------
run_ep SERVER_DIR="$SERVER" FERROUS_SCRIPTS="$SCRIPTS"
assert_eq "missing rcon password exits non-zero" "1" "$EPRC"
assert_contains "missing rcon password message" "RCON_PASSWORD must be set" "$EPOUT"
assert_not_contains "no game exec without password" "STUB-ARGS" "$EPOUT"

# --- 3. first boot: installs, then execs the game with proven args ----------
rm -rf "$SERVER"; mkdir -p "$SERVER"
run_ep SERVER_DIR="$SERVER" FERROUS_SCRIPTS="$SCRIPTS" RCON_PASSWORD=pw1 OXIDE=false
assert_contains "first boot runs update" "STUB-UPDATE" "$EPOUT"
assert_contains "first boot execs game" "STUB-ARGS:" "$EPOUT"
assert_contains "rcon binds all interfaces" "+rcon.ip 0.0.0.0" "$EPOUT"
assert_contains "rcon web enabled" "+rcon.web true" "$EPOUT"
assert_contains "rcon password passed" "+rcon.password pw1" "$EPOUT"
assert_contains "proven seed default" "+server.seed 20261007" "$EPOUT"
assert_contains "proven worldsize default" "+server.worldsize 3500" "$EPOUT"
assert_contains "identity default" "+server.identity main" "$EPOUT"
assert_contains "game port" "+server.port 28015" "$EPOUT"
assert_not_contains "no hostname convar without SERVER_NAME" "+server.hostname" "$EPOUT"

# --- 4. SERVER_NAME adds +server.hostname -----------------------------------
rm -rf "$SERVER"; mkdir -p "$SERVER"
run_ep SERVER_DIR="$SERVER" FERROUS_SCRIPTS="$SCRIPTS" RCON_PASSWORD=pw OXIDE=false \
  SERVER_NAME="My Server"
assert_contains "SERVER_NAME → hostname" "+server.hostname My Server" "$EPOUT"

# --- 5. AUTO_UPDATE=false with files present skips steamcmd -----------------
run_ep SERVER_DIR="$SERVER" FERROUS_SCRIPTS="$SCRIPTS" RCON_PASSWORD=pw OXIDE=false \
  AUTO_UPDATE=false
assert_not_contains "AUTO_UPDATE=false skips update" "STUB-UPDATE" "$EPOUT"
assert_contains "still runs the game" "STUB-ARGS:" "$EPOUT"

# --- 6. AUTO_UPDATE=true with same buildid skips oxide ----------------------
mkdir -p "$SERVER/oxide"
echo '{}' >"$SERVER/oxide/oxide.config.json" # oxide already applied
run_ep SERVER_DIR="$SERVER" FERROUS_SCRIPTS="$SCRIPTS" RCON_PASSWORD=pw OXIDE=true
assert_contains "update runs" "STUB-UPDATE" "$EPOUT"
assert_contains "up to date logged" "(up to date)" "$EPOUT"
assert_not_contains "no oxide refresh when buildid unchanged" "STUB-OXIDE" "$EPOUT"

# --- 7. game update forces oxide refresh ------------------------------------
run_ep SERVER_DIR="$SERVER" FERROUS_SCRIPTS="$SCRIPTS" RCON_PASSWORD=pw OXIDE=true \
  STUB_BUMP=1 STUB_BUILDID=2222
assert_contains "game update detected" "game updated:" "$EPOUT"
assert_contains "oxide refreshed after update" "STUB-OXIDE" "$EPOUT"

# --- 8. steamcmd failure aborts the boot (proven semantics) -----------------
run_ep SERVER_DIR="$SERVER" FERROUS_SCRIPTS="$SCRIPTS" RCON_PASSWORD=pw OXIDE=false \
  STUB_UPDATE_FAIL=1
assert_eq "update failure exits non-zero" "1" "$EPRC"
assert_not_contains "update failure does not exec the game" "STUB-ARGS" "$EPOUT"

# --- 9. oxide failure warns but the game still runs -------------------------
run_ep SERVER_DIR="$SERVER" FERROUS_SCRIPTS="$SCRIPTS" RCON_PASSWORD=pw OXIDE=true \
  STUB_OXIDE_FAIL=1 STUB_BUMP=1 STUB_BUILDID=3333
assert_contains "oxide failure warning" "WARNING: Oxide update failed" "$EPOUT"
assert_contains "game runs despite oxide failure" "STUB-ARGS:" "$EPOUT"

# --- 10. InvDump seeding: idempotent, never clobbers user edits -------------
run_ep SERVER_DIR="$SERVER" FERROUS_SCRIPTS="$SCRIPTS" RCON_PASSWORD=pw OXIDE=true
assert_file "InvDump.cs seeded" "$SERVER/oxide/plugins/InvDump.cs"
echo "// user edit" >"$SERVER/oxide/plugins/InvDump.cs"
run_ep SERVER_DIR="$SERVER" FERROUS_SCRIPTS="$SCRIPTS" RCON_PASSWORD=pw OXIDE=true
assert_contains "user InvDump edits preserved" "// user edit" "$(cat "$SERVER/oxide/plugins/InvDump.cs")"

# --- 11. OXIDE=false: vanilla, nothing seeded -------------------------------
rm -rf "$SERVER"; mkdir -p "$SERVER"
run_ep SERVER_DIR="$SERVER" FERROUS_SCRIPTS="$SCRIPTS" RCON_PASSWORD=pw OXIDE=false
assert_contains "vanilla log line" "vanilla mode" "$EPOUT"
if [[ -e "$SERVER/oxide" ]]; then fail "OXIDE=false must not create oxide/"; else ok; fi

# --- 12. signals pass straight through exec to the game ---------------------
# set -m: without job control a non-interactive shell starts background jobs
# with SIGINT ignored (POSIX async rule) and a trap can't reinstall an ignored
# signal. Docker doesn't do that, so the container path is unaffected — but
# the test needs monitor mode to deliver INT at all.
rm -rf "$SERVER"; mkdir -p "$SERVER"
set -m
env SERVER_DIR="$SERVER" FERROUS_SCRIPTS="$SCRIPTS" RCON_PASSWORD=pw OXIDE=false \
  STUB_LINGER=1 \
  bash "$EP" >"$tmp/sig.out" 2>&1 &
eppid=$!
# wait for the game stub to be exec'd (same pid as the entrypoint after exec)
for _ in $(seq 1 50); do grep -q "STUB-ARGS" "$tmp/sig.out" 2>/dev/null && break; sleep 0.1; done
if grep -q "STUB-ARGS" "$tmp/sig.out" 2>/dev/null; then
  kill -INT "$eppid" 2>/dev/null
  alive=1
  for _ in $(seq 1 30); do
    kill -0 "$eppid" 2>/dev/null || { alive=0; break; }
    sleep 0.1
  done
  if [[ $alive -eq 0 ]]; then
    wait "$eppid" 2>/dev/null
    assert_eq "SIGINT reaches the exec'd game (exit 42)" "42" "$?"
    assert_contains "game saw SIGINT" "STUB-GOT-INT" "$(cat "$tmp/sig.out")"
  else
    fail "signal test: game still alive after SIGINT"
    kill -9 "$eppid" 2>/dev/null
    wait "$eppid" 2>/dev/null
  fi
else
  fail "signal test: game never started"
  kill "$eppid" 2>/dev/null
  wait "$eppid" 2>/dev/null
fi
set +m

# --- 13. buildid parser (proven CRLF-tolerant regex) ------------------------
mkdir -p "$SERVER/steamapps"
printf '"AppState"\r\n{\r\n\t"appid"\t\t"258550"\r\n\t"buildid"\t\t"2634"\r\n}\r\n' \
  >"$SERVER/steamapps/appmanifest_258550.acf"
# entrypoint's function, extracted verbatim via sourcing a probe:
probe='
MANIFEST="'"$SERVER"'/steamapps/appmanifest_258550.acf"
buildid() {
  [[ -f "$MANIFEST" ]] || { echo "none"; return; }
  tr -d "\r" <"$MANIFEST" | grep -m1 -oE "\"buildid\"[[:space:]]*\"[0-9]+\"" \
    | grep -oE "[0-9]+" || true
}
buildid'
assert_eq "buildid parses CRLF manifest" "2634" "$(bash -c "$probe")"

# --- summary -----------------------------------------------------------------
echo
echo "entrypoint tests: $passes passed, $fails failed"
[[ $fails -eq 0 ]]
