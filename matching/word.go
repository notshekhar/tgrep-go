package matching

import (
	"regexp/syntax"
	"strings"
)

// wordWrap applies -w to one pattern.
//
// ripgrep defines -w as \b{start-half} and \b{end-half}: the match must not be
// preceded or followed by a word character. Plain \b is not that. Beside a
// non-word character, \b demands a word character on the OTHER side, so
// `\b(?:a-)\b` cannot match "a- b" at all. RE2 has no half boundaries, but
// where the pattern's edge character is known they reduce to one assertion:
//
//   - edge is a word character: "no word character outside" is \b;
//   - edge is a non-word character: "no word character outside" is \B, since
//     both sides of the edge are then non-word (the text's start and end count
//     as non-word too, exactly as the half boundary wants).
//
// An alternation whose branches disagree is wrapped branch by branch, so
// `a-|-a` gets the right edges for each. When an edge is still unknown — `.`,
// or a mixed class — no single assertion is right, and \b is kept: the
// behaviour before this existed, correct for a word-character edge.
func wordWrap(p string) string {
	re, err := syntax.Parse(p, syntax.Perl)
	if err != nil {
		// Let the real compile report it.
		return `\b(?:` + p + `)\b`
	}
	return wrapParsed(re, p)
}

func wrapParsed(re *syntax.Regexp, source string) string {
	start, end := edgeKind(re, true), edgeKind(re, false)
	if re.Op == syntax.OpAlternate && (start == edgeUnknown || end == edgeUnknown) {
		branches := make([]string, len(re.Sub))
		for i, sub := range re.Sub {
			branches[i] = wrapParsed(sub, sub.String())
		}
		return `(?:` + strings.Join(branches, `|`) + `)`
	}
	return edgeAssertion(start) + `(?:` + source + `)` + edgeAssertion(end)
}

type edge int

const (
	edgeUnknown edge = iota
	edgeWord
	edgeNonWord
)

func edgeAssertion(e edge) string {
	if e == edgeNonWord {
		return `\B`
	}
	return `\b`
}

// edgeKind classifies the first (or last) character every match of re has.
func edgeKind(re *syntax.Regexp, first bool) edge {
	switch re.Op {
	case syntax.OpLiteral:
		if len(re.Rune) == 0 {
			return edgeUnknown
		}
		r := re.Rune[len(re.Rune)-1]
		if first {
			r = re.Rune[0]
		}
		return runeKind(r)
	case syntax.OpCharClass:
		kind := edgeUnknown
		for i := 0; i < len(re.Rune); i += 2 {
			k := rangeKind(re.Rune[i], re.Rune[i+1])
			if k == edgeUnknown || (kind != edgeUnknown && k != kind) {
				return edgeUnknown
			}
			kind = k
		}
		return kind
	case syntax.OpCapture, syntax.OpPlus:
		return edgeKind(re.Sub[0], first)
	case syntax.OpRepeat:
		if re.Min >= 1 {
			return edgeKind(re.Sub[0], first)
		}
	case syntax.OpConcat:
		// Only the outermost child decides, and only when it must consume a
		// character; an optional or zero-width one leaves the edge open.
		if len(re.Sub) > 0 {
			sub := re.Sub[len(re.Sub)-1]
			if first {
				sub = re.Sub[0]
			}
			return edgeKind(sub, first)
		}
	case syntax.OpAlternate:
		kind := edgeKind(re.Sub[0], first)
		for _, sub := range re.Sub[1:] {
			if edgeKind(sub, first) != kind {
				return edgeUnknown
			}
		}
		return kind
	}
	return edgeUnknown
}

// isWordByte is RE2's word character, the definition \b and \B use.
func isWordByte(r rune) bool {
	return r == '_' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

func runeKind(r rune) edge {
	if isWordByte(r) {
		return edgeWord
	}
	return edgeNonWord
}

// rangeKind is the kind every rune in lo..hi shares, or unknown.
func rangeKind(lo, hi rune) edge {
	kind := runeKind(lo)
	for _, boundary := range []rune{'0', '9' + 1, 'A', 'Z' + 1, '_', '_' + 1, 'a', 'z' + 1} {
		if lo < boundary && boundary <= hi {
			return edgeUnknown
		}
	}
	return kind
}
