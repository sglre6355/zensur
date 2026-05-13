package censor

import (
	"reflect"
	"testing"
)

func ptrBool(b bool) *bool { return &b }

func mustCompile(t *testing.T, rules []RawRule) *Ruleset {
	t.Helper()
	cfg := &Config{
		Rules:       rules,
		Replacement: "***",
	}
	rs, err := Compile(cfg)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return rs
}

func TestMatch_SubstringEnglish(t *testing.T) {
	rs := mustCompile(t, []RawRule{{
		ID: "en", Patterns: []string{"badword"}, Mode: "substring", Action: "delete",
	}})
	got := rs.Match("you said badword here")
	if len(got) != 1 || got[0].RuleID != "en" {
		t.Fatalf("expected 1 hit, got %+v", got)
	}
}

func TestMatch_WordBoundary(t *testing.T) {
	rs := mustCompile(t, []RawRule{{
		ID: "en", Patterns: []string{"ass"}, Mode: "word", Action: "delete",
	}})
	if got := rs.Match("classy passing assassin"); len(got) != 0 {
		t.Fatalf("word mode should not match substrings: %+v", got)
	}
	if got := rs.Match("kick his ass now"); len(got) != 1 {
		t.Fatalf("word mode should match isolated word: %+v", got)
	}
}

func TestMatch_CaseInsensitive(t *testing.T) {
	rs := mustCompile(t, []RawRule{{
		ID: "en", Patterns: []string{"badword"}, Mode: "substring", Action: "delete",
	}})
	if got := rs.Match("BadWord"); len(got) != 1 {
		t.Fatalf("case-insensitive default should match: %+v", got)
	}
}

func TestMatch_Japanese(t *testing.T) {
	rs := mustCompile(t, []RawRule{{
		ID: "jp", Patterns: []string{"ばか"}, Mode: "substring", Action: "delete",
	}})
	if got := rs.Match("こいつばかだな"); len(got) != 1 {
		t.Fatalf("japanese substring should match: %+v", got)
	}
}

func TestMatch_KanaFold(t *testing.T) {
	rs := mustCompile(t, []RawRule{{
		ID:            "jp",
		Patterns:      []string{"ばか"},
		Mode:          "substring",
		Action:        "delete",
		NormalizeOpts: NormalizeOpts{FoldKana: ptrBool(true)},
	}})
	if got := rs.Match("バカと言うな"); len(got) != 1 {
		t.Fatalf("kana-fold should match katakana version: %+v", got)
	}
}

func TestMatch_NFKCFullwidth(t *testing.T) {
	rs := mustCompile(t, []RawRule{{
		ID: "en", Patterns: []string{"badword"}, Mode: "substring", Action: "delete",
	}})
	// Full-width letters via NFKC compatibility normalization.
	if got := rs.Match("ｂａｄｗｏｒｄ"); len(got) != 1 {
		t.Fatalf("nfkc should fold fullwidth: %+v", got)
	}
}

func TestMatch_LeetOptIn(t *testing.T) {
	off := mustCompile(t, []RawRule{{
		ID: "en", Patterns: []string{"shit"}, Mode: "substring", Action: "delete",
	}})
	if got := off.Match("5h1t"); len(got) != 0 {
		t.Fatalf("leet off should not match: %+v", got)
	}
	on := mustCompile(t, []RawRule{{
		ID: "en", Patterns: []string{"shit"}, Mode: "substring", Action: "delete",
		NormalizeOpts: NormalizeOpts{Leet: ptrBool(true)},
	}})
	if got := on.Match("5h1t"); len(got) != 1 {
		t.Fatalf("leet on should match: %+v", got)
	}
}

func TestMatch_CollapseRepeats(t *testing.T) {
	rs := mustCompile(t, []RawRule{{
		ID: "en", Patterns: []string{"fuck"}, Mode: "substring", Action: "delete",
		NormalizeOpts: NormalizeOpts{CollapseRepeats: ptrBool(true)},
	}})
	if got := rs.Match("fuuuuuck"); len(got) != 1 {
		t.Fatalf("collapse should match: %+v", got)
	}
	// Doubled letters should NOT collapse — "book" must remain "book".
	books := mustCompile(t, []RawRule{{
		ID: "x", Patterns: []string{"bok"}, Mode: "substring", Action: "delete",
		NormalizeOpts: NormalizeOpts{CollapseRepeats: ptrBool(true)},
	}})
	if got := books.Match("book"); len(got) != 0 {
		t.Fatalf("collapse should keep doubles: %+v", got)
	}
}

func TestMatch_AllowList(t *testing.T) {
	rs := mustCompile(t, []RawRule{{
		ID: "en", Patterns: []string{"cunt"}, Mode: "substring", Action: "delete",
		Allow: []string{"scunthorpe"},
	}})
	if got := rs.Match("I love Scunthorpe"); len(got) != 0 {
		t.Fatalf("allowlist should suppress scunthorpe: %+v", got)
	}
	if got := rs.Match("that cunt"); len(got) != 1 {
		t.Fatalf("allowlist should not suppress when the allowed phrase is absent: %+v", got)
	}
}

func TestMatch_Regex(t *testing.T) {
	rs := mustCompile(t, []RawRule{{
		ID: "url", Patterns: []string{`https?://\S+`}, Mode: "regex", Action: "delete",
	}})
	if got := rs.Match("visit http://example.com today"); len(got) != 1 {
		t.Fatalf("regex should match url: %+v", got)
	}
}

func TestCensor_SpansReplaced(t *testing.T) {
	rs := mustCompile(t, []RawRule{{
		ID: "en", Patterns: []string{"badword"}, Mode: "substring", Action: "replace",
		Replacement: "[redacted]",
	}})
	src := "you said badword here"
	hits := rs.Match(src)
	got := rs.Censor(src, hits)
	want := "you said [redacted] here"
	if got != want {
		t.Fatalf("censor: got %q want %q", got, want)
	}
}

func TestCensor_OverlappingHitsMerge(t *testing.T) {
	rs := mustCompile(t, []RawRule{
		{ID: "a", Patterns: []string{"foo"}, Mode: "substring", Action: "replace", Replacement: "X"},
		{ID: "b", Patterns: []string{"oob"}, Mode: "substring", Action: "replace", Replacement: "Y"},
	})
	src := "foobar"
	got := rs.Censor(src, rs.Match(src))
	// "foo" [0,3) and "oob" [1,4) overlap → merge into [0,4) using first rule's
	// replacement "X". Tail "ar" remains.
	want := "Xar"
	if got != want {
		t.Fatalf("merge: got %q want %q", got, want)
	}
}

func TestCensor_NFKCSpan(t *testing.T) {
	rs := mustCompile(t, []RawRule{{
		ID: "en", Patterns: []string{"badword"}, Mode: "substring", Action: "replace",
		Replacement: "***",
	}})
	src := "say ｂａｄｗｏｒｄ now"
	got := rs.Censor(src, rs.Match(src))
	want := "say *** now"
	if got != want {
		t.Fatalf("nfkc span: got %q want %q", got, want)
	}
}

func TestMaxAction(t *testing.T) {
	cases := []struct {
		hits []Hit
		want Action
	}{
		{nil, ActionLog},
		{[]Hit{{Action: ActionLog}}, ActionLog},
		{[]Hit{{Action: ActionWarn}, {Action: ActionLog}}, ActionWarn},
		{[]Hit{{Action: ActionDelete}, {Action: ActionWarn}}, ActionDelete},
		{[]Hit{{Action: ActionReplace}, {Action: ActionDelete}}, ActionReplace},
	}
	for _, c := range cases {
		if got := MaxAction(c.hits); got != c.want {
			t.Errorf("MaxAction(%+v) = %v, want %v", c.hits, got, c.want)
		}
	}
}

func TestParseAction(t *testing.T) {
	cases := map[string]Action{
		"":        ActionDelete,
		"log":     ActionLog,
		"delete":  ActionDelete,
		"warn":    ActionWarn,
		"replace": ActionReplace,
		"DELETE":  ActionDelete,
		" warn ":  ActionWarn,
	}
	for in, want := range cases {
		got, err := parseAction(in)
		if err != nil {
			t.Errorf("parseAction(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parseAction(%q) = %v, want %v", in, got, want)
		}
	}
	if _, err := parseAction("nope"); err == nil {
		t.Errorf("parseAction(nope) should error")
	}
}

func TestNormalize_OffsetMapping(t *testing.T) {
	// "ｆoo" — full-width F should NFKC-fold to 'f' (single rune in, single
	// rune out), preserving the 1:1 mapping for plain ASCII tail.
	opts := ResolvedOpts{NormalizeUnicode: true, CaseInsensitive: true, StripMarks: true}
	n := normalizeText("ｆoo", opts)
	if n.Text != "foo" {
		t.Fatalf("normalized text = %q, want foo", n.Text)
	}
	if !reflect.DeepEqual(n.SrcStart, []int{0, 1, 2}) {
		t.Fatalf("SrcStart = %v, want [0 1 2]", n.SrcStart)
	}
}
