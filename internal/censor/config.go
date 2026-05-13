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
