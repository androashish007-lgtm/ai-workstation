#!/data/data/com.termux/files/usr/bin/bash
# Fetches the latest published VERSION and this device's binary from the
# repo's default branch and replaces the local copies — same approach as
# update.sh, just Termux's shebang and binary-name mapping (see
# start-termux.sh for why Termux maps architectures to the linux-* binaries).
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$DIR"

REPO_RAW="https://raw.githubusercontent.com/androashish007-lgtm/ai-workstation/master"

ARCH="$(uname -m)"
case "$ARCH" in
  aarch64|arm64) BIN_NAME="aistation-linux-arm64" ;;
  armv7l|armv8l) BIN_NAME="aistation-linux-armv7" ;;
  *)
    echo "Unsupported device architecture: $ARCH" >&2
    exit 1
    ;;
esac

if ! command -v curl >/dev/null 2>&1; then
  echo "Need curl to check for updates — run: pkg install curl" >&2
  exit 1
fi

CURRENT_VERSION="$(cat VERSION 2>/dev/null || echo unknown)"
LATEST_VERSION="$(curl -fsSL "$REPO_RAW/VERSION" | tr -d '[:space:]')"

if [ -z "$LATEST_VERSION" ]; then
  echo "Could not read the latest version from GitHub." >&2
  exit 1
fi
if [ "$LATEST_VERSION" = "$CURRENT_VERSION" ]; then
  echo "Already up to date (v$CURRENT_VERSION)."
  exit 0
fi

echo "Updating v$CURRENT_VERSION -> v$LATEST_VERSION..."
curl -fsSL "$REPO_RAW/bin/${BIN_NAME}" -o "bin/${BIN_NAME}.new"
chmod +x "bin/${BIN_NAME}.new"
mv -f "bin/${BIN_NAME}.new" "bin/${BIN_NAME}"
echo "$LATEST_VERSION" > VERSION
echo "Updated to v$LATEST_VERSION. Run start-termux.sh to launch it."
