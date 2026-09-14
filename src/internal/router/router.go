// Package router makes every decision the user is never asked to make:
// text vs image vs mixed intent, which installed model best fits the
// request and the hardware, and what context/resolution/step parameters to
// use. Nothing here is exposed as a setting — it's re-evaluated per request.
package router

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"aistation/internal/hw"
	"aistation/internal/registry"
)

type Intent string

const (
	IntentText  Intent = "text"
	IntentImage Intent = "image"
	IntentMixed Intent = "mixed" // e.g. "describe this, then draw it"
)

var imageVerbs = regexp.MustCompile(`(?i)\b(draw|paint|sketch|render|generate|create|make|design)\b.{0,40}\b(image|picture|photo|art|illustration|drawing|painting|logo|icon|wallpaper|poster|scene)\b`)
var imageNouns = regexp.MustCompile(`(?i)\b(a picture of|an image of|a photo of|a drawing of|a painting of)\b`)

// editVerbs matches phrasing that asks to modify an attached image rather
// than either generate a new one from scratch or describe it — "recreate
// the image, remove the cap" is the case that motivated this: it contains
// neither an imageVerbs match ("recreate" isn't one of the generation
// verbs) nor an analysis phrase, so without this it fell through to plain
// IntentText and got treated as "describe this image" instead of "edit
// this image." Only checked when an image is actually attached (see
// ClassifyIntent) — these verbs are common enough in plain text that
// without that guard they'd misfire constantly.
var editVerbs = regexp.MustCompile(`(?i)\b(edit|modify|recreate|retouch|touch up|alter|adjust|redo|transform|remove|erase|delete|replace|swap|recolor|colorize|change)\b`)

// ClassifyIntent is a cheap heuristic first pass (regex, not a model call) —
// good enough for the common phrasings; routing every message through the
// LLM itself for intent classification is a Phase 2 refinement once this
// baseline is proven in daily use.
func ClassifyIntent(text string, hasImageAttachment bool) Intent {
	wantsImage := imageVerbs.MatchString(text) || imageNouns.MatchString(text)
	wantsEdit := hasImageAttachment && editVerbs.MatchString(text)
	mentionsAnalysis := hasImageAttachment && (strings.Contains(strings.ToLower(text), "describe") ||
		strings.Contains(strings.ToLower(text), "what is") || strings.Contains(strings.ToLower(text), "explain"))
	switch {
	case wantsEdit:
		return IntentImage
	case wantsImage && mentionsAnalysis:
		return IntentMixed
	case wantsImage:
		return IntentImage
	default:
		return IntentText
	}
}

type Complexity string

const (
	ComplexitySimple  Complexity = "simple"
	ComplexityComplex Complexity = "complex"
)

// ClassifyComplexity is a cheap proxy for "does this need the bigger model":
// long prompts, code fences, and multi-part/analytical phrasing skew
// complex; short conversational prompts skew simple. Good enough to route
// without a model call of its own — a wrong call here just means a slightly
// bigger or smaller model answers, never a failure.
func ClassifyComplexity(text string) Complexity {
	trimmed := strings.TrimSpace(text)
	words := len(strings.Fields(trimmed))
	signals := 0
	if words > 40 {
		signals++
	}
	if strings.Contains(trimmed, "```") {
		signals++
	}
	for _, kw := range []string{"analyze", "compare", "explain in detail", "step by step", "architecture", "design", "debug", "refactor", "prove", "derive"} {
		if strings.Contains(strings.ToLower(trimmed), kw) {
			signals++
			break
		}
	}
	if strings.Count(trimmed, "?") > 1 {
		signals++
	}
	if signals >= 1 {
		return ComplexityComplex
	}
	return ComplexitySimple
}

// SelectTextModel picks the best installed text model for the given
// complexity that fits within the hardware budget: the largest (by file
// size, our proxy for capability) candidate that fits, or if none fit, the
// smallest available as a last-resort fallback rather than refusing to
// answer.
//
// preferredID, when non-empty, is a user's explicit override (picked from
// the model dropdown instead of leaving it on "Auto") — if it's among the
// candidates that already pass the kind/vision/exclude filters, it wins
// outright, skipping the size/budget heuristic entirely; a user's deliberate
// pick isn't second-guessed against the hardware budget the way the
// automatic fallback is. If the preferred model isn't in that filtered set
// (deleted since, wrong kind, doesn't support vision when this turn needs
// it, or already tried and failed this turn), this falls through to the
// normal automatic selection rather than erroring.
func SelectTextModel(models []registry.Model, profile hw.Profile, complexity Complexity, needVision bool, exclude map[string]bool, preferredID string) (*registry.Model, error) {
	var candidates []registry.Model
	for _, m := range models {
		if m.Kind != registry.KindText || m.IsVisionProjector {
			continue
		}
		if needVision && !m.VisionCapable() {
			continue
		}
		if exclude != nil && exclude[m.ID] {
			continue
		}
		candidates = append(candidates, m)
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no installed text model available")
	}
	if preferredID != "" {
		for _, m := range candidates {
			if m.ID == preferredID {
				picked := m
				return &picked, nil
			}
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].SizeBytes < candidates[j].SizeBytes })

	budget := profile.BudgetBytes()
	var fitting []registry.Model
	for _, m := range candidates {
		if budget == 0 || uint64(m.SizeBytes) <= budget {
			fitting = append(fitting, m)
		}
	}
	if len(fitting) == 0 {
		// Nothing comfortably fits; fall back to the smallest installed
		// model rather than failing outright — self-healing over refusal.
		m := candidates[0]
		return &m, nil
	}
	if complexity == ComplexitySimple {
		m := fitting[0] // smallest that fits: fastest for a simple request
		return &m, nil
	}
	m := fitting[len(fitting)-1] // largest that fits: best quality for a complex request
	return &m, nil
}

// editingModelHints: filename substrings that mark a checkpoint as trained
// for conditioned image editing (pix2pix, inpainting, controlnet) rather
// than plain text-to-image — these need an input image/mask to make sense
// and produce poor results on a bare text prompt, so they're only picked
// when nothing else installed fits the request.
var editingModelHints = []string{"pix2pix", "inpaint", "controlnet", "img2img"}

func LooksLikeEditingModel(filename string) bool {
	f := strings.ToLower(filename)
	for _, hint := range editingModelHints {
		if strings.Contains(f, hint) {
			return true
		}
	}
	return false
}

// SelectImageModel picks the largest installed plain text-to-image model
// that fits the hardware budget. Conditioned editing checkpoints like
// pix2pix/inpainting (see LooksLikeEditingModel) are deprioritized to a
// last resort for a plain generate-from-scratch request — but preferred
// first when preferEditing is true (the request is actually editing an
// attached image), since those are exactly the models built for that job.
//
// preferredID is the same user-override mechanism as SelectTextModel's: if
// it names an installed, not-yet-excluded image model, that model is used
// outright (bypassing both the budget check and the editing-model
// preference/deprioritization — an explicit pick is trusted as-is),
// otherwise this falls through to the automatic pick.
func SelectImageModel(models []registry.Model, profile hw.Profile, exclude map[string]bool, preferredID string, preferEditing bool) (*registry.Model, error) {
	var candidates, editingCandidates []registry.Model
	for _, m := range models {
		if m.Kind != registry.KindImage || (exclude != nil && exclude[m.ID]) {
			continue
		}
		// VAE/text-encoder component files are never themselves
		// generatable, and a FLUX.2 checkpoint missing either of its
		// paired component files can't run at all — both are excluded
		// from the candidate pool entirely, not just deprioritized, since
		// picking one would only guarantee an immediate failure.
		if m.IsImageComponent() || !m.FluxReady() {
			continue
		}
		if preferredID != "" && m.ID == preferredID {
			picked := m
			return &picked, nil
		}
		if LooksLikeEditingModel(m.Filename) {
			editingCandidates = append(editingCandidates, m)
		} else {
			candidates = append(candidates, m)
		}
	}
	if preferEditing && len(editingCandidates) > 0 {
		candidates = editingCandidates
	} else if len(candidates) == 0 {
		candidates = editingCandidates
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no installed image model available")
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].SizeBytes < candidates[j].SizeBytes })
	budget := profile.BudgetBytes()
	var fitting []registry.Model
	for _, m := range candidates {
		if budget == 0 || uint64(m.SizeBytes) <= budget {
			fitting = append(fitting, m)
		}
	}
	if len(fitting) == 0 {
		m := candidates[0]
		return &m, nil
	}
	m := fitting[len(fitting)-1]
	return &m, nil
}

// TextParams are inferred, never user-set.
type TextParams struct {
	ContextTokens int
	MaxTokens     int
	Temperature   float64
	GPULayers     int
}

// InferTextParams derives a context window from the model's trained
// context length (capped to what the hardware can comfortably hold) and a
// max-output-tokens budget from how long/complex the prompt looks.
func InferTextParams(model registry.Model, profile hw.Profile, promptLen int, complexity Complexity) TextParams {
	ctx := model.ContextLength
	if ctx <= 0 {
		ctx = 4096
	}
	// Rough RAM-per-context-token headroom check: KV cache scales with
	// context size, so on very constrained hardware we cap it well below
	// the model's trained maximum. This is intentionally conservative —
	// the OOM-retry path in the server package steps it down further if
	// this guess is still too high.
	budget := profile.BudgetBytes()
	if budget > 0 && uint64(model.SizeBytes) > 0 {
		headroom := budget - uint64(model.SizeBytes)
		maxCtxByRAM := int(headroom / (256 * 1024)) // ~256KB/token rough KV-cache heuristic
		if maxCtxByRAM > 0 && maxCtxByRAM < ctx {
			ctx = maxCtxByRAM
		}
	}
	if ctx < 1024 {
		ctx = 1024
	}
	if ctx > 32768 {
		ctx = 32768 // sane ceiling regardless of the model's advertised max
	}

	maxTokens := 512
	if complexity == ComplexityComplex {
		maxTokens = 1536
	}
	if promptLen > 2000 {
		maxTokens += 512
	}

	gpuLayers := 0
	if profile.GPUVendor != hw.GPUNone {
		gpuLayers = 999 // offload as much as fits; llama.cpp clamps automatically
	}

	return TextParams{ContextTokens: ctx, MaxTokens: maxTokens, Temperature: 0.7, GPULayers: gpuLayers}
}

// StepDown produces a smaller, safer parameter set after a failure that
// isn't specifically a GPU allocation error (see StepDownGPU for that
// case) — shrinking context/output-length trims the CPU-RAM footprint
// (KV-cache scales with context size) without touching GPU offload.
func (p TextParams) StepDown() TextParams {
	next := p
	next.ContextTokens = maxInt(1024, p.ContextTokens/2)
	next.MaxTokens = maxInt(256, p.MaxTokens/2)
	return next
}

// gpuLayerLadder is the sequence of --n-gpu-layers values tried after a
// GPU allocation failure, most to least aggressive. These are absolute
// layer counts, not fractions of the model's real layer count (which this
// process never sees — llama.cpp clamps a too-large request down to
// "all layers" internally) — so each rung is just "meaningfully less than
// the last", ending at 0 (CPU-only), which is guaranteed to work given
// enough system RAM. 999 (the conventional "offload everything" sentinel)
// is intentionally NOT repeated here since StepDownGPU is only ever
// called after that first full-offload attempt already failed.
var gpuLayerLadder = []int{20, 10, 5, 2, 0}

// StepDownGPU tries the next rung down the GPU-offload ladder rather than
// halving: how much a GPU backend can actually allocate isn't something
// this process can query (Vulkan exposes no reliable "free memory" check,
// especially on an integrated GPU where the real ceiling is whatever the
// driver/BIOS decided to expose — often far less than total system RAM),
// and llama.cpp/Vulkan hard-crashes on an allocation failure rather than
// gracefully falling back on its own. A blind halving from a large
// starting value (e.g. 999 -> 499) can still be too big and just repeats
// the same crash; stepping through explicit small rungs down to 0
// guarantees eventual success while still giving a real chance of
// keeping *some* GPU acceleration rather than jumping straight to
// CPU-only. done reports true once the ladder is exhausted (already at
// 0) — the caller should fall through to StepDown()'s context-trimming
// route instead of retrying a GPU config again.
func (p TextParams) StepDownGPU() (TextParams, bool) {
	next := p
	for _, rung := range gpuLayerLadder {
		if rung < p.GPULayers {
			next.GPULayers = rung
			return next, false
		}
	}
	next.GPULayers = 0
	return next, p.GPULayers == 0
}

// IsGPUAllocationFailure reports whether an error looks like the engine
// crashed trying to allocate GPU memory, as opposed to a general
// system-RAM/CPU problem — the two need different responses (retry with
// less GPU offload vs. retry with a smaller context/output size), and a
// plain substring match on the captured process output is the most
// reliable signal available (llama.cpp/Vulkan doesn't expose a typed
// error here, just this text on stderr).
func IsGPUAllocationFailure(errText string) bool {
	t := strings.ToLower(errText)
	for _, sig := range []string{
		"vulkan", "out of device memory", "outofdevicememory",
		"ggml_vulkan", "cuda out of memory", "cuda error",
	} {
		if strings.Contains(t, sig) {
			return true
		}
	}
	return false
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ImageParams are inferred from the model family and hardware, never
// user-set.
type ImageParams struct {
	Width, Height, Steps int
	// CFGScale, when non-zero, is passed as --cfg-scale. Left 0 (meaning
	// "don't pass the flag, let sd-cli use its own default") for every
	// family except FLUX/FLUX.2: those are guidance-distilled and expect
	// cfg-scale 1.0 — sd-cli's un-flagged default (tuned for classic
	// CFG-driven SD/SDXL models) would otherwise over-guide and degrade
	// output badly on a Flux checkpoint.
	CFGScale float64
	// Strength/ImgCFGScale are only set when editing an attached image
	// (see InferEditParams) — 0 means "not an edit, omit both flags."
	Strength    float64
	ImgCFGScale float64
}

// InferEditParams picks --strength/--img-cfg-scale for editing an attached
// image, split by whether the selected model was actually built for
// instruction-based editing (see LooksLikeEditingModel) or is a generic
// SD/SDXL checkpoint being asked to do img2img anyway.
//
// --strength is noising strength, NOT "how strongly to apply the image
// conditioning" — stable-diffusion.cpp's own --help is explicit that 1.0
// means full destruction of the init image's information. 0.55 for an
// editing model is an empirically-tuned middle ground, not a documented
// default: real end-to-end testing against instruct-pix2pix walked through
// 1.0 (output was a blank/garbled blob — the init image's information was
// completely gone), 0.9 (same failure), 0.75 (recognizable but heavily
// degraded), and 0.4 (the original scene came through clearly, but the
// requested edit barely applied at all — too little noise for the
// instruction to take hold). 0.55 sits between the "edit applies" and
// "image survives" failure modes; it is a compromise, not a value that
// reliably produces a clean result — the installed
// instruct-pix2pix-00-22000-pruned-fp16 checkpoint (an old, small,
// community-pruned conversion) is simply inconsistent at this kind of
// precise photorealistic edit regardless of parameters. A generic
// (non-editing) model gets a lower value still (0.4) since it has no
// instruction-following conditioning to lean on at all — more of the
// original needs to survive the noising for the prompt alone to land as a
// targeted edit rather than a new, unrelated image that happens to reuse
// the canvas size.
//
// --img-cfg-scale (image guidance scale, separate from the regular text
// --cfg-scale) only applies to editing models — a generic model has no
// separate image-conditioning path for it to scale, so it's left 0
// (omitted) there. 1.5 for editing models matches the commonly-documented
// InstructPix2Pix default pairing (~7.5 text / ~1.5 image guidance) —
// noticeably lower than text guidance so the edit instruction can actually
// take effect instead of being dominated by "stay close to the input."
func InferEditParams(model registry.Model) (strength, imgCFGScale float64) {
	if LooksLikeEditingModel(model.Filename) {
		return 0.55, 1.5
	}
	return 0.4, 0
}

// UsesReferenceImageEditing reports whether editing an attached image with
// this model should use sd-cli's -r/--ref-image (FLUX Kontext/FLUX.2's own
// instruction-following reference-conditioning) instead of -i/--init-img's
// SDEdit-style renoising (see InferEditParams). Confirmed against a real
// test: pointing a FLUX.2 edit request at -i produced an image that
// resembled the attached photo but completely ignored the text
// instruction — -i has no "follow this instruction" concept, it just
// partially renoises and redraws; -r is FLUX's actual edit path.
func UsesReferenceImageEditing(model registry.Model) bool {
	return model.ImageFamily == registry.ImageFamilyFlux2
}

func InferImageParams(model registry.Model, profile hw.Profile) ImageParams {
	width, height := 512, 512
	steps := 20
	cfgScale := 0.0
	switch model.ImageFamily {
	case registry.ImageFamilySDXL, registry.ImageFamilyFlux, registry.ImageFamilyFlux2, registry.ImageFamilySD3:
		width, height = 1024, 1024
	case registry.ImageFamilySD2:
		width, height = 768, 768
	}
	if model.ImageFamily == registry.ImageFamilyFlux || model.ImageFamily == registry.ImageFamilyFlux2 {
		cfgScale = 1.0
	}
	// CPU-only, low-RAM hardware: trade resolution/steps for a response
	// that finishes in a reasonable time instead of a multi-minute wait.
	if profile.GPUVendor == hw.GPUNone {
		if width > 512 {
			width, height = 512, 512
		}
		steps = 15
	}
	return ImageParams{Width: width, Height: height, Steps: steps, CFGScale: cfgScale}
}

// StepDown produces smaller/faster image parameters after a failure
// (OOM, timeout) so a retry has a real chance of succeeding.
func (p ImageParams) StepDown() ImageParams {
	next := p
	if next.Width > 384 {
		next.Width -= 128
		next.Height -= 128
	}
	if next.Steps > 10 {
		next.Steps -= 5
	}
	return next
}
