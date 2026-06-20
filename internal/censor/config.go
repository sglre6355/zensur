package censor

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config is the censor module's static configuration, loaded from YAML.
//
// Defaults are applied to every rule unless the rule overrides them. The
// Notice, NoticeAutoDeleteSeconds, and Replacement fields supply fallbacks
// when an individual rule does not set its own.
type Config struct {
	Defaults                NormalizeOpts `yaml:"defaults"`
	Notice                  string        `yaml:"notice"`
	NoticeAutoDeleteSeconds int           `yaml:"notice_auto_delete_seconds"`
	Replacement             string        `yaml:"replacement"`
	Rules                   []RawRule     `yaml:"rules"`

	// LLM configures an optional semantic filter that asks a large language
	// model whether each message violates a natural-language directive. It runs
	// alongside the pattern rules; a message is acted on if either the rules or
	// the LLM flag it. Omit the section (or set enabled: false) to disable it.
	LLM *LLMConfig `yaml:"llm,omitempty"`
}

// LLMConfig configures the semantic message filter. Each provider is its
// official Go SDK wrapped behind the censor package's own small adapter, so
// switching vendors is just a provider/model change.
type LLMConfig struct {
	// Enabled turns the filter on. When false the section is parsed but ignored.
	Enabled bool `yaml:"enabled"`
	// Provider selects the API adapter: openai, anthropic, or google. "openai"
	// also drives any OpenAI-compatible endpoint (xAI, Groq, OpenRouter, vLLM,
	// ollama's /v1, …) when Endpoint is set.
	Provider string `yaml:"provider"`
	// Model is the provider-specific model identifier, e.g. "gpt-4o-mini".
	Model string `yaml:"model"`
	// APIKeyEnv names the environment variable holding the provider API key.
	// The key is read from the environment so it never lives in the config file.
	// May be empty for keyless local endpoints.
	APIKeyEnv string `yaml:"api_key_env"`
	// Endpoint optionally overrides the SDK's default base URL (useful for
	// self-hosted gateways, Azure, OpenAI-compatible servers, or a custom Gemini
	// base URL). It is a base URL, not a full request path.
	Endpoint string `yaml:"endpoint,omitempty"`
	// Directive is the natural-language moderation policy the model applies to
	// message text.
	Directive string `yaml:"directive"`
	// ContextMessages is how many recent messages (including the triggering one)
	// are sent to the model together, so it can catch a banned term split across
	// consecutive messages. Default 10; set to 1 for per-message evaluation.
	ContextMessages int `yaml:"context_messages,omitempty"`
	// Action is taken when the model flags a message: log, delete, or warn.
	// (replace is unavailable because the model reports no character spans.)
	Action string `yaml:"action"`
	// Notice overrides the global notice for the warn action on LLM hits.
	Notice string `yaml:"notice,omitempty"`
	// TimeoutSeconds bounds each model call (default 15).
	TimeoutSeconds int `yaml:"timeout_seconds,omitempty"`
	// MaxTokens caps the model's response length (default 200).
	MaxTokens int `yaml:"max_tokens,omitempty"`
	// Temperature controls sampling randomness (default 0 for determinism).
	Temperature *float64 `yaml:"temperature,omitempty"`
	// MaxMessageChars truncates long messages before sending them to the model
	// to bound token usage (default 4000).
	MaxMessageChars int `yaml:"max_message_chars,omitempty"`

	// Images optionally extends the filter to image attachments using the same
	// provider's vision capability. Omit (or set enabled: false) to skip images.
	Images *LLMImageConfig `yaml:"images,omitempty"`
}

// LLMImageConfig configures vision-based filtering of image attachments. It
// reuses the parent LLMConfig's provider, endpoint, and API key; only the
// fields that differ for images live here.
type LLMImageConfig struct {
	// Enabled turns image filtering on.
	Enabled bool `yaml:"enabled"`
	// Model optionally overrides the text model with a vision-capable one. When
	// empty the parent LLMConfig.Model is used (it must then support images).
	Model string `yaml:"model,omitempty"`
	// Action is taken when the model flags an image: log, delete, or warn.
	// When empty it inherits the parent LLMConfig.Action.
	Action string `yaml:"action,omitempty"`
	// Directive is the natural-language policy applied to images. When empty it
	// inherits the parent LLMConfig.Directive.
	Directive string `yaml:"directive,omitempty"`
	// MaxBytes skips image attachments larger than this many bytes (default
	// 5 MiB) to bound download cost and token usage.
	MaxBytes int64 `yaml:"max_bytes,omitempty"`
	// MaxCount caps how many image attachments are checked per message
	// (default 4).
	MaxCount int `yaml:"max_count,omitempty"`
}

// LoadConfigFile reads a YAML config at path.
func LoadConfigFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read censor config %s: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse censor config %s: %w", path, err)
	}
	return &c, nil
}

// NormalizeOpts controls text normalization. Pointer fields distinguish
// "unset" (inherit from global defaults) from explicitly false. The zero value
// represents "inherit everything".
type NormalizeOpts struct {
	CaseInsensitive  *bool `yaml:"case_insensitive,omitempty"`
	NormalizeUnicode *bool `yaml:"normalize_unicode,omitempty"`
	StripMarks       *bool `yaml:"strip_marks,omitempty"`
	Leet             *bool `yaml:"leet,omitempty"`
	CollapseRepeats  *bool `yaml:"collapse_repeats,omitempty"`
	FoldKana         *bool `yaml:"fold_kana,omitempty"`
}

// ResolvedOpts is NormalizeOpts after merging with defaults — every field is
// concrete.
type ResolvedOpts struct {
	CaseInsensitive  bool
	NormalizeUnicode bool
	StripMarks       bool
	Leet             bool
	CollapseRepeats  bool
	FoldKana         bool
}

// conservativeDefaults are applied when neither the rule nor the global
// Defaults section sets a given option.
var conservativeDefaults = ResolvedOpts{
	CaseInsensitive:  true,
	NormalizeUnicode: true,
	StripMarks:       true,
	Leet:             false,
	CollapseRepeats:  false,
	FoldKana:         false,
}

func resolveOpts(global, rule NormalizeOpts) ResolvedOpts {
	pick := func(r, g *bool, fallback bool) bool {
		if r != nil {
			return *r
		}
		if g != nil {
			return *g
		}
		return fallback
	}
	return ResolvedOpts{
		CaseInsensitive:  pick(rule.CaseInsensitive, global.CaseInsensitive, conservativeDefaults.CaseInsensitive),
		NormalizeUnicode: pick(rule.NormalizeUnicode, global.NormalizeUnicode, conservativeDefaults.NormalizeUnicode),
		StripMarks:       pick(rule.StripMarks, global.StripMarks, conservativeDefaults.StripMarks),
		Leet:             pick(rule.Leet, global.Leet, conservativeDefaults.Leet),
		CollapseRepeats:  pick(rule.CollapseRepeats, global.CollapseRepeats, conservativeDefaults.CollapseRepeats),
		FoldKana:         pick(rule.FoldKana, global.FoldKana, conservativeDefaults.FoldKana),
	}
}

// RawRule is the YAML-deserialized form of a single rule.
type RawRule struct {
	ID            string   `yaml:"id"`
	Patterns      []string `yaml:"patterns"`
	Mode          string   `yaml:"mode"`
	Action        string   `yaml:"action"`
	Notice        string   `yaml:"notice,omitempty"`
	Replacement   string   `yaml:"replacement,omitempty"`
	Allow         []string `yaml:"allow,omitempty"`
	NormalizeOpts `yaml:",inline"`
}
