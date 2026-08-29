package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"aistation/internal/catalog"
	"aistation/internal/engine"
	"aistation/internal/imageperf"
	"aistation/internal/registry"
	"aistation/internal/router"
	"aistation/internal/safego"
	"aistation/internal/session"
)

const systemPrompt = "You are a helpful, concise AI assistant running fully offline on the user's own device. Answer directly."

const imagePromptEnrichSystem = "You expand short user requests into a single detailed text-to-image generation prompt. " +
	"Describe subject, style, composition, and lighting in one dense paragraph. Output ONLY the prompt text, nothing else — no preamble, no quotes."

// imageTotalBudget is the hard ceiling on wall-clock time for one image
// response (across however many step-down retries it takes) — the request
// context passed to the CLI is always capped so the last attempt can never
// run past this regardless of how slow the hardware turns out to be.
const imageTotalBudget = 10 * time.Minute

// imageFirstAttemptTarget: how much of the total budget the first (full
// quality) attempt gets to aim for, via imageperf's measured steps-per-
// second-at-this-resolution — leaving the rest as a safety margin for a
// step-down retry if the hardware turns out slower than the last measurement.
const imageFirstAttemptTarget = 7 * time.Minute

type chatRequestBody struct {
	SessionID         string `json:"session_id"`
	Message           string `json:"message"`
	AttachmentDataURI string `json:"attachment_data_uri,omitempty"`
}

// handleChat only validates and kicks the actual work off in the
// background, then returns immediately — the response is a Generation
// object other requests (session switches, other tabs, a reload) can
// (re)attach to via GET /api/sessions/{id}/stream. This is what makes
// switching chats, or closing the tab, never interrupt anything: nothing
// about the generation is tied to this HTTP request's lifetime.
func (a *App) handleChat(w http.ResponseWriter, r *http.Request) {
	var body chatRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	sess, err := a.sessions.Load(body.SessionID)
	if err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	gen, ok := a.generations.Start(sess.ID)
	if !ok {
		http.Error(w, "a response is already being generated for this chat", http.StatusConflict)
		return
	}

	safego.Go(func() {
		defer a.generations.Finish(sess.ID, gen)
		a.runChatTurn(context.Background(), gen, sess, body)
	})

	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, map[string]any{"accepted": true})
}

// runChatTurn is the full pipeline for one exchange, running independently
// of any HTTP connection. Every step reports itself through gen.send so
// anyone watching (live or reconnected) sees the same thing.
func (a *App) runChatTurn(ctx context.Context, gen *Generation, sess *session.Session, body chatRequestBody) {
	hasAttachment := body.AttachmentDataURI != ""
	userMsg := session.Message{Role: session.RoleUser, Content: body.Message, Timestamp: time.Now()}
	if hasAttachment {
		if p, err := a.saveDataURIImage(sess.ID, body.AttachmentDataURI); err == nil {
			userMsg.ImagePath = p
		}
	}
	sess.Messages = append(sess.Messages, userMsg)
	gen.send(map[string]any{"type": "user_message", "content": body.Message, "image_path": userMsg.ImagePath})

	intent := router.ClassifyIntent(body.Message, hasAttachment)
	complexity := router.ClassifyComplexity(body.Message)

	var assistantText string
	var imageEvent map[string]any

	if intent == router.IntentText || intent == router.IntentMixed {
		text, handled := a.runTextTurn(ctx, gen, sess, hasAttachment, complexity)
		if !handled {
			gen.send(map[string]any{"type": "done"})
			return
		}
		assistantText = text
	}

	if intent == router.IntentImage || intent == router.IntentMixed {
		prompt := body.Message
		if intent == router.IntentMixed {
			// Use whatever the assistant just said as extra context for what to draw.
			prompt = body.Message + "\n\n" + assistantText
		}
		imgEvt, handled := a.runImageTurn(ctx, gen, sess, prompt)
		if !handled {
			gen.send(map[string]any{"type": "done"})
			return
		}
		imageEvent = imgEvt
	}

	assistantMsg := session.Message{Role: session.RoleAssistant, Content: assistantText, Timestamp: time.Now()}
	if imageEvent != nil {
		if p, ok := imageEvent["rel_path"].(string); ok {
			assistantMsg.ImagePath = p
		}
		if assistantMsg.Content == "" {
			if p, ok := imageEvent["prompt"].(string); ok {
				assistantMsg.Content = p
			}
		}
	}
	sess.Messages = append(sess.Messages, assistantMsg)
	sess.AutoTitle()
	a.sessions.Save(sess)

	cfg := a.config.Load()
	cfg.ActiveSessionID = sess.ID
	a.config.Save(cfg)

	gen.send(map[string]any{"type": "done"})
}

func displayModelName(m registry.Model) string {
	if m.Name != "" {
		return m.Name
	}
	return m.Filename
}

// runTextTurn drives text generation with automatic model selection,
// OOM step-down retry, and fallback to the next-best installed model.
// Returns the full assistant text and whether the turn was handled (false
// means an event ending the stream — no_model, approval-needed, or a
// terminal error — has already been sent).
func (a *App) runTextTurn(ctx context.Context, gen *Generation, sess *session.Session, needVision bool, complexity router.Complexity) (string, bool) {
	models := a.reg.ByKind(registry.KindText)
	tried := map[string]bool{}

	for attempt := 0; attempt < 3; attempt++ {
		model, err := router.SelectTextModel(models, a.profile, complexity, needVision, tried)
		if err != nil {
			suggestion := catalog.BestFit(a.cat, registry.KindText, a.profile, a.reg.Snapshot())
			gen.send(map[string]any{"type": "no_model", "kind": "text", "suggestion": suggestion})
			return "", false
		}

		binPath, status := a.ensureTextBinary(ctx)
		if status != engine.StatusReady {
			t, _ := a.engines.Snapshot()
			gen.send(map[string]any{"type": "engine_approval_needed", "component": "text", "status": t})
			return "", false
		}

		gen.send(map[string]any{"type": "model", "role": "text", "name": displayModelName(*model)})

		params := router.InferTextParams(*model, a.profile, len(sess.Messages[len(sess.Messages)-1].Content), complexity)
		text, err := a.generateText(ctx, gen, binPath, *model, params, sess, needVision)
		if err == nil {
			a.usageLog.Record(model.ID, displayModelName(*model), "text")
			return text, true
		}
		log.Printf("text generation failed on model %s: %v", model.Filename, err)
		tried[model.ID] = true
	}
	gen.send(map[string]any{"type": "error", "message": "Text generation failed on every installed model that fits this hardware. Try a smaller model or a shorter request."})
	return "", false
}

func (a *App) generateText(ctx context.Context, gen *Generation, binPath string, model registry.Model, params router.TextParams, sess *session.Session, needVision bool) (string, error) {
	opID, end := a.activity.Begin(ActivityGeneratingText)
	defer end()

	mmproj := ""
	if model.VisionCapable() {
		mmproj = filepath.Join(a.dirs.ModelsText, model.PairedProjector)
	}

	for attempt := 0; attempt < 2; attempt++ {
		tp, release, started, err := a.textPool.Acquire(binPath, model, params, mmproj)
		if err != nil {
			params = params.StepDown()
			continue
		}
		messages := a.buildChatMessages(sess, needVision)
		stream, err := tp.ChatStream(ctx, messages, params.MaxTokens, params.Temperature)
		if err != nil {
			release()
			if started {
				a.textPool.Discard(model, mmproj)
				params = params.StepDown()
				continue
			}
			return "", fmt.Errorf("shared model instance rejected the request: %w", err)
		}
		var out strings.Builder
		streamFailed := false
		for ev := range stream {
			if ev.Err != nil {
				streamFailed = true
				break
			}
			if ev.Delta != "" {
				out.WriteString(ev.Delta)
				gen.send(map[string]any{"type": "text_delta", "text": ev.Delta})
				a.activity.Beat(opID)
			}
			if ev.Done {
				break
			}
		}
		release()
		if streamFailed || out.Len() == 0 {
			if started {
				a.textPool.Discard(model, mmproj)
				params = params.StepDown()
				continue
			}
			return "", fmt.Errorf("generation failed on a shared model instance")
		}
		return out.String(), nil
	}
	return "", fmt.Errorf("generation failed after retry with reduced settings")
}

func (a *App) buildChatMessages(sess *session.Session, needVision bool) []engine.ChatMessage {
	system := systemPrompt
	if sess.ProjectID != "" {
		if p, ok := a.projects.Get(sess.ProjectID); ok && strings.TrimSpace(p.Notes) != "" {
			// Shared project context — same for every chat in this project,
			// independent of that chat's own message history.
			system += "\n\nShared context for this project (\"" + p.Name + "\"):\n" + p.Notes
		}
	}
	msgs := []engine.ChatMessage{{Role: "system", Content: system}}
	// Cap history so we don't blow the inferred context window on long sessions.
	start := 0
	if len(sess.Messages) > 20 {
		start = len(sess.Messages) - 20
	}
	for i := start; i < len(sess.Messages); i++ {
		m := sess.Messages[i]
		cm := engine.ChatMessage{Role: string(m.Role), Content: m.Content}
		if i == len(sess.Messages)-1 && needVision && m.ImagePath != "" {
			if uri, err := a.imageFileToDataURI(m.ImagePath); err == nil {
				cm.Images = []string{uri}
			}
		}
		msgs = append(msgs, cm)
	}
	return msgs
}

func (a *App) ensureTextBinary(ctx context.Context) (string, engine.Status) {
	a.mu.Lock()
	if a.textBinPath != "" {
		p := a.textBinPath
		a.mu.Unlock()
		return p, engine.StatusReady
	}
	started := a.textBootstrapStarted
	a.textBootstrapStarted = true
	a.mu.Unlock()
	if !started {
		safego.Go(func() {
			bin, err := a.engines.EnsureText(context.Background())
			if err == nil {
				a.mu.Lock()
				a.textBinPath = bin
				a.mu.Unlock()
			}
		})
	}
	time.Sleep(400 * time.Millisecond)
	a.mu.Lock()
	p := a.textBinPath
	a.mu.Unlock()
	if p != "" {
		return p, engine.StatusReady
	}
	t, _ := a.engines.Snapshot()
	return "", t.Status
}

func (a *App) ensureImageBinary(ctx context.Context) (string, engine.Status) {
	a.mu.Lock()
	if a.imageBinPath != "" {
		p := a.imageBinPath
		a.mu.Unlock()
		return p, engine.StatusReady
	}
	started := a.imageBootstrapStarted
	a.imageBootstrapStarted = true
	a.mu.Unlock()
	if !started {
		safego.Go(func() {
			bin, err := a.engines.EnsureImage(context.Background())
			if err == nil {
				a.mu.Lock()
				a.imageBinPath = bin
				a.mu.Unlock()
			}
		})
	}
	time.Sleep(400 * time.Millisecond)
	a.mu.Lock()
	p := a.imageBinPath
	a.mu.Unlock()
	if p != "" {
		return p, engine.StatusReady
	}
	_, i := a.engines.Snapshot()
	return "", i.Status
}

// runImageTurn enriches the prompt via the resident text model, generates
// an image with automatic model selection and step-down/fallback retry, and
// persists the result. Returns event data (including "rel_path" for the
// session message) and whether the turn was handled.
func (a *App) runImageTurn(ctx context.Context, gen *Generation, sess *session.Session, rawPrompt string) (map[string]any, bool) {
	opID, end := a.activity.Begin(ActivityGeneratingImage)
	defer end()
	enriched := a.enrichImagePrompt(ctx, rawPrompt)
	gen.send(map[string]any{"type": "image_prompt", "prompt": enriched})

	models := a.reg.ByKind(registry.KindImage)
	tried := map[string]bool{}

	// One shared deadline for the whole response, not per model tried —
	// otherwise a bad first pick could burn its own full 10 minutes before
	// a better-suited installed model ever gets a turn.
	deadline := time.Now().Add(imageTotalBudget)

	for attempt := 0; attempt < 3; attempt++ {
		if time.Until(deadline) < 30*time.Second {
			break
		}
		model, err := router.SelectImageModel(models, a.profile, tried)
		if err != nil {
			suggestion := catalog.BestFit(a.cat, registry.KindImage, a.profile, a.reg.Snapshot())
			gen.send(map[string]any{"type": "no_model", "kind": "image", "suggestion": suggestion})
			return nil, false
		}
		binPath, status := a.ensureImageBinary(ctx)
		if status != engine.StatusReady {
			_, i := a.engines.Snapshot()
			gen.send(map[string]any{"type": "engine_approval_needed", "component": "image", "status": i})
			return nil, false
		}

		gen.send(map[string]any{"type": "model", "role": "image", "name": displayModelName(*model)})

		params := router.InferImageParams(*model, a.profile)
		png, err := a.generateImage(ctx, opID, binPath, *model, enriched, params, deadline)
		if err == nil {
			relPath, url, saveErr := a.saveGeneratedImage(sess.ID, png)
			if saveErr != nil {
				gen.send(map[string]any{"type": "error", "message": "Image generated but could not be saved: " + saveErr.Error()})
				return nil, false
			}
			a.usageLog.Record(model.ID, displayModelName(*model), "image")
			evt := map[string]any{"type": "image", "url": url, "prompt": enriched, "rel_path": relPath}
			gen.send(evt)
			return evt, true
		}
		log.Printf("image generation failed on model %s: %v", model.Filename, err)
		tried[model.ID] = true
	}
	gen.send(map[string]any{"type": "error", "message": "Image generation didn't finish within the 10-minute budget on this hardware, across every installed model that fits."})
	return nil, false
}

// generateImage is time-budgeted, not step-count-budgeted: the step count
// for each attempt is computed from imageperf's measured seconds-per-step
// on THIS hardware at THIS resolution, aiming to use as many steps as fit
// (maximizing quality) rather than a fixed guess — and the request context
// is always capped so the whole call (across every retry) can never exceed
// imageTotalBudget, regardless of how slow the hardware turns out to be.
func (a *App) generateImage(ctx context.Context, opID string, binPath string, model registry.Model, prompt string, params router.ImageParams, deadline time.Time) ([]byte, error) {
	threads := a.profile.CPUCores

	// Cold start: with no real measurement yet, StepsForBudget's built-in
	// guess (assumes a modest modern CPU) can be wildly wrong on weak or
	// virtualized hardware — wrong enough that a full-length first attempt
	// at the guessed step count would overshoot its own timeout and burn
	// most of the 10-minute budget before any retry gets a chance. A cheap,
	// short, capped probe gets a real number on the board first.
	if !a.imagePerf.HasData() {
		a.calibrateImagePerf(ctx, binPath, model, params.Width, params.Height, threads, time.Until(deadline))
	}

	for attempt := 0; attempt < 3; attempt++ {
		remaining := time.Until(deadline)
		if remaining < 30*time.Second {
			break // not enough budget left for a meaningful attempt
		}
		targetSeconds := imageFirstAttemptTarget.Seconds()
		if attempt > 0 || targetSeconds > remaining.Seconds()-5 {
			targetSeconds = remaining.Seconds() - 5
		}
		params.Steps = a.imagePerf.StepsForBudget(params.Width, params.Height, targetSeconds, 8, 30)

		// Cap this attempt's own timeout to its target budget (with slack
		// for the estimate being off), not the full remaining time — a bad
		// per-step estimate on an early attempt must not consume the whole
		// 10-minute ceiling and leave nothing for a step-down retry.
		attemptTimeout := time.Duration(targetSeconds*1.15+15) * time.Second
		if attemptTimeout > remaining {
			attemptTimeout = remaining
		}
		genCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
		result, err := engine.GenerateImage(genCtx, engine.ImageRequest{
			BinPath: binPath, ModelPath: model.Path, Prompt: prompt,
			Width: params.Width, Height: params.Height, Steps: params.Steps,
			Threads: threads, TmpDir: a.dirs.Downloads,
		})
		cancel()
		a.activity.Beat(opID)

		if secPerStep, ok := imageperf.ParseSecPerStep(result.RawOutput); ok {
			a.imagePerf.Record(params.Width, params.Height, secPerStep)
		}
		if err == nil {
			return result.PNG, nil
		}
		params = params.StepDown()
	}
	return nil, fmt.Errorf("could not finish within the %s budget on this hardware", imageTotalBudget)
}

// calibrateImagePerf runs a short, cheap probe (2 steps, at the actual
// resolution this request will use — compute cost per step doesn't scale
// perfectly linearly with pixel count, so measuring at a smaller resolution
// and extrapolating up under-predicted the real cost badly enough in
// testing to blow the budget anyway; measuring at the real size costs a bit
// more probe time but is trustworthy) bounded by its own tight timeout, so
// the very first real generation on this machine doesn't have to burn most
// of the 10-minute budget discovering the built-in guess was wrong. Even a
// probe that times out usually has partial step-timing output to parse
// (sd-cli logs each step as it completes); if it produces nothing at all
// (an extremely slow machine), a deliberately conservative synthetic
// estimate is recorded so the real attempt still picks a small, safe step
// count instead of repeating the same overshoot.
func (a *App) calibrateImagePerf(ctx context.Context, binPath string, model registry.Model, width, height, threads int, budgetLeft time.Duration) {
	const probeSteps = 2
	probeTimeout := 110 * time.Second
	if probeTimeout > budgetLeft {
		probeTimeout = budgetLeft
	}
	if probeTimeout < 15*time.Second {
		return // not enough budget left to even try a probe
	}

	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	result, _ := engine.GenerateImage(probeCtx, engine.ImageRequest{
		BinPath: binPath, ModelPath: model.Path, Prompt: "calibration probe",
		Width: width, Height: height, Steps: probeSteps,
		Threads: threads, TmpDir: a.dirs.Downloads,
	})
	if secPerStep, ok := imageperf.ParseSecPerStep(result.RawOutput); ok {
		a.imagePerf.Record(width, height, secPerStep)
		return
	}
	a.imagePerf.Record(512, 512, 60.0) // conservative fallback: assume this machine is slow
}

// enrichImagePrompt asks the resident text model to turn a short request
// into a full T2I prompt. Falls back to the raw request verbatim if no text
// model/engine is available yet, so image generation still works standalone.
func (a *App) enrichImagePrompt(ctx context.Context, rawPrompt string) string {
	models := a.reg.ByKind(registry.KindText)
	model, err := router.SelectTextModel(models, a.profile, router.ComplexitySimple, false, nil)
	if err != nil {
		return rawPrompt
	}
	binPath, status := a.ensureTextBinary(ctx)
	if status != engine.StatusReady {
		return rawPrompt
	}
	params := router.InferTextParams(*model, a.profile, len(rawPrompt), router.ComplexitySimple)
	mmproj := ""
	if model.VisionCapable() {
		mmproj = filepath.Join(a.dirs.ModelsText, model.PairedProjector)
	}
	tp, release, _, err := a.textPool.Acquire(binPath, *model, params, mmproj)
	if err != nil {
		return rawPrompt
	}
	defer release()
	messages := []engine.ChatMessage{
		{Role: "system", Content: imagePromptEnrichSystem},
		{Role: "user", Content: rawPrompt},
	}
	stream, err := tp.ChatStream(ctx, messages, 220, 0.9)
	if err != nil {
		return rawPrompt
	}
	var out strings.Builder
	for ev := range stream {
		if ev.Err != nil {
			break
		}
		out.WriteString(ev.Delta)
		if ev.Done {
			break
		}
	}
	result := strings.TrimSpace(out.String())
	if result == "" {
		return rawPrompt
	}
	return result
}

func (a *App) saveGeneratedImage(sessionID string, png []byte) (relPath, url string, err error) {
	dir := filepath.Join(a.dirs.Images, sessionID)
	if err = os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	filename := fmt.Sprintf("gen-%d.png", time.Now().UnixNano())
	if err = os.WriteFile(filepath.Join(dir, filename), png, 0o644); err != nil {
		return
	}
	relPath = sessionID + "/" + filename
	url = "/images/" + sessionID + "/" + filename
	return
}

func (a *App) saveDataURIImage(sessionID, dataURI string) (string, error) {
	idx := strings.Index(dataURI, ",")
	if idx == -1 {
		return "", fmt.Errorf("not a data URI")
	}
	raw, err := base64.StdEncoding.DecodeString(dataURI[idx+1:])
	if err != nil {
		return "", err
	}
	dir := filepath.Join(a.dirs.Images, sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	filename := fmt.Sprintf("upload-%d.png", time.Now().UnixNano())
	if err := os.WriteFile(filepath.Join(dir, filename), raw, 0o644); err != nil {
		return "", err
	}
	return sessionID + "/" + filename, nil
}

func (a *App) imageFileToDataURI(relPath string) (string, error) {
	full := filepath.Join(a.dirs.Images, filepath.FromSlash(relPath))
	raw, err := os.ReadFile(full)
	if err != nil {
		return "", err
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(raw), nil
}
