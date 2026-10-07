#!/usr/bin/env bash
# Hermetic installer tests: --prefix fake root + --from local binary, so no
# network, no systemd and no real system paths are touched.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(dirname "$HERE")"
INSTALL="$REPO/install.sh"
passes=0
fails=0

ok()  { passes=$((passes + 1)); }
bad() { fails=$((fails + 1)); echo "  FAIL: $1" >&2; }

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# need a binary to install
if [ ! -x "$REPO/bin/ferrous-agent" ]; then
  (cd "$REPO" && make build >/dev/null 2>&1) || { echo "cannot build ferrous-agent" >&2; exit 1; }
fi

# --- 1. --help works standalone
if sh "$INSTALL" --help >/dev/null 2>&1; then ok; else bad "--help exits 0"; fi

# --- 2. install into a fake prefix
P1="$tmp/root1"
out1="$(sh "$INSTALL" --prefix "$P1" --from "$REPO/bin/ferrous-agent" --token testtok-123 2>&1)"
rc=$?
if [ $rc -eq 0 ]; then ok; else bad "install exits 0 (rc=$rc): $out1"; fi
if [ -x "$P1/usr/local/bin/ferrous-agent" ]; then ok; else bad "binary installed"; fi
v="$("$P1/usr/local/bin/ferrous-agent" --version 2>/dev/null)"
case "$v" in
  ferrous-agent\ *) ok ;;
  *) bad "installed binary runs (got [$v])" ;;
esac
UNIT="$P1/etc/systemd/system/ferrous-agent.service"
ENV1="$P1/etc/ferrous/agent.env"
if [ -f "$UNIT" ]; then ok; else bad "unit file written"; fi
grep -q "EnvironmentFile=$ENV1" "$UNIT" 2>/dev/null && ok || bad "unit points at env file"
grep -q "ExecStart=$P1/usr/local/bin/ferrous-agent" "$UNIT" 2>/dev/null && ok || bad "unit ExecStart"
grep -q "WantedBy=multi-user.target" "$UNIT" 2>/dev/null && ok || bad "unit install target"
grep -q "^FERROUS_TOKEN=testtok-123$" "$ENV1" && ok || bad "token in env file"
grep -q "^FERROUS_LISTEN=127.0.0.1:8710$" "$ENV1" && ok || bad "default listen in env file"
perms="$(stat -c %a "$ENV1" 2>/dev/null)"
if [ "$perms" = "600" ]; then ok; else bad "env file mode 600 (got [$perms])"; fi
case "$out1" in
  *"verify: curl"*) ok ;;
  *) bad "prints verify hint" ;;
esac

# --- 3. re-run never rotates the existing token
out2="$(sh "$INSTALL" --prefix "$P1" --from "$REPO/bin/ferrous-agent" 2>&1)"
case "$out2" in
  *"reusing existing token"*) ok ;;
  *) bad "re-run reuses existing token" ;;
esac
grep -q "^FERROUS_TOKEN=testtok-123$" "$ENV1" && ok || bad "token stable across re-runs"

# --- 4. fresh prefix, no --token → generated, announced, matches env file
P2="$tmp/root2"
out3="$(sh "$INSTALL" --prefix "$P2" --from "$REPO/bin/ferrous-agent" 2>&1)"
case "$out3" in
  *"generated API token"*) ok ;;
  *) bad "generated token announced" ;;
esac
gen="$(sed -n 's/^FERROUS_TOKEN=//p' "$P2/etc/ferrous/agent.env")"
if [ "${#gen}" -eq 64 ]; then ok; else bad "generated token is 64 hex chars (got ${#gen})"; fi
case "$out3" in
  *"$gen"*) ok ;;
  *) bad "generated token printed for the panel" ;;
esac

# --- 5. --listen override lands in the env file
P3="$tmp/root3"
sh "$INSTALL" --prefix "$P3" --from "$REPO/bin/ferrous-agent" --token t --listen 0.0.0.0:9999 >/dev/null 2>&1
grep -q "^FERROUS_LISTEN=0.0.0.0:9999$" "$P3/etc/ferrous/agent.env" && ok || bad "--listen lands in env file"

# --- 6. bad input fails loudly
sh "$INSTALL" --prefix "$tmp/root4" --from "$tmp/no-such-binary" >/dev/null 2>&1
if [ $? -ne 0 ]; then ok; else bad "--from missing file must fail"; fi
sh "$INSTALL" --prefix "$tmp/root4" --bogus >/dev/null 2>&1
if [ $? -ne 0 ]; then ok; else bad "unknown option must fail"; fi

# --- 7. no systemd touched in prefix mode (nothing was enabled/started)
case "$out1" in
  *"service installed"*) bad "prefix mode must not touch systemd" ;;
  *) ok ;;
esac

# --- 8. the real one-liner path: download from a release-style HTTP server
case "$(uname -m)" in
  x86_64|amd64) RARCH=amd64 ;;
  aarch64|arm64) RARCH=arm64 ;;
  *) RARCH="" ;;
esac
if [ -n "$RARCH" ] && command -v python3 >/dev/null 2>&1; then
  [ -f "$REPO/dist/ferrous-agent_linux_$RARCH.tar.gz" ] || (cd "$REPO" && make release >/dev/null 2>&1)
  (cd "$REPO/dist" && exec python3 -m http.server 8907 --bind 127.0.0.1 >/dev/null 2>&1) &
  httpd=$!
  for _ in $(seq 1 30); do
    kill -0 "$httpd" 2>/dev/null || break
    curl -sf -o /dev/null "http://127.0.0.1:8907/install.sh" && break
    sleep 0.2
  done
  P5="$tmp/root5"
  out5="$(FERROUS_RELEASE_BASE="http://127.0.0.1:8907" \
    sh "$INSTALL" --prefix "$P5" --token dl-tok 2>&1)"
  rc=$?
  kill "$httpd" 2>/dev/null
  wait "$httpd" 2>/dev/null
  if [ $rc -eq 0 ]; then ok; else bad "download install exits 0 (rc=$rc): $out5"; fi
  case "$out5" in
    *"downloading http://"*) ok ;;
    *) bad "download URL announced" ;;
  esac
  [ -x "$P5/usr/local/bin/ferrous-agent" ] && ok || bad "downloaded binary installed"
  grep -q "^FERROUS_TOKEN=dl-tok$" "$P5/etc/ferrous/agent.env" && ok || bad "token survives download path"
else
  echo "  (skip: no python3 or unsupported arch for the download test)"
fi

echo
echo "install tests: $passes passed, $fails failed"
[[ $fails -eq 0 ]]
