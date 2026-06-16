package censor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// Default LLMFilter tunables, applied when the corresponding config field is
// left unset.
const (
	defaultLLMTimeout   = 15 * time.Second
	defaultLLMMaxTokens = 200
	defaultLLMMaxChars  = 4000
	defaultLLMTemp      = 0.0

	defaultImageMaxBytes int64 = 5 << 20 // 5 MiB
	defaultImageMaxCount       = 4
)

// classifierIntro frames the model as a strict moderation classifier and pins
// the output format so the verdict parses reliably across providers.
const classifierIntro = `You are a strict content-moderation classifier for a chat platform.

Apply the following moderation policy exactly. Do not invent additional rules,
and do not flag content the policy does not cover.
--- POLICY ---
%s
--- END POLICY ---

Respond with a single line of compact JSON and nothing else, in exactly this form:
{"flagged": true, "reason": "<short reason, at most 100 characters>"}
Use {"flagged": false, "reason": ""} when nothing violates the policy.`

const textUserTemplate = `Decide whether this MESSAGE violates the policy:
--- MESSAGE ---
%s
--- END MESSAGE ---`

const imageUserTemplate = `Decide whether the attached IMAGE (and any caption below) violates the policy.
--- CAPTION ---
%s
--- END CAPTION ---`

// Verdict is the model's judgement about a single message or image.
type Verdict struct {
	Flagged bool
	Reason  string
}

// LLMFilter evaluates messages and image attachments against natural-language
// directives using a provider's HTTP API. It is safe for concurrent use; each
// call is a stateless request.
type LLMFilter struct {
	provider chatProvider

	// Text filtering.
	textModel string
	directive string
	action    Action
	notice    string
	maxChars  int

	// Image filtering.
	imagesEnabled  bool
	imageModel     string
	imageDirective string
	imageAction    Action
	imageMaxBytes  int64
	imageMaxCount  int

	timeout     time.Duration
	maxTokens   int
	temperature float64
}

// NewLLMFilter validates cfg and constructs a ready-to-use filter. It reads the
// API key from the environment variable named by cfg.APIKeyEnv so secrets stay
// out of the config file.
func NewLLMFilter(cfg *LLMConfig) (*LLMFilter, error) {
	if cfg == nil {
		return nil, fmt.Errorf("nil llm config")
	}
	if strings.TrimSpace(cfg.Provider) == "" {
		return nil, fmt.Errorf("llm: provider is required")
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, fmt.Errorf("llm: model is required")
	}
	if strings.TrimSpace(cfg.Directive) == "" {
		return nil, fmt.Errorf("llm: directive is required")
	}

	action, err := parseLLMAction(cfg.Action)
	if err != nil {
		return nil, fmt.Errorf("llm: %w", err)
	}

	timeout := defaultLLMTimeout
	if cfg.TimeoutSeconds > 0 {
		timeout = time.Duration(cfg.TimeoutSeconds) * time.Second
	}
	maxTokens := defaultLLMMaxTokens
	if cfg.MaxTokens > 0 {
		maxTokens = cfg.MaxTokens
	}
	maxChars := defaultLLMMaxChars
	if cfg.MaxMessageChars > 0 {
		maxChars = cfg.MaxMessageChars
	}
	temperature := defaultLLMTemp
	if cfg.Temperature != nil {
		temperature = *cfg.Temperature
	}

	var apiKey string
	if env := strings.TrimSpace(cfg.APIKeyEnv); env != "" {
		apiKey = strings.TrimSpace(os.Getenv(env))
		if apiKey == "" {
			return nil, fmt.Errorf("llm: env var %s (api_key_env) is empty", env)
		}
	}

	provider, err := newProvider(cfg.Provider, apiKey, strings.TrimSpace(cfg.Endpoint))
	if err != nil {
		return nil, fmt.Errorf("llm: %w", err)
	}

	f := &LLMFilter{
		provider:    provider,
		textModel:   cfg.Model,
		directive:   strings.TrimSpace(cfg.Directive),
		action:      action,
		notice:      cfg.Notice,
		maxChars:    maxChars,
		timeout:     timeout,
		maxTokens:   maxTokens,
		temperature: temperature,
	}

	if ic := cfg.Images; ic != nil && ic.Enabled {
		imageAction := action
		if strings.TrimSpace(ic.Action) != "" {
			imageAction, err = parseLLMAction(ic.Action)
			if err != nil {
				return nil, fmt.Errorf("llm images: %w", err)
			}
		}
		imageDirective := f.directive
		if strings.TrimSpace(ic.Directive) != "" {
			imageDirective = strings.TrimSpace(ic.Directive)
		}
		imageModel := cfg.Model
		if strings.TrimSpace(ic.Model) != "" {
			imageModel = strings.TrimSpace(ic.Model)
		}
		f.imagesEnabled = true
		f.imageModel = imageModel
		f.imageDirective = imageDirective
		f.imageAction = imageAction
		f.imageMaxBytes = defaultImageMaxBytes
		if ic.MaxBytes > 0 {
			f.imageMaxBytes = ic.MaxBytes
		}
		f.imageMaxCount = defaultImageMaxCount
		if ic.MaxCount > 0 {
			f.imageMaxCount = ic.MaxCount
		}
	}

	return f, nil
}

// parseLLMAction parses an action and rejects "replace", which the filter
// cannot perform (the model reports no character spans).
func parseLLMAction(s string) (Action, error) {
	action, err := parseAction(s)
	if err != nil {
		return 0, err
	}
	if action == ActionReplace {
		return 0, fmt.Errorf("action %q is unsupported (want log|delete|warn)", s)
	}
	return action, nil
}

// Action is the action to take when the model flags a message's text.
func (f *LLMFilter) Action() Action { return f.action }

// Notice is the per-filter warn notice, or "" to fall back to the global notice.
func (f *LLMFilter) Notice() string { return f.notice }

// Provider returns the configured provider name.
func (f *LLMFilter) Provider() string { return f.provider.name() }

// Model returns the configured text model identifier.
func (f *LLMFilter) Model() string { return f.textModel }

// ImagesEnabled reports whether image attachments should be filtered.
func (f *LLMFilter) ImagesEnabled() bool { return f.imagesEnabled }

// ImageAction is the action to take when the model flags an image.
func (f *LLMFilter) ImageAction() Action { return f.imageAction }

// ImageModel returns the vision model identifier.
func (f *LLMFilter) ImageModel() string { return f.imageModel }

// ImageMaxBytes is the per-image size limit; larger attachments are skipped.
func (f *LLMFilter) ImageMaxBytes() int64 { return f.imageMaxBytes }

// ImageMaxCount is the maximum number of image attachments checked per message.
func (f *LLMFilter) ImageMaxCount() int { return f.imageMaxCount }

// Evaluate asks the model whether content violates the text directive. The
// supplied context bounds the call; Evaluate additionally applies its own
// timeout so a background context still cannot hang.
func (f *LLMFilter) Evaluate(ctx context.Context, content string) (Verdict, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return Verdict{}, nil
	}
	if f.maxChars > 0 {
		if runes := []rune(content); len(runes) > f.maxChars {
			content = string(runes[:f.maxChars])
		}
	}

	reply, err := f.run(ctx, chatRequest{
		model:  f.textModel,
		system: fmt.Sprintf(classifierIntro, f.directive),
		text:   fmt.Sprintf(textUserTemplate, content),
	})
	if err != nil {
		return Verdict{}, err
	}
	return parseVerdict(reply)
}

// EvaluateImage asks the model whether an image attachment (with an optional
// caption) violates the image directive. data is the raw image bytes and
// mimeType its content type (e.g. "image/png").
func (f *LLMFilter) EvaluateImage(ctx context.Context, mimeType string, data []byte, caption string) (Verdict, error) {
	if len(data) == 0 {
		return Verdict{}, nil
	}
	caption = strings.TrimSpace(caption)
	if caption == "" {
		caption = "(no caption)"
	} else if f.maxChars > 0 {
		if runes := []rune(caption); len(runes) > f.maxChars {
			caption = string(runes[:f.maxChars])
		}
	}

	reply, err := f.run(ctx, chatRequest{
		model:  f.imageModel,
		system: fmt.Sprintf(classifierIntro, f.imageDirective),
		text:   fmt.Sprintf(imageUserTemplate, caption),
		images: []chatImage{{mimeType: mimeType, data: data}},
	})
	if err != nil {
		return Verdict{}, err
	}
	return parseVerdict(reply)
}

// run fills in the shared request fields, applies the per-call timeout, and
// dispatches to the provider.
func (f *LLMFilter) run(ctx context.Context, req chatRequest) (string, error) {
	req.maxTokens = f.maxTokens
	req.temperature = f.temperature

	ctx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()

	reply, err := f.provider.complete(ctx, req)
	if err != nil {
		return "", fmt.Errorf("llm %s: %w", f.provider.name(), err)
	}
	return reply, nil
}

// parseVerdict extracts the JSON verdict object from a model response,
// tolerating surrounding prose or code fences by scanning for the outermost
// brace pair.
func parseVerdict(s string) (Verdict, error) {
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return Verdict{}, fmt.Errorf("no JSON verdict in model response: %q", truncateForError(s))
	}
	var raw struct {
		Flagged bool   `json:"flagged"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(s[start:end+1]), &raw); err != nil {
		return Verdict{}, fmt.Errorf("parse verdict json: %w", err)
	}
	return Verdict{Flagged: raw.Flagged, Reason: strings.TrimSpace(raw.Reason)}, nil
}

func truncateForError(s string) string {
	s = strings.TrimSpace(s)
	const max = 200
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
