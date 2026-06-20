package censor

import (
	"sort"
	"testing"
)

func TestParseConversationVerdict(t *testing.T) {
	known := map[string]struct{}{"1": {}, "2": {}, "3": {}}

	t.Run("keeps known ids", func(t *testing.T) {
		out, err := parseConversationVerdict(
			`{"flagged":[{"id":"1","reason":"frag wa"},{"id":"2","reason":"frag ho"}]}`, known)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got := ids(out)
		if len(got) != 2 || got[0] != "1" || got[1] != "2" {
			t.Fatalf("ids = %v, want [1 2]", got)
		}
		if out[0].Reason != "frag wa" {
			t.Errorf("reason = %q", out[0].Reason)
		}
	})

	t.Run("drops hallucinated ids", func(t *testing.T) {
		out, err := parseConversationVerdict(
			`{"flagged":[{"id":"2","reason":"x"},{"id":"999","reason":"made up"}]}`, known)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := ids(out); len(got) != 1 || got[0] != "2" {
			t.Fatalf("ids = %v, want [2]", got)
		}
	})

	t.Run("deduplicates", func(t *testing.T) {
		out, err := parseConversationVerdict(
			`{"flagged":[{"id":"3"},{"id":"3"}]}`, known)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := ids(out); len(got) != 1 || got[0] != "3" {
			t.Fatalf("ids = %v, want [3]", got)
		}
	})

	t.Run("empty flagged", func(t *testing.T) {
		out, err := parseConversationVerdict(`{"flagged":[]}`, known)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(out) != 0 {
			t.Fatalf("want none, got %v", ids(out))
		}
	})

	t.Run("tolerates code fence", func(t *testing.T) {
		out, err := parseConversationVerdict(
			"```json\n{\"flagged\":[{\"id\":\"1\",\"reason\":\"r\"}]}\n```", known)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := ids(out); len(got) != 1 || got[0] != "1" {
			t.Fatalf("ids = %v, want [1]", got)
		}
	})

	t.Run("no json errors", func(t *testing.T) {
		if _, err := parseConversationVerdict("I can't help.", known); err == nil {
			t.Fatal("expected error")
		}
	})
}

func ids(fms []FlaggedMessage) []string {
	out := make([]string, len(fms))
	for i, fm := range fms {
		out[i] = fm.ID
	}
	sort.Strings(out)
	return out
}

func TestParseVerdict(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		wantFlagged bool
		wantReason  string
		wantErr     bool
	}{
		{
			name:        "clean compact json",
			in:          `{"flagged": true, "reason": "harassment"}`,
			wantFlagged: true,
			wantReason:  "harassment",
		},
		{
			name:        "not flagged",
			in:          `{"flagged": false, "reason": ""}`,
			wantFlagged: false,
		},
		{
			name:        "wrapped in code fence and prose",
			in:          "Sure, here is the verdict:\n```json\n{\"flagged\": true, \"reason\": \"spam link\"}\n```",
			wantFlagged: true,
			wantReason:  "spam link",
		},
		{
			name:    "no json object",
			in:      "I cannot help with that.",
			wantErr: true,
		},
		{
			name:    "malformed json",
			in:      `{"flagged": yes}`,
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, err := parseVerdict(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got verdict %+v", v)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if v.Flagged != tc.wantFlagged {
				t.Errorf("flagged = %v, want %v", v.Flagged, tc.wantFlagged)
			}
			if v.Reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", v.Reason, tc.wantReason)
			}
		})
	}
}

func TestNewLLMFilterValidation(t *testing.T) {
	// Provider "openai" with no api_key_env builds a keyless client, so these
	// validation tests touch no network and need no secret.
	base := func() *LLMConfig {
		return &LLMConfig{
			Provider:  "openai",
			Model:     "gpt-4o-mini",
			Directive: "Flag spam.",
			Action:    "warn",
		}
	}

	t.Run("valid config", func(t *testing.T) {
		f, err := NewLLMFilter(base())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if f.Action() != ActionWarn {
			t.Errorf("action = %v, want warn", f.Action())
		}
		if f.Provider() != "openai" || f.Model() != "gpt-4o-mini" {
			t.Errorf("provider/model = %q/%q", f.Provider(), f.Model())
		}
		if f.ImagesEnabled() {
			t.Error("images should be disabled by default")
		}
	})

	t.Run("missing directive", func(t *testing.T) {
		cfg := base()
		cfg.Directive = "   "
		if _, err := NewLLMFilter(cfg); err == nil {
			t.Fatal("expected error for missing directive")
		}
	})

	t.Run("missing provider", func(t *testing.T) {
		cfg := base()
		cfg.Provider = ""
		if _, err := NewLLMFilter(cfg); err == nil {
			t.Fatal("expected error for missing provider")
		}
	})

	t.Run("unknown provider", func(t *testing.T) {
		cfg := base()
		cfg.Provider = "nope"
		if _, err := NewLLMFilter(cfg); err == nil {
			t.Fatal("expected error for unknown provider")
		}
	})

	t.Run("replace action rejected", func(t *testing.T) {
		cfg := base()
		cfg.Action = "replace"
		if _, err := NewLLMFilter(cfg); err == nil {
			t.Fatal("expected error for replace action")
		}
	})

	t.Run("missing api key env", func(t *testing.T) {
		cfg := base()
		cfg.APIKeyEnv = "ZENSUR_TEST_DEFINITELY_UNSET_KEY"
		if _, err := NewLLMFilter(cfg); err == nil {
			t.Fatal("expected error for empty api key env")
		}
	})
}

func TestNewLLMFilterImages(t *testing.T) {
	base := func() *LLMConfig {
		return &LLMConfig{
			Provider:  "openai",
			Model:     "gpt-4o-mini",
			Directive: "Flag spam.",
			Action:    "delete",
		}
	}

	t.Run("inherits parent defaults", func(t *testing.T) {
		cfg := base()
		cfg.Images = &LLMImageConfig{Enabled: true}
		f, err := NewLLMFilter(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !f.ImagesEnabled() {
			t.Fatal("images should be enabled")
		}
		if f.ImageAction() != ActionDelete {
			t.Errorf("image action = %v, want inherited delete", f.ImageAction())
		}
		if f.ImageModel() != "gpt-4o-mini" {
			t.Errorf("image model = %q, want inherited gpt-4o-mini", f.ImageModel())
		}
		if f.ImageMaxBytes() != defaultImageMaxBytes {
			t.Errorf("image max bytes = %d, want default %d", f.ImageMaxBytes(), defaultImageMaxBytes)
		}
		if f.ImageMaxCount() != defaultImageMaxCount {
			t.Errorf("image max count = %d, want default %d", f.ImageMaxCount(), defaultImageMaxCount)
		}
	})

	t.Run("overrides applied", func(t *testing.T) {
		cfg := base()
		cfg.Images = &LLMImageConfig{
			Enabled:  true,
			Model:    "gpt-4o",
			Action:   "warn",
			MaxBytes: 1024,
			MaxCount: 2,
		}
		f, err := NewLLMFilter(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if f.ImageAction() != ActionWarn {
			t.Errorf("image action = %v, want warn", f.ImageAction())
		}
		if f.ImageModel() != "gpt-4o" {
			t.Errorf("image model = %q, want gpt-4o", f.ImageModel())
		}
		if f.ImageMaxBytes() != 1024 || f.ImageMaxCount() != 2 {
			t.Errorf("image limits = %d/%d, want 1024/2", f.ImageMaxBytes(), f.ImageMaxCount())
		}
	})

	t.Run("replace image action rejected", func(t *testing.T) {
		cfg := base()
		cfg.Images = &LLMImageConfig{Enabled: true, Action: "replace"}
		if _, err := NewLLMFilter(cfg); err == nil {
			t.Fatal("expected error for replace image action")
		}
	})

	t.Run("disabled images section ignored", func(t *testing.T) {
		cfg := base()
		cfg.Images = &LLMImageConfig{Enabled: false, Action: "replace"} // invalid action ignored when disabled
		f, err := NewLLMFilter(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if f.ImagesEnabled() {
			t.Error("images should be disabled")
		}
	})
}
