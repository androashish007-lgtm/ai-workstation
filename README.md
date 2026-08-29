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
nothing is ever fetched without that click. Sending a message with an
attached image while no vision-capable model (see above) is installed
works the same way: you get a clear explanation and a suggested vision
model to download, saved to the chat so it's there even if you don't catch
it live, rather than a reply that ignores the image.

## Attaching images

Click 📎 to attach one or more images to a message — each shows as its own
thumbnail with an ✕ to remove it before sending. All of them go to the
model together as part of that one message.

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

Every message shows the time it was sent or received underneath its bubble.

## Stopping a response mid-generation

While a chat is generating, the **Send** button turns into a red **Stop**
button — click it to cancel immediately. Text generation stops after its
current token; image generation kills the render outright. Whatever text
had already streamed in stays on screen, marked *(stopped)*, and the chat
is saved as-is so you can pick up from there with a follow-up message.

## Deleting chats

Hover a chat in the sidebar for a **✕** to delete it (with confirmation).
**🗑 Delete all chats** in the sidebar footer clears everything at once —
any chat still generating a response is skipped rather than force-stopped,
and you're told how many were skipped.

## Projects (grouped chats with shared context)

Click **+ Project** to create a named group with shared notes/instructions
— e.g. "Building a portfolio site for a photographer, minimal dark theme."
Every chat you create inside that project (via the **+** on its group
header) automatically gets those notes as background context, on top of
its own independent conversation — so you don't have to repeat the same
background in every chat. Chats keep entirely separate histories; only the
shared notes are common. Editing a project's notes (✎) affects every chat
in it going forward; deleting a project ungroups its chats rather than
deleting them.

Existing chats can be moved in or out of a project any time — hover a chat
for the **📁** icon and pick a project (or "No project" to ungroup).

## Model usage

The **Model usage** tab (sidebar, next to Chat) lists every model actually
installed on disk — not just ones that happen to have been used yet —
grouped by `models/text/` and `models/image/`. Each one shows its file
size, a one-line description of what it's actually good for (vision-capable
chat, fast vs. high-quality chat, SD1.5 vs. SDXL image generation, editing
checkpoints that need a reference image, etc.), and how many times it's
really been used. A 🗑 next to each one deletes that file from disk (with a
confirmation) — useful for clearing out a model that turned out not to be
worth the space.

## GPU detection

On Windows, GPU vendor/name is read via PowerShell's `Get-CimInstance` (the
`GPU vendor`/`GPU name` line in the startup log confirms what was found).
When a GPU is detected and Vulkan is available, both engines automatically
download the Vulkan-accelerated build instead of CPU-only — this covers
Intel integrated graphics (including Iris Xe), AMD, and NVIDIA alike from
one backend, rather than needing a vendor-specific toolkit like CUDA or
OpenVINO. The status badge (top-right) shows live GPU utilization next to
CPU/RAM when a GPU is present, sampled every ~6s (the underlying Windows
counter is inherently slow to query, so it's deliberately not on the
same 1s cadence as CPU/RAM).

If you already had engines downloaded before upgrading to a version with
this fix, delete `engines/<platform>/` once to force a fresh bootstrap —
otherwise the app keeps using whatever was already downloaded.

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
your local network — it always encodes this machine's actual LAN address
(a real `192.168.x.x`/`10.x.x.x`-style private address, the same one
`ipconfig`/`ifconfig` shows), never `127.0.0.1` (which is meaningless to a
phone) and never a link-local `169.254.x.x` fallback address. No cloud
relay, no account, no telemetry — the only network calls this app ever
makes are the engine/model downloads you approve. If your phone still
can't connect, see "Phone/tablet can't reach the LAN URL or QR code" in
`TROUBLESHOOTING.md` — it's almost always a Windows Firewall prompt that
was dismissed rather than allowed.

The app listens on port **2222** by default (`--port <n>` to change it).

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

**Flux image model support**: the image engine invocation currently only
passes a single checkpoint file (`-m <model>`), which is all SD1.5/SDXL
single-file checkpoints need. Flux (dev/schnell) is a meaningfully higher
quality option but needs 3 additional weight files loaded alongside the
main diffusion model — a VAE, a CLIP-L text encoder, and a T5-XXL text
encoder (itself several GB) — so this needs: extra CLI flags in
`internal/engine/image.go`, a way for the registry to track/pair those
companion files (similar to the existing vision-projector auto-pairing),
and a catalog entry bundling all four files with a combined size that still
fits a reasonable hardware budget.
