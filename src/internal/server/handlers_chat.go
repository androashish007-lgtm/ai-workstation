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
//
// Relaxed from 10 to 15 minutes after testing: on this hardware the old
// 30-step ceiling (not the old 10-minute budget) was the actual binding
// constraint for models that render fast, while a slow-per-step model
// (large/high-res) was time-starved down to the 8-step floor well before
// the old budget ran out — raising both this and imageMaxSteps together
// let that case reach ~19 steps instead of 8, a real quality difference,
// at roughly 11 vs 5 minutes. 20 minutes tested fine but wasn't judged
// worth the extra wait over 15.
const imageTotalBudget = 15 * time.Minute

// imageFirstAttemptTarget: how much of the total budget the first (full
// quality) attempt gets to aim for, via imageperf's measured steps-per-
// second-at-this-resolution — leaving the rest as a safety margin for a
// step-down retry if the hardware turns out slower than the last measurement.
const imageFirstAttemptTarget = 11 * time.Minute

// imageMaxSteps caps sampling steps regardless of how much time budget is
// available — for SD/SDXL-class models, quality gains past the
// mid-40s/step range are marginal (diminishing returns), so this isn't
// meant to be raised indefinitely just because the budget above was.
const imageMaxSteps = 50

type chatRequestBody struct {
	SessionID          string   `json:"session_id"`
	Message            string   `json:"message"`
	AttachmentDataURIs []string `json:"attachment_data_uris,omitempty"`
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
	gen, genCtx, ok := a.generations.Start(sess.ID)
	if !ok {
		http.Error(w, "a response is already being generated for this chat", http.StatusConflict)
		return
	}

	safego.Go(func() {
		defer a.generations.Finish(sess.ID, gen)
		a.runChatTurn(genCtx, gen, sess, body)
	})

	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, map[string]any{"accepted": true})
}

// runChatTurn is the full pipeline for one exchange, running independently
// of any HTTP connection. Every step reports itself through gen.send so
// anyone watching (live or reconnected) sees the same thing.
func (a *App) runChatTurn(ctx context.Context, gen *Generation, sess *session.Session, body chatRequestBody) {
	hasAttachment := len(body.AttachmentDataURIs) > 0
	userMsg := session.Message{Role: session.RoleUser, Content: body.Message, Timestamp: time.Now()}
	if hasAttachment {
		for i, uri := range body.AttachmentDataURIs {
			if p, err := a.saveDataURIImage(sess.ID, uri, i); err == nil {
				userMsg.ImagePaths = append(userMsg.ImagePaths, p)
			}
		}
		hasAttachment = len(userMsg.ImagePaths) > 0
	}
	sess.Messages = append(sess.Messages, userMsg)
	gen.send(map[string]any{"type": "user_message", "content": body.Message, "image_paths": userMsg.ImagePaths})
	// Saved immediately so the user's own message is never lost to history
	// even if what follows is cancelled or interrupted.
	sess.AutoTitle()
	a.sessions.Save(sess)

	intent := a.classifyIntent(body.Message, hasAttachment)
	complexity := router.ClassifyComplexity(body.Message)

	var assistantText string
	var imageEvent map[string]any

	if intent == router.IntentText || intent == router.IntentMixed {
		text, handled := a.runTextTurn(ctx, gen, sess, hasAttachment, complexity)
		if !handled {
			a.finishInterrupted(ctx, gen, sess)
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
			a.finishInterrupted(ctx, gen, sess)
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

	// Replace the truncated first-message title with a real LLM-generated
	// one, once, right after the first exchange — in the background, since
	// it's a nice-to-have that must never delay the actual response the
	// user is waiting on. Text-only for now: for an image-only turn the
	// user's own prompt is usually already a decent title, and building an
	// image-aware prompt for this is more complexity than the truncated
	// fallback's shortcoming justifies.
	if len(sess.Messages) == 2 && assistantText != "" {
		userText := body.Message
		safego.Go(func() { a.generateTitle(sess.ID, userText, assistantText) })
	}

	cfg := a.config.Load()
	cfg.ActiveSessionID = sess.ID
	a.config.Save(cfg)

	gen.send(map[string]any{"type": "done"})
}

// finishInterrupted handles a turn that ended without a normal assistant
// response. If it's because the user clicked Stop (ctx.Err() != nil), that's
// recorded in history — so it's not just silently missing next time this
// chat is opened — and the stream gets a distinct "cancelled" event rather
// than "done" (which the UI treats as a full, successful completion).
// Anything else that ends a turn early (no_model, engine_approval_needed, a
// terminal generation error) already sent its own specific event before
// returning here, so this just closes the stream with "done" as before.
func (a *App) finishInterrupted(ctx context.Context, gen *Generation, sess *session.Session) {
	if ctx.Err() != nil {
		sess.Messages = append(sess.Messages, session.Message{
			Role: session.RoleAssistant, Content: "(stopped)", Timestamp: time.Now(),
		})
		a.sessions.Save(sess)
		gen.send(map[string]any{"type": "cancelled"})
		return
	}
	gen.send(map[string]any{"type": "done"})
}

// persistNotice saves a short assistant-visible explanation to history for a
// turn that ends without a real response (no installed model fits, an
// engine needs approval, generation failed everywhere). Without this, a
// turn that fails fast enough — no model installed is often near-instant,
// there's no inference to wait on — can finish before the live SSE stream
// even finishes connecting, so the event that explains why never reaches
// the browser and the chat is left with the user's message and no reply at
// all, forever, since nothing was ever saved. Re-opening this chat later
// always re-renders from saved history first, so this is what guarantees
// the explanation is seen even when the live event race is lost.
//
// notice, when non-empty, marks this as a stand-in for a specific live
// event (see the session.Message.Notice doc) so the frontend knows to
// fetch a *current* suggestion/approval card rather than trusting
// whatever this text says — the situation may have already changed by
// the time this message is actually viewed (e.g. the user already
// installed the suggested model since). Pass "" for a plain error with
// no follow-up action.
func (a *App) persistNotice(sess *session.Session, text, notice string) {
	sess.Messages = append(sess.Messages, session.Message{
		Role: session.RoleAssistant, Content: text, Notice: notice, Timestamp: time.Now(),
	})
	a.sessions.Save(sess)
}

// generateTitle asks the smallest installed text model for a short title
// summarizing the first exchange, replacing the truncated placeholder
// AutoTitle set. Runs detached from the request that triggered it — reloads
// the session fresh before writing so it doesn't clobber anything that
// happened in the meantime, and simply gives up on any failure (an
// unhelpful truncated title is a fine fallback; this is a nice-to-have,
// not something worth retrying or reporting to the user).
func (a *App) generateTitle(sessionID, userText, assistantText string) {
	models := a.reg.ByKind(registry.KindText)
	model, err := router.SelectTextModel(models, a.profile, router.ComplexitySimple, false, nil, "")
	if err != nil {
		return
	}
	binPath, status := a.ensureTextBinary(context.Background())
	if status != engine.StatusReady {
		return
	}

	prompt := "Reply with ONLY a short 3-6 word title for this chat — no quotes, no trailing punctuation, no preamble.\n\nUser: " +
		truncateForTitle(userText, 300)
	if assistantText != "" {
		prompt += "\nAssistant: " + truncateForTitle(assistantText, 300)
	}

	params := router.InferTextParams(*model, a.profile, len(prompt), router.ComplexitySimple)
	params.MaxTokens = 16 // a title needs a handful of tokens, not the usual reply budget

	tp, release, _, err := a.textPool.Acquire(context.Background(), binPath, *model, params, "")
	if err != nil {
		return // most likely a load failure (see runTextTurn) — not worth retrying for a title
	}
	defer release()

	stream, err := tp.ChatStream(context.Background(), []engine.ChatMessage{{Role: "user", Content: prompt}}, params.MaxTokens, 0.5)
	if err != nil {
		return
	}
	var out strings.Builder
	for ev := range stream {
		if ev.Err != nil {
			return
		}
		out.WriteString(ev.Delta)
		if ev.Done {
			break
		}
	}
	title := sanitizeTitle(out.String())
	if title == "" {
		return
	}

	fresh, err := a.sessions.Load(sessionID)
	if err != nil {
		return
	}
	fresh.Title = title
	a.sessions.Save(fresh)
}

// classifyIntent asks the smallest installed text model whether this
// request is text/image/mixed, falling back to router.ClassifyIntent's
// keyword heuristic on any failure — a model that's slow to load, an
// engine that isn't ready, a request that comes back ambiguous, or simply
// no model being installed yet must never block the actual turn just to
// decide how to route it. An attachment always skips straight to the
// heuristic: "an image was attached" already unambiguously means at least
// some vision analysis is needed, so there's nothing a classification
// call would add.
func (a *App) classifyIntent(text string, hasAttachment bool) router.Intent {
	fallback := router.ClassifyIntent(text, hasAttachment)
	if hasAttachment {
		return fallback
	}
	models := a.reg.ByKind(registry.KindText)
	model, err := router.SelectTextModel(models, a.profile, router.ComplexitySimple, false, nil, "")
	if err != nil {
		return fallback
	}
	// Only worth doing when this model is already warm: Acquire has no
	// timeout of its own for a cold start (see TextPool.HasResident), so a
	// classification call — meant to be a quick routing aid, not the
	// response itself — must never be the thing that triggers (and then
	// blocks on) loading a model from scratch. First message of a session
	// gets the heuristic; every one after that, once something's resident,
	// gets the real classification for free.
	if !a.textPool.HasResident(model.ID, "") {
		return fallback
	}
	binPath, status := a.ensureTextBinary(context.Background())
	if status != engine.StatusReady {
		return fallback
	}

	prompt := "Classify the request below as exactly one word — TEXT (a question, conversation, or writing request), " +
		"IMAGE (asking to create/draw/generate/paint a picture), or MIXED (asking to both discuss or describe something " +
		"AND create an image). Respond with ONLY that one word, nothing else.\n\nRequest: " + truncateForTitle(text, 500)

	params := router.InferTextParams(*model, a.profile, len(prompt), router.ComplexitySimple)
	params.MaxTokens = 4 // one classification word

	tp, release, _, err := a.textPool.Acquire(context.Background(), binPath, *model, params, "")
	if err != nil {
		return fallback
	}
	defer release()

	// Short, hard timeout: this is a routing aid, not the response itself
	// — a classification call that's still running after a few seconds is
	// not worth waiting on when the cheap heuristic is right there.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := tp.ChatStream(ctx, []engine.ChatMessage{{Role: "user", Content: prompt}}, params.MaxTokens, 0)
	if err != nil {
		return fallback
	}
	var out strings.Builder
	for ev := range stream {
		if ev.Err != nil {
			return fallback
		}
		out.WriteString(ev.Delta)
		if ev.Done {
			break
		}
	}
	switch strings.ToUpper(strings.TrimSpace(out.String())) {
	case "IMAGE":
		return router.IntentImage
	case "MIXED":
		return router.IntentMixed
	case "TEXT":
		return router.IntentText
	default:
		return fallback // didn't answer cleanly — don't trust a guess over the heuristic
	}
}

func truncateForTitle(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// sanitizeTitle strips the quoting/trailing-punctuation a small model
// commonly wraps a short answer in, and caps length defensively in case it
// ignores the "3-6 words" instruction.
func sanitizeTitle(s string) string {
	t := strings.TrimSpace(s)
	t = strings.Trim(t, "\"'“”‘’")
	t = strings.TrimRight(t, ".!? \t\n")
	if len(t) > 60 {
		t = strings.TrimSpace(t[:60])
	}
	return t
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
	fallbackNoticeSent := false

	for attempt := 0; attempt < 3; attempt++ {
		if ctx.Err() != nil {
			return "", false // stopped by the user — finishInterrupted handles reporting this
		}
		model, err := router.SelectTextModel(models, a.profile, complexity, needVision, tried, sess.LastTextID)
		if err != nil {
			if len(tried) > 0 {
				// A candidate WAS found and attempted (visible in tried) but
				// generation failed on it — that's a load/OOM failure, not
				// "nothing installed". Saying "not installed" here would send
				// the user to re-download something they already have.
				msg := "The installed model that fits this request failed to generate — likely not enough free memory to load it right now (other models or programs may be using it). Try again after closing something else, or resend in a moment."
				gen.send(map[string]any{"type": "error", "message": msg})
				a.persistNotice(sess, msg, "")
				return "", false
			}
			suggestion := catalog.BestFit(a.cat, registry.KindText, a.profile, a.reg.Snapshot(), needVision)
			gen.send(map[string]any{"type": "no_model", "kind": "text", "suggestion": suggestion})
			if needVision {
				a.persistNotice(sess, "No vision-capable chat model is installed, so I can't see the attached image. Approve the suggested download, then resend your message.", "no_vision_model")
			} else {
				a.persistNotice(sess, "No installed model fits this request yet. Approve the suggested download, then resend your message.", "no_text_model")
			}
			return "", false
		}

		binPath, status := a.ensureTextBinary(ctx)
		if status != engine.StatusReady {
			t, _ := a.engines.Snapshot()
			gen.send(map[string]any{"type": "engine_approval_needed", "component": "text", "status": t})
			a.persistNotice(sess, "Setting up the text engine for the first time — approve the download, then resend your message.", "text_engine_approval")
			return "", false
		}

		gen.send(map[string]any{"type": "model", "role": "text", "name": displayModelName(*model)})

		// A pick that fails to load/generate gets excluded (tried) and this
		// loop falls back to whatever else fits — sensible self-healing, but
		// silent about it otherwise looks exactly like "my selection was
		// ignored" (this is genuinely what a user reported after a selected
		// model kept timing out on slow storage — see healthTimeoutFor's doc
		// comment for that specific case). One notice per turn, only when
		// the model actually used differs from the explicit pick.
		if sess.LastTextID != "" && model.ID != sess.LastTextID && !fallbackNoticeSent {
			fallbackNoticeSent = true
			requestedName := sess.LastTextID
			for _, m := range models {
				if m.ID == sess.LastTextID {
					requestedName = displayModelName(m)
					break
				}
			}
			gen.send(map[string]any{"type": "model_fallback", "requested": requestedName, "used": displayModelName(*model)})
		}

		params := router.InferTextParams(*model, a.profile, len(sess.Messages[len(sess.Messages)-1].Content), complexity)
		text, err := a.generateText(ctx, gen, binPath, *model, params, sess, needVision)
		if err == nil {
			a.usageLog.Record(model.ID, displayModelName(*model), "text")
			return text, true
		}
		log.Printf("text generation failed on model %s: %v", model.Filename, err)
		tried[model.ID] = true
	}
	if ctx.Err() != nil {
		return "", false // stopped by the user mid-retry — finishInterrupted handles reporting this
	}
	const msg = "Text generation failed on every installed model that fits this hardware. Try a smaller model or a shorter request."
	gen.send(map[string]any{"type": "error", "message": msg})
	a.persistNotice(sess, msg, "")
	return "", false
}

func (a *App) generateText(ctx context.Context, gen *Generation, binPath string, model registry.Model, params router.TextParams, sess *session.Session, needVision bool) (string, error) {
	opID, end := a.activity.Begin(ActivityGeneratingText, displayModelName(model))
	defer end()

	mmproj := ""
	if model.VisionCapable() {
		mmproj = filepath.Join(a.dirs.ModelsText, model.PairedProjector)
	}

	// stepDown picks the retry lever that actually addresses what just
	// failed: a GPU allocation crash needs less GPU offload (the graduated
	// ladder), not a smaller context — trimming context/tokens wouldn't
	// have prevented a failure that happened while loading model weights
	// onto the GPU, before any request-specific memory was even involved.
	// Anything else (a generic CPU/RAM problem, an empty/failed stream)
	// gets the context/token trim instead, since GPU offload wasn't the
	// resource under pressure. gpuLadderDone latches once the GPU ladder
	// bottoms out at 0, so a later non-GPU failure doesn't re-walk it.
	gpuLadderDone := params.GPULayers == 0
	stepDown := func(errText string) {
		if !gpuLadderDone && params.GPULayers > 0 && router.IsGPUAllocationFailure(errText) {
			var done bool
			params, done = params.StepDownGPU()
			gpuLadderDone = done
			return
		}
		params = params.StepDown()
	}

	var lastErr error
	// Generous enough to walk the full GPU offload ladder down to CPU-only
	// (router.gpuLayerLadder has 5 rungs) plus a few generic context/token
	// retries after that — each failed attempt fails fast (the engine
	// crashes almost immediately on a bad allocation rather than hanging),
	// so extra headroom here costs very little in the failure case.
	const maxTextGenerationAttempts = 8
	for attempt := 0; attempt < maxTextGenerationAttempts; attempt++ {
		tp, release, started, err := a.textPool.Acquire(ctx, binPath, model, params, mmproj)
		if err != nil {
			lastErr = fmt.Errorf("starting/acquiring model instance: %w", err)
			stepDown(err.Error())
			continue
		}
		messages := a.buildChatMessages(sess, needVision)
		stream, err := tp.ChatStream(ctx, messages, params.MaxTokens, params.Temperature)
		if err != nil {
			release()
			if started {
				a.textPool.Discard(model, mmproj)
				lastErr = fmt.Errorf("starting chat stream: %w", err)
				stepDown(err.Error())
				continue
			}
			return "", fmt.Errorf("shared model instance rejected the request: %w", err)
		}
		var out strings.Builder
		var streamErr error
		for ev := range stream {
			if ev.Err != nil {
				streamErr = ev.Err
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
		if streamErr != nil || out.Len() == 0 {
			if started {
				a.textPool.Discard(model, mmproj)
				if streamErr != nil {
					lastErr = fmt.Errorf("streaming response: %w", streamErr)
					stepDown(streamErr.Error())
				} else {
					lastErr = fmt.Errorf("model produced an empty response")
					stepDown("")
				}
				continue
			}
			return "", fmt.Errorf("generation failed on a shared model instance")
		}
		return out.String(), nil
	}
	if lastErr != nil {
		return "", fmt.Errorf("generation failed after retrying with reduced settings: %w", lastErr)
	}
	return "", fmt.Errorf("generation failed after retrying with reduced settings")
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
		if i == len(sess.Messages)-1 && needVision && len(m.ImagePaths) > 0 {
			for _, p := range m.ImagePaths {
				if uri, err := a.imageFileToDataURI(p); err == nil {
					cm.Images = append(cm.Images, uri)
				}
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
	// detail is empty at Begin — which model this op is using isn't known
	// until SelectImageModel picks one below, and may change across a
	// fallback retry within this same op — so it's set with SetDetail
	// instead, once (and each time) a model is actually selected.
	opID, end := a.activity.Begin(ActivityGeneratingImage, "")
	defer end()
	enriched := a.enrichImagePrompt(ctx, rawPrompt)
	gen.send(map[string]any{"type": "image_prompt", "prompt": enriched})

	models := a.reg.ByKind(registry.KindImage)
	tried := map[string]bool{}
	fallbackNoticeSent := false

	// One shared deadline for the whole response, not per model tried —
	// otherwise a bad first pick could burn its own full 10 minutes before
	// a better-suited installed model ever gets a turn.
	deadline := time.Now().Add(imageTotalBudget)

	for attempt := 0; attempt < 3; attempt++ {
		if time.Until(deadline) < 30*time.Second || ctx.Err() != nil {
			break
		}
		model, err := router.SelectImageModel(models, a.profile, tried, sess.LastImageID)
		if err != nil {
			if len(tried) > 0 {
				// A candidate WAS found and attempted (see runTextTurn's
				// identical guard for why this needs to be distinguished
				// from nothing-installed at all).
				msg := "The installed model that fits this request failed to generate — likely not enough free memory to load it right now (other models or programs may be using it). Try again after closing something else, or resend in a moment."
				gen.send(map[string]any{"type": "error", "message": msg})
				a.persistNotice(sess, msg, "")
				return nil, false
			}
			suggestion := catalog.BestFit(a.cat, registry.KindImage, a.profile, a.reg.Snapshot(), false)
			gen.send(map[string]any{"type": "no_model", "kind": "image", "suggestion": suggestion})
			a.persistNotice(sess, "No installed image model fits this request yet. Approve the suggested download, then resend your message.", "no_image_model")
			return nil, false
		}
		binPath, status := a.ensureImageBinary(ctx)
		if status != engine.StatusReady {
			_, i := a.engines.Snapshot()
			gen.send(map[string]any{"type": "engine_approval_needed", "component": "image", "status": i})
			a.persistNotice(sess, "Setting up the image engine for the first time — approve the download, then resend your message.", "image_engine_approval")
			return nil, false
		}

		gen.send(map[string]any{"type": "model", "role": "image", "name": displayModelName(*model)})
		a.activity.SetDetail(opID, displayModelName(*model))

		if sess.LastImageID != "" && model.ID != sess.LastImageID && !fallbackNoticeSent {
			fallbackNoticeSent = true
			requestedName := sess.LastImageID
			for _, m := range models {
				if m.ID == sess.LastImageID {
					requestedName = displayModelName(m)
					break
				}
			}
			gen.send(map[string]any{"type": "model_fallback", "requested": requestedName, "used": displayModelName(*model)})
		}

		params := router.InferImageParams(*model, a.profile)
		png, err := a.generateImage(ctx, opID, binPath, *model, enriched, params, deadline)
		if err == nil {
			relPath, url, saveErr := a.saveGeneratedImage(sess.ID, png)
			if saveErr != nil {
				msg := "Image generated but could not be saved: " + saveErr.Error()
				gen.send(map[string]any{"type": "error", "message": msg})
				a.persistNotice(sess, msg, "")
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
	if ctx.Err() != nil {
		return nil, false // stopped by the user mid-retry — finishInterrupted handles reporting this
	}
	const msg = "Image generation didn't finish within the 10-minute budget on this hardware, across every installed model that fits."
	gen.send(map[string]any{"type": "error", "message": msg})
	a.persistNotice(sess, msg, "")
	return nil, false
}

// generateImage is time-budgeted, not step-count-budgeted: the step count
// for each attempt is computed from imageperf's measured seconds-per-step
// on THIS hardware at THIS resolution, aiming to use as many steps as fit
// (maximizing quality) rather than a fixed guess — and the request context
// is always capped so the whole call (across every retry) can never exceed
// imageTotalBudget, regardless of how slow the hardware turns out to be.
// fluxComponentPaths resolves a FLUX.2 checkpoint's paired VAE/text-encoder
// filenames (see registry.Model.PairedVAE/PairedTextEncoder) to full paths
// in models/image/, empty for every other family — the direct signal
// engine.ImageRequest uses to decide between the plain -m path and FLUX.2's
// three-file --diffusion-model/--vae/--llm invocation.
func (a *App) fluxComponentPaths(model registry.Model) (vae, llm string) {
	if model.ImageFamily != registry.ImageFamilyFlux2 {
		return "", ""
	}
	if model.PairedVAE != "" {
		vae = filepath.Join(a.dirs.ModelsImage, model.PairedVAE)
	}
	if model.PairedTextEncoder != "" {
		llm = filepath.Join(a.dirs.ModelsImage, model.PairedTextEncoder)
	}
	return vae, llm
}

func (a *App) generateImage(ctx context.Context, opID string, binPath string, model registry.Model, prompt string, params router.ImageParams, deadline time.Time) ([]byte, error) {
	threads := a.profile.CPUCores
	vaePath, llmPath := a.fluxComponentPaths(model)

	// Cold start: with no real measurement yet, StepsForBudget's built-in
	// guess (assumes a modest modern CPU) can be wildly wrong on weak or
	// virtualized hardware — wrong enough that a full-length first attempt
	// at the guessed step count would overshoot its own timeout and burn
	// most of the 10-minute budget before any retry gets a chance. A cheap,
	// short, capped probe gets a real number on the board first.
	if !a.imagePerf.HasData() {
		a.calibrateImagePerf(ctx, binPath, model, params.Width, params.Height, threads, time.Until(deadline))
	}

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		remaining := time.Until(deadline)
		if remaining < 30*time.Second {
			break // not enough budget left for a meaningful attempt
		}
		targetSeconds := imageFirstAttemptTarget.Seconds()
		if attempt > 0 || targetSeconds > remaining.Seconds()-5 {
			targetSeconds = remaining.Seconds() - 5
		}
		params.Steps = a.imagePerf.StepsForBudget(params.Width, params.Height, targetSeconds, 8, imageMaxSteps)

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
			BinPath: binPath, ModelPath: model.Path, VAEPath: vaePath, LLMPath: llmPath, Prompt: prompt,
			Width: params.Width, Height: params.Height, Steps: params.Steps, CFGScale: params.CFGScale,
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
		lastErr = err // kept so a total failure below explains why, not just "ran out of time/attempts"
		params = params.StepDown()
	}
	if lastErr != nil {
		return nil, fmt.Errorf("could not finish within the %s budget on this hardware: %w", imageTotalBudget, lastErr)
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

	vaePath, llmPath := a.fluxComponentPaths(model)
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	result, _ := engine.GenerateImage(probeCtx, engine.ImageRequest{
		BinPath: binPath, ModelPath: model.Path, VAEPath: vaePath, LLMPath: llmPath, Prompt: "calibration probe",
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
	model, err := router.SelectTextModel(models, a.profile, router.ComplexitySimple, false, nil, "")
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
	tp, release, _, err := a.textPool.Acquire(ctx, binPath, *model, params, mmproj)
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

func (a *App) saveDataURIImage(sessionID, dataURI string, idx int) (string, error) {
	commaIdx := strings.Index(dataURI, ",")
	if commaIdx == -1 {
		return "", fmt.Errorf("not a data URI")
	}
	raw, err := base64.StdEncoding.DecodeString(dataURI[commaIdx+1:])
	if err != nil {
		return "", err
	}
	dir := filepath.Join(a.dirs.Images, sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	filename := fmt.Sprintf("upload-%d-%d.png", time.Now().UnixNano(), idx)
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
