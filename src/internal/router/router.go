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

// ClassifyIntent is a cheap heuristic first pass (regex, not a model call) —
// good enough for the common phrasings; routing every message through the
// LLM itself for intent classification is a Phase 2 refinement once this
// baseline is proven in daily use.
func ClassifyIntent(text string, hasImageAttachment bool) Intent {
	wantsImage := imageVerbs.MatchString(text) || imageNouns.MatchString(text)
	mentionsAnalysis := hasImageAttachment && (strings.Contains(strings.ToLower(text), "describe") ||
		strings.Contains(strings.ToLower(text), "what is") || strings.Contains(strings.ToLower(text), "explain"))
	switch {
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
func SelectTextModel(models []registry.Model, profile hw.Profile, complexity Complexity, needVision bool, exclude map[string]bool) (*registry.Model, error) {
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

func looksLikeEditingModel(filename string) bool {
	f := strings.ToLower(filename)
	for _, hint := range editingModelHints {
		if strings.Contains(f, hint) {
			return true
		}
	}
	return false
}

// SelectImageModel picks the largest installed plain text-to-image model
// that fits the hardware budget (conditioned editing checkpoints like
// pix2pix/inpainting are deprioritized to a last resort — see
// looksLikeEditingModel), since Phase 1 doesn't yet infer style/quality
// tiers beyond "best the hardware can run."
func SelectImageModel(models []registry.Model, profile hw.Profile, exclude map[string]bool) (*registry.Model, error) {
	var candidates, editingCandidates []registry.Model
	for _, m := range models {
		if m.Kind != registry.KindImage || (exclude != nil && exclude[m.ID]) {
			continue
		}
		if looksLikeEditingModel(m.Filename) {
			editingCandidates = append(editingCandidates, m)
		} else {
			candidates = append(candidates, m)
		}
	}
	if len(candidates) == 0 {
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

// StepDown produces a smaller, safer parameter set after an OOM/load
// failure, so the caller can retry once automatically instead of
// surfacing a raw error.
func (p TextParams) StepDown() TextParams {
	next := p
	next.ContextTokens = maxInt(1024, p.ContextTokens/2)
	next.MaxTokens = maxInt(256, p.MaxTokens/2)
	if p.GPULayers > 0 {
		next.GPULayers = maxInt(0, p.GPULayers/2)
	}
	return next
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
}

func InferImageParams(model registry.Model, profile hw.Profile) ImageParams {
	width, height := 512, 512
	steps := 20
	switch model.ImageFamily {
	case registry.ImageFamilySDXL, registry.ImageFamilyFlux, registry.ImageFamilySD3:
		width, height = 1024, 1024
	case registry.ImageFamilySD2:
		width, height = 768, 768
	}
	// CPU-only, low-RAM hardware: trade resolution/steps for a response
	// that finishes in a reasonable time instead of a multi-minute wait.
	if profile.GPUVendor == hw.GPUNone {
		if width > 512 {
			width, height = 512, 512
		}
		steps = 15
	}
	return ImageParams{Width: width, Height: height, Steps: steps}
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
