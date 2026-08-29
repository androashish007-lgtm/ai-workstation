#!/data/data/com.termux/files/usr/bin/bash
# Single-step install+configure+launch for Android via Termux.
# `pkg` here only ever installs into Termux's own $PREFIX (user-space,
# no root) — it never touches the rest of the Android system, so it stays
# within "no system-wide install."
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$DIR"

ARCH="$(uname -m)"
case "$ARCH" in
  aarch64|arm64) BIN="$DIR/bin/aistation-linux-arm64" ;;
  armv7l|armv8l) BIN="$DIR/bin/aistation-linux-armv7" ;;
  *)
    echo "Unsupported device architecture: $ARCH" >&2
    exit 1
    ;;
esac

if [ ! -f "$BIN" ]; then
  echo "Missing binary: $BIN" >&2
  exit 1
fi
chmod +x "$BIN" 2>/dev/null || true

echo "Starting AI Workstation..."
echo "First run may pause to ask approval for a one-time engine download or"
echo "on-device build (Termux has no prebuilt stable-diffusion.cpp, so image"
echo "generation is compiled locally with clang/cmake the first time it's used)."
echo

exec "$BIN" --dir "$DIR" "$@"
