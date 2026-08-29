#!/usr/bin/env bash
# Single-step install+configure+launch for macOS and Linux.
# Everything lives inside this folder — nothing is installed system-wide.
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$DIR"

OS="$(uname -s)"
ARCH="$(uname -m)"

case "$OS" in
  Darwin) PLATFORM="darwin" ;;
  Linux)  PLATFORM="linux" ;;
  *)
    echo "Unsupported OS: $OS. Use start.bat on Windows or start-termux.sh on Android/Termux." >&2
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

BIN="$DIR/bin/aistation-${PLATFORM}-${GOARCH}"
if [ "$PLATFORM" = "linux" ] && [ "$GOARCH" = "armv7" ]; then
  BIN="$DIR/bin/aistation-linux-armv7"
fi

if [ ! -f "$BIN" ]; then
  echo "No prebuilt binary for ${PLATFORM}/${GOARCH} at $BIN" >&2
  echo "See TROUBLESHOOTING.md for how to build one from src/ with a portable Go toolchain." >&2
  exit 1
fi
chmod +x "$BIN" 2>/dev/null || true

exec "$BIN" --dir "$DIR" "$@"
