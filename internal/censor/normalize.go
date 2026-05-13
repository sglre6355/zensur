package censor

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Normalized is text after the configurable normalization pipeline, plus a
// rune-level map back to the source.
//
// For each normalized rune i, SrcStart[i] is the rune index in the source
// where the region producing rune i begins, and SrcEnd[i] is the exclusive
// end. A single source rune may expand to several normalized runes (e.g. NFKC
// 'ﬃ' → "ffi" — all three share the same [SrcStart, SrcEnd)) and a single
// normalized rune may cover several source runes (e.g. repeat collapse:
// "aaaa" → "a" — the one kept rune spans the full run).
type Normalized struct {
	Text     string
	Runes    []rune
	SrcStart []int
	SrcEnd   []int
}

// leetMap rewrites common digit/symbol look-alikes to their canonical letter
// form so that patterns written in plain script ("shit") still catch evasions
// like "5h1t".
var leetMap = map[rune]rune{
	'0': 'o',
	'1': 'i',
	'3': 'e',
	'4': 'a',
	'5': 's',
	'7': 't',
	'8': 'b',
	'@': 'a',
	'$': 's',
	'!': 'i',
}

func normalizeText(src string, opts ResolvedOpts) Normalized {
	srcRunes := []rune(src)
	out := make([]rune, len(srcRunes))
	srcStart := make([]int, len(srcRunes))
	srcEnd := make([]int, len(srcRunes))
	for i, r := range srcRunes {
		out[i] = r
		srcStart[i] = i
		srcEnd[i] = i + 1
	}

	if opts.NormalizeUnicode {
		out, srcStart, srcEnd = applyNFKC(out, srcStart, srcEnd)
	}
	if opts.StripMarks {
		out, srcStart, srcEnd = stripMarks(out, srcStart, srcEnd)
	}
	if opts.CaseInsensitive {
		for i, r := range out {
			out[i] = unicode.ToLower(r)
		}
	}
	if opts.Leet {
		for i, r := range out {
			if rep, ok := leetMap[r]; ok {
				out[i] = rep
			}
		}
	}
	if opts.CollapseRepeats {
		out, srcStart, srcEnd = collapseRepeats(out, srcStart, srcEnd)
	}
	if opts.FoldKana {
		for i, r := range out {
			// Hiragana U+3041..U+3096 → Katakana U+30A1..U+30F6.
			if r >= 0x3041 && r <= 0x3096 {
				out[i] = r + 0x60
			}
		}
	}

	return Normalized{
		Text:     string(out),
		Runes:    out,
		SrcStart: srcStart,
		SrcEnd:   srcEnd,
	}
}

func applyNFKC(in []rune, srcStart, srcEnd []int) ([]rune, []int, []int) {
	out := make([]rune, 0, len(in))
	ns := make([]int, 0, len(in))
	ne := make([]int, 0, len(in))
	var buf strings.Builder
	for i, r := range in {
		buf.Reset()
		buf.WriteRune(r)
		for _, nr := range norm.NFKC.String(buf.String()) {
			out = append(out, nr)
			ns = append(ns, srcStart[i])
			ne = append(ne, srcEnd[i])
		}
	}
	return out, ns, ne
}

func stripMarks(in []rune, srcStart, srcEnd []int) ([]rune, []int, []int) {
	out := make([]rune, 0, len(in))
	ns := make([]int, 0, len(in))
	ne := make([]int, 0, len(in))
	for i, r := range in {
		if unicode.Is(unicode.Mn, r) {
			// Combining mark — drop it but extend the previous rune's span so
			// the mark's source position stays inside a matched region.
			if k := len(ne) - 1; k >= 0 && srcEnd[i] > ne[k] {
				ne[k] = srcEnd[i]
			}
			continue
		}
		out = append(out, r)
		ns = append(ns, srcStart[i])
		ne = append(ne, srcEnd[i])
	}
	return out, ns, ne
}

// collapseRepeats collapses runs of 3+ identical runes down to one. Runs of
// 1–2 are preserved so legitimate doubled letters ("book", "schoolwork") are
// not falsely matched.
func collapseRepeats(in []rune, srcStart, srcEnd []int) ([]rune, []int, []int) {
	out := make([]rune, 0, len(in))
	ns := make([]int, 0, len(in))
	ne := make([]int, 0, len(in))
	i := 0
	for i < len(in) {
		j := i
		for j < len(in) && in[j] == in[i] {
			j++
		}
		runLen := j - i
		keep := runLen
		if keep >= 3 {
			keep = 1
		}
		for k := 0; k < keep; k++ {
			out = append(out, in[i+k])
			ns = append(ns, srcStart[i+k])
			if k == keep-1 {
				ne = append(ne, srcEnd[j-1])
			} else {
				ne = append(ne, srcEnd[i+k])
			}
		}
		i = j
	}
	return out, ns, ne
}
