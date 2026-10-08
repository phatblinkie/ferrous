#!/bin/sh
# ferrous agent installer — installs the agent as a systemd service.
#
# One-liner (against a published release):
#   curl -fsSL https://github.com/phatblinkie/ferrous/releases/latest/download/install.sh \
#     | sudo sh -s -- --token YOURTOKEN
# (set FERROUS_RELEASE_BASE to use a fork or self-hosted release)
#
# Options (each has a FERROUS_* env equivalent):
#   --token <t>    API bearer token. Default: FERROUS_TOKEN → token already in
#                  /etc/ferrous/agent.env (re-run safe) → generated and printed.
#   --listen <a>   listen address, default 127.0.0.1:8710 (remote panel access:
#                  bind 0.0.0.0 and firewall it, or front with a reverse proxy)
#   --from <path>  install this local binary instead of downloading
#   --base <url>   release base URL (default: FERROUS_RELEASE_BASE or the
#                  GitHub latest-download URL)
#   --no-start     install files only; do not touch systemd
#   --prefix <dir> test hook: install under <dir>, skips systemd
set -eu

BASE="${FERROUS_RELEASE_BASE:-https://github.com/phatblinkie/ferrous/releases/latest/download}"
TOKEN="${FERROUS_TOKEN:-}"
LISTEN="${FERROUS_LISTEN:-127.0.0.1:8710}"
FROM=""
START=1
PREFIX=""

usage() {
  cat <<'EOF'
ferrous agent installer

  sh install.sh [--token <t>] [--listen <addr>] [--from <path>]
                [--base <url>] [--no-start] [--prefix <dir>]

Installs:
  /usr/local/bin/ferrous-agent        the agent
  /etc/ferrous/agent.env              token + listen (mode 0600)
  /etc/systemd/system/ferrous-agent.service
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --token)   TOKEN="${2:?--token needs a value}"; shift 2 ;;
    --listen)  LISTEN="${2:?--listen needs a value}"; shift 2 ;;
    --from)    FROM="${2:?--from needs a value}"; shift 2 ;;
    --base)    BASE="${2:?--base needs a value}"; shift 2 ;;
    --prefix)  PREFIX="${2:?--prefix needs a value}"; shift 2 ;;
    --no-start) START=0; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "install.sh: unknown option: $1" >&2; usage >&2; exit 2 ;;
  esac
done

if [ "$(id -u)" -ne 0 ] && [ -z "$PREFIX" ]; then
  echo "install.sh: run as root (curl ... | sudo sh -s -- --token <t>)" >&2
  exit 1
fi

case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "install.sh: unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

BIN_DIR="$PREFIX/usr/local/bin"
CONF_DIR="$PREFIX/etc/ferrous"
UNIT_DIR="$PREFIX/etc/systemd/system"
ENV_FILE="$CONF_DIR/agent.env"

# token: explicit flag → whatever the existing install already uses (re-runs
# must never rotate it silently) → generate once and print it
GEN=0
if [ -z "$TOKEN" ] && [ -f "$ENV_FILE" ]; then
  TOKEN="$(sed -n 's/^FERROUS_TOKEN=//p' "$ENV_FILE" | head -n1)"
  if [ -n "$TOKEN" ]; then
    echo "reusing existing token from $ENV_FILE"
  fi
fi
if [ -z "$TOKEN" ]; then
  TOKEN="$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')"
  GEN=1
fi

# fetch the binary: local file (test/offline) or release tarball
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
if [ -n "$FROM" ]; then
  if [ ! -f "$FROM" ]; then
    echo "install.sh: --from: no such file: $FROM" >&2
    exit 1
  fi
  cp "$FROM" "$tmp/ferrous-agent"
else
  url="$BASE/ferrous-agent_linux_$ARCH.tar.gz"
  echo "downloading $url"
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$url" -o "$tmp/pkg.tar.gz"
  elif command -v wget >/dev/null 2>&1; then
    wget -qO "$tmp/pkg.tar.gz" "$url"
  else
    echo "install.sh: need curl or wget to download $url" >&2
    exit 1
  fi
  if ! tar -xzf "$tmp/pkg.tar.gz" -C "$tmp" 2>/dev/null || [ ! -f "$tmp/ferrous-agent" ]; then
    echo "install.sh: bad release package (want ferrous-agent at the tar root)" >&2
    exit 1
  fi
fi

install -d -m 0755 "$BIN_DIR" "$CONF_DIR" "$UNIT_DIR"
install -m 0755 "$tmp/ferrous-agent" "$BIN_DIR/ferrous-agent"

# the token lives only here — never world-readable
(umask 077; printf 'FERROUS_TOKEN=%s\nFERROUS_LISTEN=%s\n' "$TOKEN" "$LISTEN" > "$ENV_FILE")
chmod 0600 "$ENV_FILE"

cat > "$UNIT_DIR/ferrous-agent.service" <<UNIT
[Unit]
Description=ferrous agent (game-server control plane)
After=network-online.target docker.service
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=$CONF_DIR/agent.env
ExecStart=$BIN_DIR/ferrous-agent
Restart=on-failure
RestartSec=5
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
UNIT

if [ "$START" -eq 1 ] && [ -z "$PREFIX" ] && command -v systemctl >/dev/null 2>&1; then
  systemctl daemon-reload
  systemctl enable ferrous-agent.service
  if ! systemctl restart ferrous-agent.service; then
    echo "install.sh: service failed to start — check: systemctl status ferrous-agent" >&2
    exit 1
  fi
  echo "service installed, enabled and running (logs: journalctl -u ferrous-agent -f)"
else
  echo "files installed (systemd not touched):"
  echo "  $BIN_DIR/ferrous-agent   (env: $ENV_FILE)"
fi

ver="$("$BIN_DIR/ferrous-agent" --version 2>/dev/null | awk '{print $2}')"
echo "installed ferrous-agent ${ver:-unknown}"
if [ "$GEN" -eq 1 ]; then
  echo
  echo "generated API token — paste this in the panel (Hosts → add host):"
  echo "  $TOKEN"
fi
echo "verify: curl -H 'Authorization: Bearer <token>' http://127.0.0.1:8710/api/v1/ping"
