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
	defaultLLMTimeout = 15 * time.Second
	// defaultLLMMaxTokens caps the model's reply. It is generous because on
	// reasoning models (e.g. OpenAI's GPT-5 family) hidden reasoning tokens count
	// against this budget; too low a cap starves the visible JSON verdict.
	defaultLLMMaxTokens = 1024
	defaultLLMMaxChars  = 4000
	defaultLLMWindow    = 10

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

const imageUserTemplate = `Decide whether the attached IMAGE (and any caption below) violates the policy.
--- CAPTION ---
%s
--- END CAPTION ---`

// conversationIntro frames the windowed text classifier: it sees several recent
// messages at once so it can catch a banned term split across consecutive
// messages, and returns the ids of every offending message.
const conversationIntro = `You are a strict content-moderation classifier for a chat platform.

Apply the following moderation policy exactly. Do not invent additional rules,
and do not flag content the policy does not cover.
--- POLICY ---
%s
--- END POLICY ---

You are given the most recent messages in one channel, oldest first, each tagged
with its message id and author. Offenders often try to bypass the policy by
splitting a banned word or phrase across several consecutive messages (for
example sending "wa" then "ho" to spell a banned word). Read the messages
together, in order, treating each author's consecutive messages as a continuous
stream.

Respond with a single JSON object and nothing else, in exactly this form:
{"flagged": [{"id": "<message id>", "reason": "<short reason, at most 100 characters>"}]}
List every message that violates the policy, INCLUDING each message that
contributes a fragment of a split-up banned term. Use {"flagged": []} when no
message violates the policy.`

const conversationUserTemplate = `Messages to evaluate:
%s`

// Verdict is the model's judgement about a single image.
type Verdict struct {
	Flagged bool
	Reason  string
}

// ConversationMessage is one message handed to the windowed text filter.
type ConversationMessage struct {
	ID      string
	Author  string
	Content string
}

// FlaggedMessage identifies a message the model judged to violate the policy.
type FlaggedMessage struct {
	ID     string
	Reason string
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
	window    int // number of recent messages evaluated together

	// Image filtering.
	imagesEnabled  bool
	imageModel     string
	imageDirective string
	imageAction    Action
	imageMaxBytes  int64
	imageMaxCount  int

	timeout   time.Duration
	maxTokens int
	// temperature is nil unless explicitly configured. It is left unset by
	// default because some models (notably reasoning models) reject any value
	// other than their fixed default; sending nothing lets every model accept
	// the request.
	temperature *float64
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
	window := defaultLLMWindow
	if cfg.ContextMessages > 0 {
		window = cfg.ContextMessages
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
		window:      window,
		timeout:     timeout,
		maxTokens:   maxTokens,
		temperature: cfg.Temperature,
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

// Window is the number of recent messages evaluated together by EvaluateConversation.
func (f *LLMFilter) Window() int { return f.window }

// EvaluateConversation asks the model which of the given messages violate the
// text directive, considering them together so a banned term split across
// several messages is caught. msgs should be ordered oldest-first. It returns
// the offending messages (by id, with a reason); ids the model invents that are
// not in msgs are discarded. The supplied context bounds the call; an
// additional internal timeout guards against a hung request.
func (f *LLMFilter) EvaluateConversation(ctx context.Context, msgs []ConversationMessage) ([]FlaggedMessage, error) {
	if len(msgs) == 0 {
		return nil, nil
	}

	var sb strings.Builder
	known := make(map[string]struct{}, len(msgs))
	for _, m := range msgs {
		if strings.TrimSpace(m.ID) == "" {
			continue
		}
		content := m.Content
		if f.maxChars > 0 {
			if runes := []rune(content); len(runes) > f.maxChars {
				content = string(runes[:f.maxChars])
			}
		}
		// Keep each message on one line so ids stay unambiguous.
		content = strings.ReplaceAll(content, "\n", " ")
		author := strings.TrimSpace(m.Author)
		if author == "" {
			author = "unknown"
		}
		fmt.Fprintf(&sb, "[id=%s author=%s] %s\n", m.ID, author, content)
		known[m.ID] = struct{}{}
	}
	if len(known) == 0 {
		return nil, nil
	}

	reply, err := f.run(ctx, chatRequest{
		model:  f.textModel,
		system: fmt.Sprintf(conversationIntro, f.directive),
		text:   fmt.Sprintf(conversationUserTemplate, sb.String()),
	})
	if err != nil {
		return nil, err
	}
	return parseConversationVerdict(reply, known)
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

// parseConversationVerdict extracts the {"flagged":[{id,reason}]} object from a
// model response. It keeps only ids present in known (guarding against
// hallucinated ids) and deduplicates, so the caller can trust every returned id
// corresponds to a real message it supplied.
func parseConversationVerdict(s string, known map[string]struct{}) ([]FlaggedMessage, error) {
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("no JSON verdict in model response: %q", truncateForError(s))
	}
	var raw struct {
		Flagged []struct {
			ID     string `json:"id"`
			Reason string `json:"reason"`
		} `json:"flagged"`
	}
	if err := json.Unmarshal([]byte(s[start:end+1]), &raw); err != nil {
		return nil, fmt.Errorf("parse verdict json: %w", err)
	}

	var out []FlaggedMessage
	seen := make(map[string]struct{}, len(raw.Flagged))
	for _, fm := range raw.Flagged {
		id := strings.TrimSpace(fm.ID)
		if id == "" {
			continue
		}
		if _, ok := known[id]; !ok {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, FlaggedMessage{ID: id, Reason: strings.TrimSpace(fm.Reason)})
	}
	return out, nil
}

func truncateForError(s string) string {
	s = strings.TrimSpace(s)
	const max = 200
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
