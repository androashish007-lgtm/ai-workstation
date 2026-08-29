# Troubleshooting

## "No prebuilt binary found for <platform>/<arch>"

`bin/` ships binaries for: Windows x64 + ARM64, macOS Intel + Apple Silicon,
Linux x64 + ARM64 + ARMv7, and the same Linux ARM64/ARMv7 binaries run
under Termux. If your device genuinely isn't covered (e.g. Windows x86
32-bit, or a Linux distro on an unusual architecture), you'll need a Go
toolchain to rebuild:

1. Install Go temporarily (or use a portable copy — see
   [go.dev/dl](https://go.dev/dl/), the `.tar.gz`/`.zip` archives need no
   installer, just extract and put `go/bin` on `PATH` for the session).
2. From `src/`, run:
   ```
   CGO_ENABLED=0 GOOS=<target-os> GOARCH=<target-arch> go build -o ../bin/aistation-<target-os>-<target-arch> ./cmd/aistation
   ```
3. Re-run the platform's start script.

## Engine not found / bootstrap keeps asking for approval

The first time text or image generation is needed, the app downloads the
matching `llama.cpp` / `stable-diffusion.cpp` release for your OS/CPU (or,
on Termux, builds `stable-diffusion.cpp` on-device since no prebuilt Android
binary exists). This needs one approval — either type `y` at the terminal
prompt, or click **Approve** on the card that appears above the chat input.
If you dismissed both, just send another message to re-trigger the prompt.

If the download keeps failing: check your internet connection (engine
bootstrap is the one time this app needs it, besides model downloads), or
check `engines/_downloads/` for a partial file and delete it to force a
clean retry.

## Termux: image generation is very slow to start the first time

There's no prebuilt `stable-diffusion.cpp` binary for Android, so Termux
compiles it on-device the first time an image is requested — this can take
10–30+ minutes on a phone CPU and is a one-time cost (the binary is cached
in `engines/termux-<arch>/image/` afterward). `pkg install clang cmake` may
also run first if those tools aren't already present; this installs only
into Termux's own `$PREFIX`, not the Android system.

If the text engine's prebuilt Android binary fails to run (some devices'
process/security model rejects it), the app automatically falls back to the
same kind of on-device build for `llama.cpp` — you'll see a log line saying
so, and a similar one-time wait.

## Port already in use

By default the app tries port 8420 and lets the OS pick a free one if
that's unavailable — the terminal banner and browser always show the port
actually in use. If you need a specific port, run the start script with
`--port <n>` appended (e.g. `start.sh --port 9000`).

## Out of memory / model won't load

The app already tries reduced settings and then a smaller installed model
automatically before giving up. If every installed model still fails to
load, your smallest installed model is likely too large for this machine —
delete it from `models/text/` (or `models/image/`) and let the chat UI's
"no model installed" suggestion offer one sized correctly for your
hardware.

## Permission errors per OS

- **Windows**: if the binary won't run from a USB drive, right-click it →
  Properties → check for an "Unblock" checkbox (SmartScreen sometimes flags
  binaries copied from removable media) and check it.
- **macOS**: Gatekeeper may block an unsigned binary the first time —
  right-click the binary in `bin/` → Open, once, to approve it, or run
  `xattr -d com.apple.quarantine bin/aistation-darwin-*` from a terminal.
- **Linux/Termux**: if `start.sh`/`start-termux.sh` won't execute, run
  `chmod +x start.sh bin/aistation-*` once (drives formatted exFAT/NTFS and
  mounted from Windows sometimes drop the executable bit).

## "A response is already being generated for this chat"

Each chat can only have one response in flight at a time (sending a second
message before the first finishes would garble both). This is unrelated to
running multiple *different* chats at once, which is fully supported —
switching to another chat, or opening a new one, and sending there works
immediately without waiting.

## Image generation is still slow even after the timing fix

The step count adapts to this machine's own measured speed, but that speed
is itself a hardware fact the app can't change — on CPU-only hardware with
weak floating-point throughput (common on virtualized/cloud/shared-core
environments, and some laptop CPUs under thermal limits), even a well-tuned
step count can mean a low-resolution result rather than a fast high-res one.
Deleting `data/image-perf.json` resets the measurement (useful if you've
since freed up CPU load elsewhere and want it to re-measure), but the
biggest lever is real GPU acceleration if your hardware has one — check
`data/config.json`-adjacent engine logs for which backend (`cpu` vs
`vulkan`) actually got selected.

## Where things are stored

Everything is under this folder: `data/sessions/*.json` (chat history),
`data/registry.json` (detected models), `data/config.json` (last
session/settings), `data/model-usage.json` (the Model usage tab's counts),
`data/image-perf.json` (this machine's measured image-generation speed,
self-calibrating), `models/text/` and `models/image/` (your model files),
`engines/` (downloaded/built engine binaries — safe to delete to force a
fresh re-download). Deleting `data/` resets history and settings but never
touches your models.
