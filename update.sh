#!/usr/bin/env bash
# Fetches the latest published VERSION and this platform's binary from the
# repo's default branch and replaces the local copies. There's no CI/release
# pipeline for this project yet — binaries live directly in bin/ on the
# default branch rather than as GitHub Release assets, so this pulls the
# raw files directly instead of using the GitHub Releases API.
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$DIR"

REPO_RAW="https://raw.githubusercontent.com/androashish007-lgtm/ai-workstation/master"

OS="$(uname -s)"
ARCH="$(uname -m)"
case "$OS" in
  Darwin) PLATFORM="darwin" ;;
  Linux)  PLATFORM="linux" ;;
  *)
    echo "Unsupported OS: $OS. Use update.bat on Windows or update-termux.sh on Android/Termux." >&2
    exit 1
    ;;
esac
case "$ARCH" in
  x86_64|amd64) GOARCH="amd64" ;;
  arm64|aarch64) GOARCH="arm64" ;;
  armv7l|armv6l) GOARCH="armv7" ;;
  *)
    echo "Unsupported CPU architecture: $ARCH" >&2
    exit 1
    ;;
esac
BIN_NAME="aistation-${PLATFORM}-${GOARCH}"
[ "$PLATFORM" = "linux" ] && [ "$GOARCH" = "armv7" ] && BIN_NAME="aistation-linux-armv7"

fetch() {
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$1" -o "$2"
  elif command -v wget >/dev/null 2>&1; then
    wget -q "$1" -O "$2"
  else
    echo "Need curl or wget to check for updates." >&2
    exit 1
  fi
}

CURRENT_VERSION="$(cat VERSION 2>/dev/null || echo unknown)"
TMP_VERSION="$(mktemp)"
trap 'rm -f "$TMP_VERSION"' EXIT
fetch "$REPO_RAW/VERSION" "$TMP_VERSION"
LATEST_VERSION="$(tr -d '[:space:]' < "$TMP_VERSION")"

if [ -z "$LATEST_VERSION" ]; then
  echo "Could not read the latest version from GitHub." >&2
  exit 1
fi
if [ "$LATEST_VERSION" = "$CURRENT_VERSION" ]; then
  echo "Already up to date (v$CURRENT_VERSION)."
  exit 0
fi

echo "Updating v$CURRENT_VERSION -> v$LATEST_VERSION..."
TMP_BIN="bin/${BIN_NAME}.new"
fetch "$REPO_RAW/bin/${BIN_NAME}" "$TMP_BIN"
chmod +x "$TMP_BIN"
mv -f "$TMP_BIN" "bin/${BIN_NAME}" # atomic rename — safe even if the old binary is currently running
echo "$LATEST_VERSION" > VERSION
echo "Updated to v$LATEST_VERSION. Run start.sh to launch it."
