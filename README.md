# AI Workstation (portable, offline-first)

A self-contained AI chat + image-generation workstation that runs from this
folder — a USB drive, an external SSD, or a plain local directory — on
Windows, macOS, Linux, and Android (Termux). Nothing is installed
system-wide; everything the app needs (engines, models, history) lives in
this folder.

## Quick start

| Platform | Command |
|---|---|
| Windows | double-click `start.bat`, or run it from a terminal |
| macOS / Linux | `./start.sh` |
| Android (Termux) | `./start-termux.sh` |

One command installs (downloads the inference engines, with your approval),
configures, and launches — every time. The browser opens automatically to
the chat UI.

There are no settings to configure and no model to pick: type a message and
the system figures out which installed model to use, whether the request
needs text or an image, and what parameters to run it with.

## What's in this folder

```
bin/            prebuilt app binaries, one per OS/CPU — start scripts pick the right one
engines/        llama.cpp + stable-diffusion.cpp binaries (downloaded on first run)
models/text/    drop chat/vision GGUF models here — auto-detected, no setup
models/image/   drop image-generation GGUF or .safetensors checkpoints here
data/           chat history, model registry, app settings — all local, all yours
src/            Go source for the app itself (see "Rebuilding" below)
start.sh / start.bat / start-termux.sh
```

## Adding models

Drop a `.gguf` file into `models/text/` for a chat model, or a `.gguf`/
`.safetensors` file into `models/image/` for an image model, while the app
is running (or before starting it). It's fingerprinted and registered
automatically — no catalog to edit, no restart required.

**Vision (image-understanding) models**: a chat model needs its multimodal
projector (an `mmproj-*.gguf` file, usually published alongside the main
model) in `models/text/` too. The app pairs them automatically by matching
filenames — keep the base model and its projector's names similar (e.g.
`qwen2.5-vl-7b.gguf` and `qwen2.5-vl-7b-mmproj.gguf`).

If nothing installed fits a request (or the folders are empty), the chat UI
suggests a model sized for your hardware with a one-click download —
nothing is ever fetched without that click.

## How routing works (nothing to configure)

- **Text vs. image vs. both** is detected from what you type.
- **Which model** is chosen from what's installed, matched to your RAM/VRAM
  and to how complex the request looks (short/simple → smaller/faster
  model; long/complex → the best-fitting larger one).
- **Context length, output length, image resolution/steps** are inferred
  per request, never asked.
- On an out-of-memory or load failure, the app automatically retries with
  reduced settings, then falls back to the next-best installed model,
  before ever showing a raw error.
- **Image requests** are expanded automatically: the resident chat model
  turns a short request into a full, detailed image-generation prompt
  (subject, style, composition, lighting) before it reaches the image
  engine — you never have to write the "real" prompt yourself.

## Multiple chats at once

Every chat runs independently in the background — sending a message in one,
then switching to another (or starting a new one) never interrupts the
first. A small pulsing dot next to a chat's title in the sidebar means it's
still working even though you're looking at something else; switch back any
time and it'll be there, mid-response or finished. Which model answered is
shown as a small label above its response.

## Model usage

The **Model usage** tab (sidebar, next to Chat) shows which installed
models actually get used, most to least, based on real requests — not
guesswork. Nothing here is configurable; it's purely informational.

## Image generation timing

Image requests are given a 10-minute ceiling, no matter how slow the
hardware. The step count isn't a fixed guess — it's computed from this
machine's own measured speed at the requested resolution (re-measured after
every generation, so it adapts if conditions change), aiming to use as many
sampling steps as fit in the time available rather than defaulting to a
number that might be way too slow on this particular CPU. If the first
attempt still runs long, it retries once at a smaller resolution with
whatever time remains — always within the 10-minute total. On a GPU
(Vulkan-accelerated Windows/Linux builds are used automatically when one's
detected), this ceiling is rarely relevant; on CPU-only hardware with weak
floating-point throughput, expect it to lean on the smaller-resolution retry
more often — that's the hardware, not a setting to tune.

## Phone/tablet access

Click **📱 Phone access** in the sidebar for a QR code to the same UI over
your local network — it always encodes this machine's actual LAN address,
never `127.0.0.1` (which is meaningless to a phone). No cloud relay, no
account, no telemetry — the only network calls this app ever makes are the
engine/model downloads you approve.

## Rebuilding the app binaries

You don't need to — `bin/` already has binaries for Windows (x64/ARM64),
macOS (Intel/Apple Silicon), Linux (x64/ARM64/ARMv7), and Termux (ARM64/
ARMv7). If you ever need to rebuild after editing `src/`, see
`TROUBLESHOOTING.md`.

## Phase 2 (not built yet)

Auto-generated chat titles/tags/summaries beyond simple truncation, PDF
text extraction and OCR for scanned attachments, scheduled (rather than
on-demand) re-suggestion of better-fit models as your hardware or the
catalog changes, optional local-passphrase encryption of chat history,
`update.sh`/`update.bat`/`update-termux.sh` maintenance scripts, an optional
self-update check, and LLM-based (rather than keyword-based) text-vs-image
intent detection.
