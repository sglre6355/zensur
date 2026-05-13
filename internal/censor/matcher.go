package censor

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// Action is what the bot should do when a rule matches.
type Action int

const (
	ActionLog Action = iota
	ActionDelete
	ActionWarn
	ActionReplace
)

func (a Action) String() string {
	switch a {
	case ActionLog:
		return "log"
	case ActionDelete:
		return "delete"
	case ActionWarn:
		return "warn"
	case ActionReplace:
		return "replace"
	}
	return "unknown"
}

func parseAction(s string) (Action, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "delete":
		return ActionDelete, nil
	case "log":
		return ActionLog, nil
	case "warn":
		return ActionWarn, nil
	case "replace":
		return ActionReplace, nil
	}
	return 0, fmt.Errorf("unknown action %q (want log|delete|warn|replace)", s)
}

// Mode is how patterns are matched against the normalized message.
type Mode int

const (
	ModeSubstring Mode = iota
	ModeWord
	ModeRegex
)

func parseMode(s string) (Mode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "substring":
		return ModeSubstring, nil
	case "word":
		return ModeWord, nil
	case "regex":
		return ModeRegex, nil
	}
	return 0, fmt.Errorf("unknown mode %q (want substring|word|regex)", s)
}

// Rule is a compiled, ready-to-run rule.
type Rule struct {
	ID          string
	Mode        Mode
	Action      Action
	Notice      string
	Replacement string
	Opts        ResolvedOpts

	// literals are patterns normalized with Opts. Used for substring/word modes.
	literals [][]rune
	// regexes are compiled regexp patterns. Used for regex mode. The regex is
	// matched against the normalized text.
	regexes []*regexp.Regexp
	// allow phrases (already normalized with Opts) that exempt the message from
	// this rule when any of them appears in the normalized form.
	allow []string
}

// Hit is a single match against the source message.
type Hit struct {
	RuleID   string
	Action   Action
	Pattern  string
	SrcStart int // inclusive rune index in source
	SrcEnd   int // exclusive rune index in source
}

// Ruleset is a compiled set of rules along with global fallbacks.
type Ruleset struct {
	Rules                   []*Rule
	Notice                  string
	NoticeAutoDeleteSeconds int
	Replacement             string

	byID map[string]*Rule
}

// Compile turns a Config into an executable Ruleset.
func Compile(cfg *Config) (*Ruleset, error) {
	rs := &Ruleset{
		Notice:                  cfg.Notice,
		NoticeAutoDeleteSeconds: cfg.NoticeAutoDeleteSeconds,
		Replacement:             cfg.Replacement,
		byID:                    make(map[string]*Rule, len(cfg.Rules)),
	}
	for i, raw := range cfg.Rules {
		r, err := compileRule(raw, cfg.Defaults)
		if err != nil {
			return nil, fmt.Errorf("rule %d (id=%q): %w", i, raw.ID, err)
		}
		if _, dup := rs.byID[r.ID]; dup {
			return nil, fmt.Errorf("duplicate rule id %q", r.ID)
		}
		rs.byID[r.ID] = r
		rs.Rules = append(rs.Rules, r)
	}
	return rs, nil
}

func compileRule(raw RawRule, defaults NormalizeOpts) (*Rule, error) {
	if raw.ID == "" {
		return nil, fmt.Errorf("missing id")
	}
	if len(raw.Patterns) == 0 {
		return nil, fmt.Errorf("no patterns")
	}
	mode, err := parseMode(raw.Mode)
	if err != nil {
		return nil, err
	}
	action, err := parseAction(raw.Action)
	if err != nil {
		return nil, err
	}
	opts := resolveOpts(defaults, raw.NormalizeOpts)

	r := &Rule{
		ID:          raw.ID,
		Mode:        mode,
		Action:      action,
		Notice:      raw.Notice,
		Replacement: raw.Replacement,
		Opts:        opts,
	}

	switch mode {
	case ModeSubstring, ModeWord:
		for i, p := range raw.Patterns {
			if p == "" {
				return nil, fmt.Errorf("pattern %d is empty", i)
			}
			n := normalizeText(p, opts)
			if len(n.Runes) == 0 {
				return nil, fmt.Errorf("pattern %d normalizes to empty string", i)
			}
			r.literals = append(r.literals, n.Runes)
		}
	case ModeRegex:
		for i, p := range raw.Patterns {
			re, err := regexp.Compile(p)
			if err != nil {
				return nil, fmt.Errorf("regex pattern %d: %w", i, err)
			}
			r.regexes = append(r.regexes, re)
		}
	}
	for _, a := range raw.Allow {
		if a == "" {
			continue
		}
		r.allow = append(r.allow, normalizeText(a, opts).Text)
	}
	return r, nil
}

// RuleByID returns the compiled rule with the given ID, or nil.
func (rs *Ruleset) RuleByID(id string) *Rule {
	return rs.byID[id]
}

// Match scans src against every rule and returns all hits.
func (rs *Ruleset) Match(src string) []Hit {
	var hits []Hit
	for _, r := range rs.Rules {
		hits = append(hits, r.match(src)...)
	}
	return hits
}

func (r *Rule) match(src string) []Hit {
	n := normalizeText(src, r.Opts)
	for _, a := range r.allow {
		if a != "" && strings.Contains(n.Text, a) {
			return nil
		}
	}

	switch r.Mode {
	case ModeSubstring:
		return r.matchLiteral(n, false)
	case ModeWord:
		return r.matchLiteral(n, true)
	case ModeRegex:
		return r.matchRegex(n)
	}
	return nil
}

func (r *Rule) matchLiteral(n Normalized, wordBoundary bool) []Hit {
	var hits []Hit
	for _, lit := range r.literals {
		start := 0
		for {
			idx := runeIndex(n.Runes[start:], lit)
			if idx < 0 {
				break
			}
			s := start + idx
			e := s + len(lit)
			if !wordBoundary || (isRuneBoundary(n.Runes, s) && isRuneBoundary(n.Runes, e)) {
				ss, se := srcSpan(n, s, e)
				hits = append(hits, Hit{
					RuleID:   r.ID,
					Action:   r.Action,
					Pattern:  string(lit),
					SrcStart: ss,
					SrcEnd:   se,
				})
			}
			start = e
			if start >= len(n.Runes) {
				break
			}
		}
	}
	return hits
}

func (r *Rule) matchRegex(n Normalized) []Hit {
	var hits []Hit
	for _, re := range r.regexes {
		for _, m := range re.FindAllStringIndex(n.Text, -1) {
			rs := byteToRune(n.Text, m[0])
			re2 := byteToRune(n.Text, m[1])
			ss, se := srcSpan(n, rs, re2)
			hits = append(hits, Hit{
				RuleID:   r.ID,
				Action:   r.Action,
				Pattern:  re.String(),
				SrcStart: ss,
				SrcEnd:   se,
			})
		}
	}
	return hits
}

func runeIndex(haystack, needle []rune) int {
	if len(needle) == 0 {
		return 0
	}
	if len(needle) > len(haystack) {
		return -1
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j, nr := range needle {
			if haystack[i+j] != nr {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// isRuneBoundary returns true when the position is at a transition between a
// "word" rune (letter/digit/underscore) and a non-word rune, or at either end
// of the slice. Designed for languages with whitespace word boundaries.
func isRuneBoundary(runes []rune, pos int) bool {
	if pos == 0 || pos == len(runes) {
		return true
	}
	return isWordRune(runes[pos-1]) != isWordRune(runes[pos])
}

func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
}

// srcSpan maps a [start, end) rune range in normalized text to the
// corresponding [start, end) rune range in the source.
func srcSpan(n Normalized, start, end int) (int, int) {
	if len(n.Runes) == 0 || start >= len(n.SrcStart) {
		return start, end
	}
	s := n.SrcStart[start]
	if end <= 0 {
		return s, s
	}
	if end > len(n.SrcEnd) {
		end = len(n.SrcEnd)
	}
	e := n.SrcEnd[end-1]
	return s, e
}

func byteToRune(s string, byteOffset int) int {
	if byteOffset <= 0 {
		return 0
	}
	idx := 0
	for i := range s {
		if i >= byteOffset {
			return idx
		}
		idx++
	}
	return idx
}

// Censor returns a copy of src with the source spans of each hit replaced by
// the per-rule Replacement (falling back to ruleset.Replacement). Overlapping
// hits are merged into a single replacement using the first hit's rule for
// substitution.
func (rs *Ruleset) Censor(src string, hits []Hit) string {
	if len(hits) == 0 {
		return src
	}
	merged := mergeHits(hits)
	srcRunes := []rune(src)
	var b strings.Builder
	cursor := 0
	for _, h := range merged {
		if h.SrcStart < cursor {
			h.SrcStart = cursor
		}
		if h.SrcStart > h.SrcEnd || h.SrcStart > len(srcRunes) {
			continue
		}
		if h.SrcEnd > len(srcRunes) {
			h.SrcEnd = len(srcRunes)
		}
		b.WriteString(string(srcRunes[cursor:h.SrcStart]))
		rep := rs.Replacement
		if r := rs.RuleByID(h.RuleID); r != nil && r.Replacement != "" {
			rep = r.Replacement
		}
		if rep == "" {
			rep = "***"
		}
		b.WriteString(rep)
		cursor = h.SrcEnd
	}
	if cursor < len(srcRunes) {
		b.WriteString(string(srcRunes[cursor:]))
	}
	return b.String()
}

func mergeHits(hits []Hit) []Hit {
	sorted := make([]Hit, len(hits))
	copy(sorted, hits)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].SrcStart != sorted[j].SrcStart {
			return sorted[i].SrcStart < sorted[j].SrcStart
		}
		return sorted[i].SrcEnd > sorted[j].SrcEnd
	})
	out := sorted[:0]
	for _, h := range sorted {
		if len(out) == 0 {
			out = append(out, h)
			continue
		}
		last := &out[len(out)-1]
		if h.SrcStart <= last.SrcEnd {
			if h.SrcEnd > last.SrcEnd {
				last.SrcEnd = h.SrcEnd
			}
			continue
		}
		out = append(out, h)
	}
	return out
}

// MaxAction returns the most disruptive action among hits, in order
// log < warn < delete < replace.
func MaxAction(hits []Hit) Action {
	max := ActionLog
	rank := func(a Action) int {
		switch a {
		case ActionLog:
			return 0
		case ActionWarn:
			return 1
		case ActionDelete:
			return 2
		case ActionReplace:
			return 3
		}
		return 0
	}
	for _, h := range hits {
		if rank(h.Action) > rank(max) {
			max = h.Action
		}
	}
	return max
}
