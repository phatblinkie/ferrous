#!/usr/bin/env bash
# Download the latest Oxide (uMod) build for Linux and apply it to the server
# dir. Port of the proven /mnt/storage/steamcmd/oxide-install.sh: paths
# parameterized, cache lives inside the data dir (persists across containers).
# Safe to run repeatedly. Original (unpatched) assemblies are backed up once.
set -euo pipefail

SERVER_DIR="${SERVER_DIR:-/server}"
CACHE="${OXIDE_CACHE:-$SERVER_DIR/.ferrous/oxide-cache}"
ORIG="$CACHE/original-assemblies"
mkdir -p "$CACHE" "$ORIG"

API="https://api.github.com/repos/OxideMod/Oxide.Rust/releases/latest"
RELEASE="$(curl -fsSL "$API")"
TAG="$(jq -r .tag_name <<<"$RELEASE")"
URL="$(jq -r '.assets[] | select(.name=="Oxide.Rust-linux.zip") | .browser_download_url' <<<"$RELEASE")"

if [[ -z "$URL" || "$URL" == "null" ]]; then
  echo "ERROR: no Oxide.Rust-linux.zip asset in release $TAG" >&2
  exit 1
fi

ZIP="$CACHE/Oxide.Rust-linux-$TAG.zip"
echo "Fetching Oxide $TAG ..."
curl -fsSL -o "$ZIP" "$URL"

# Back up pristine game assemblies before first patch (clean uninstall/diff).
for f in Assembly-CSharp.dll Assembly-CSharp-firstpass.dll; do
  if [[ -f "$SERVER_DIR/$f" && ! -f "$ORIG/$f.orig" && ! -f "$ORIG/$f" ]]; then
    cp -n "$SERVER_DIR/$f" "$ORIG/$f.orig"
    echo "Backed up original $f"
  fi
done

echo "Applying Oxide $TAG to $SERVER_DIR ..."
unzip -oq "$ZIP" -d "$SERVER_DIR"
echo "Oxide $TAG applied."
