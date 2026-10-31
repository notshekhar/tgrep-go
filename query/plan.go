// Package query turns a regular expression into a plan over trigram posting
// lists, so a search reads only the files that could possibly match.
//
// The plan is a tree of AND and OR nodes over individual trigram lookups, plus
// one escape hatch: MatchAll, meaning the pattern gave the index nothing to
// work with and every file has to be scanned. MatchAll is not a failure — it
// is the honest answer for `.*` — and it must be produced whenever the
// alternative would be a candidate set that could miss a real match.
//
// The soundness rule the whole package obeys: a candidate set may be larger
// than the true match set, never smaller. Everything here either narrows on
// evidence the pattern genuinely requires, or gives up and returns MatchAll.
package query

import (
	"fmt"
	"regexp"
	"regexp/syntax"
	"slices"
	"unicode"
	"unicode/utf8"

	"github.com/notshekhar/tgrep-go/trigram"
)

// TrigramQuery is one trigram lookup, optionally constrained by the byte that
// must follow it.
//
// ExpectedNext is taken from the parsed pattern rather than from the raw
// pattern text, so it stays correct for regexes where the two differ (escapes,
// case folding, character classes reduced to single literals).
type TrigramQuery struct {
	Hash uint32
	// ExpectedNext is the byte that follows this trigram in the literal it
	// came from. HasNext is false when the trigram sat at the literal's end,
	// where nothing is known about what follows.
	ExpectedNext byte
	HasNext      bool
}

// Kind distinguishes the three node types.
type Kind int

const (
	// KindAnd requires every trigram: the intersection of their posting lists.
	KindAnd Kind = iota
	// KindOr admits any branch: the union of their results.
	KindOr
	// KindMatchAll means no trigram could be extracted — scan everything.
	KindMatchAll
)

// Plan is a node in the query plan tree.
type Plan struct {
	Kind Kind
	// Queries is set for KindAnd.
	Queries []TrigramQuery
	// Branches is set for KindOr.
	Branches []Plan
}

// MatchAll is the plan that narrows nothing.
func MatchAll() Plan { return Plan{Kind: KindMatchAll} }

// IsMatchAll reports whether this plan requires scanning every file.
func (p Plan) IsMatchAll() bool { return p.Kind == KindMatchAll }

// String renders a plan for diagnostics and for the tests that assert shape.
func (p Plan) String() string {
	switch p.Kind {
	case KindMatchAll:
		return "MatchAll"
	case KindAnd:
		out := "And("
		for i, q := range p.Queries {
			if i > 0 {
				out += " "
			}
			out += fmt.Sprintf("%q", triBytes(q.Hash))
			if q.HasNext {
				out += fmt.Sprintf("+%q", string(rune(q.ExpectedNext)))
			}
		}
		return out + ")"
	case KindOr:
		out := "Or("
		for i, b := range p.Branches {
			if i > 0 {
				out += " | "
			}
			out += b.String()
		}
		return out + ")"
	}
	return "?"
}

func triBytes(h uint32) string {
	return string([]byte{byte(h >> 16), byte(h >> 8), byte(h)})
}

// Build parses a regular expression and returns its plan.
//
// caseInsensitive is applied as the parser's starting flag rather than to the
// literals afterwards, so an inline (?-i) inside the pattern still switches it
// off for its own scope, exactly as it does in the matcher.
func Build(pattern string, caseInsensitive bool) (Plan, error) {
	flags := syntax.Perl
	if caseInsensitive {
		flags |= syntax.FoldCase
	}
	re, err := syntax.Parse(pattern, flags)
	if err != nil {
		return MatchAll(), fmt.Errorf("regex parse error: %w", err)
	}
	// Simplify rewrites counted repetitions into forms the walk below can read
	// uniformly; it never changes the language, so it cannot affect soundness.
	return simplify(decompose(re.Simplify())), nil
}

// BuildLiteral returns the plan for a fixed string.
//
// A case-insensitive literal goes through the regex planner, because folding
// is not ASCII-only: `shellshock` matches `ſhellſhocK`, and only the parsed
// form knows which letters have such variants. A literal that somehow fails
// to plan scans everything rather than being rejected — that only gives up
// narrowing, and the matcher still verifies every line.
func BuildLiteral(literal string, caseInsensitive bool) Plan {
	if caseInsensitive {
		plan, err := Build(regexp.QuoteMeta(literal), true)
		if err != nil {
			return MatchAll()
		}
		return plan
	}
	return simplify(literalPlan([]byte(literal)))
}

// BuildMulti returns one plan covering every pattern the user supplied.
//
// A file matches if any pattern matches, so the plans are unioned. Building
// from only the first pattern would narrow candidates using one pattern and
// then search for all of them, silently hiding files that only the later
// patterns match.
//
// As with alternation, a single MatchAll branch absorbs the whole plan: an
// unindexable pattern can match anywhere, so no candidate set is safe.
func BuildMulti(patterns []string, fixedString, caseInsensitive bool) (Plan, error) {
	plans := make([]Plan, 0, len(patterns))
	for _, pattern := range patterns {
		var plan Plan
		if fixedString {
			plan = BuildLiteral(pattern, caseInsensitive)
		} else {
			p, err := Build(pattern, caseInsensitive)
			if err != nil {
				return MatchAll(), err
			}
			plan = p
		}
		if plan.IsMatchAll() {
			return MatchAll(), nil
		}
		plans = append(plans, plan)
	}
	switch len(plans) {
	case 0:
		return MatchAll(), nil
	case 1:
		return plans[0], nil
	default:
		return Plan{Kind: KindOr, Branches: plans}, nil
	}
}

// literalPlan converts a byte sequence into ANDed trigram queries.
//
// Each query carries the byte that follows its trigram in this literal, which
// is what lets the index reject a file on a fourth character it never promised
// to hold.
func literalPlan(bytes []byte) Plan {
	if len(bytes) < 3 {
		// Nothing shorter than a trigram can be looked up at all.
		return MatchAll()
	}
	queries := make([]TrigramQuery, 0, len(bytes)-2)
	for i := 0; i+3 <= len(bytes); i++ {
		q := TrigramQuery{Hash: trigram.Hash(bytes[i], bytes[i+1], bytes[i+2])}
		if i+3 < len(bytes) {
			q.ExpectedNext, q.HasNext = bytes[i+3], true
		}
		queries = append(queries, q)
	}
	return Plan{Kind: KindAnd, Queries: queries}
}

// asciiLower lowercases ASCII only.
//
// Deliberately not Unicode-aware: the index lowercases ASCII only when it
// extracts trigrams, so a query that folded a non-ASCII letter would look up a
// trigram the index never wrote and miss a file that genuinely matches. The
// two lowercasings have to agree, and the index's is the one that already
// exists on disk.
func asciiLower(s string) string {
	out := []byte(s)
	asciiLowerInPlace(out)
	return string(out)
}

func asciiLowerInPlace(b []byte) {
	for i, c := range b {
		b[i] = lowerByte(c)
	}
}

func lowerByte(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 32
	}
	return c
}

// asciiFold returns the one byte every case variant of r lowercases to, when
// all of them are ASCII.
//
// That is the only folding the index can answer for, because it stores ASCII-
// lowercased trigrams and nothing else. `s` fails it — ſ (U+017F) is one of
// its variants — as do `k` (Kelvin sign) and every non-ASCII letter, and each
// of those must break the literal run instead of being looked up.
func asciiFold(r rune) (byte, bool) {
	var folded byte
	for f := r; ; {
		if f > unicode.MaxASCII {
			return 0, false
		}
		b := lowerByte(byte(f))
		if f != r && b != folded {
			return 0, false
		}
		folded = b
		if f = unicode.SimpleFold(f); f == r {
			return folded, true
		}
	}
}

// asciiClassByte collapses a character class whose members are all ASCII and
// all lowercase to one byte, such as [Hh]. Anything wider breaks the run.
func asciiClassByte(re *syntax.Regexp) (byte, bool) {
	if re.Op != syntax.OpCharClass || len(re.Rune) == 0 {
		return 0, false
	}
	var folded byte
	for i := 0; i < len(re.Rune); i += 2 {
		lo, hi := re.Rune[i], re.Rune[i+1]
		if lo != hi || lo > unicode.MaxASCII {
			return 0, false
		}
		b := lowerByte(byte(lo))
		if i > 0 && b != folded {
			return 0, false
		}
		folded = b
	}
	return folded, true
}

// literalRun accumulates the bytes of adjacent literal nodes, so trigrams
// spanning the seam between two of them are found, and turns each completed
// run into required trigrams.
type literalRun struct {
	bytes []byte
	// folded is whether any byte in the run stands for a case-insensitive
	// letter. The whole run is then looked up lowercased — including its
	// case-sensitive neighbours, since a file holding "ABC" has the trigram
	// "abc" in the index but not "Abc".
	folded  bool
	queries []TrigramQuery
}

func (r *literalRun) addLiteral(re *syntax.Regexp) {
	fold := re.Flags&syntax.FoldCase != 0
	for _, c := range re.Rune {
		switch {
		case !fold || unicode.SimpleFold(c) == c:
			// Under FoldCase the parser stores a letter as its smallest
			// variant (é as É), so only a rune with no variants at all may be
			// taken literally.
			r.bytes = utf8.AppendRune(r.bytes, c)
		default:
			if b, ok := asciiFold(c); ok {
				r.addFolded(b)
			} else {
				r.flush()
			}
		}
	}
}

func (r *literalRun) addFolded(b byte) {
	r.bytes = append(r.bytes, b)
	r.folded = true
}

func (r *literalRun) flush() {
	if r.folded {
		asciiLowerInPlace(r.bytes)
	}
	if plan := literalPlan(r.bytes); plan.Kind == KindAnd {
		r.queries = append(r.queries, plan.Queries...)
	}
	r.bytes, r.folded = r.bytes[:0], false
}

func (r *literalRun) plan() Plan {
	r.flush()
	if len(r.queries) == 0 {
		return MatchAll()
	}
	return Plan{Kind: KindAnd, Queries: r.queries}
}

// decompose walks the parsed regex and builds a plan.
func decompose(re *syntax.Regexp) Plan {
	switch re.Op {
	case syntax.OpLiteral:
		var run literalRun
		run.addLiteral(re)
		return run.plan()

	case syntax.OpConcat:
		// A non-literal child breaks the run: nothing can be assumed about the
		// bytes across it.
		var run literalRun
		for _, sub := range re.Sub {
			if sub.Op == syntax.OpLiteral {
				run.addLiteral(sub)
				continue
			}
			if b, ok := asciiClassByte(sub); ok {
				run.addFolded(b)
				continue
			}
			run.flush()
			// A child that yields its own ANDed trigrams contributes them,
			// since everything in a concatenation must be present. A MatchAll
			// or Or child contributes nothing: it constrains no single
			// trigram, and folding an Or into the surrounding And would
			// require every branch's trigrams at once, which would drop real
			// matches.
			if plan := decompose(sub); plan.Kind == KindAnd {
				run.queries = append(run.queries, plan.Queries...)
			}
		}
		return run.plan()

	case syntax.OpAlternate:
		branches := make([]Plan, 0, len(re.Sub))
		for _, sub := range re.Sub {
			plan := decompose(sub)
			// One unindexable branch makes the whole alternation unindexable:
			// a file matching only that branch would be missing from any
			// candidate set built from the others.
			if plan.IsMatchAll() {
				return MatchAll()
			}
			branches = append(branches, plan)
		}
		if len(branches) == 0 {
			return MatchAll()
		}
		return Plan{Kind: KindOr, Branches: branches}

	case syntax.OpPlus:
		// One or more: the body must appear at least once.
		return decompose(re.Sub[0])

	case syntax.OpRepeat:
		if re.Min >= 1 {
			return decompose(re.Sub[0])
		}
		// Zero occurrences satisfy it, so it requires nothing.
		return MatchAll()

	case syntax.OpStar, syntax.OpQuest:
		// Both are optional, so neither requires any byte to be present.
		return MatchAll()

	case syntax.OpCapture:
		return decompose(re.Sub[0])

	default:
		// Character classes, any-char, anchors, word boundaries, empty match
		// and no-match all constrain nothing this index can look up.
		return MatchAll()
	}
}

// simplify sorts and deduplicates ANDed trigrams and collapses single-branch
// unions.
func simplify(plan Plan) Plan {
	switch plan.Kind {
	case KindAnd:
		queries := plan.Queries
		slices.SortStableFunc(queries, func(a, b TrigramQuery) int {
			switch {
			case a.Hash < b.Hash:
				return -1
			case a.Hash > b.Hash:
				return 1
			}
			return 0
		})

		// Deduplicate by trigram, keeping one query per hash.
		//
		// When the same trigram arrives from two literal runs with different
		// following bytes — `mutex.*mutex_lock` yields "tex" followed by
		// nothing and by '_' — the survivor's ExpectedNext is cleared. Keeping
		// either one would filter files on a context that only one of the two
		// occurrences has to satisfy.
		//
		// (The reference implementation intends this but does not achieve it:
		// its dedup closure mutates the element being discarded rather than
		// the one retained, so the constraint survives. It happens to be
		// harmless there — every query inside an And comes from a literal run
		// that must be present, so any one following byte is genuinely
		// required — but clearing is the conservative reading and costs only
		// a slightly wider candidate set.)
		out := queries[:0]
		for _, q := range queries {
			if len(out) > 0 && out[len(out)-1].Hash == q.Hash {
				last := &out[len(out)-1]
				if last.HasNext != q.HasNext || last.ExpectedNext != q.ExpectedNext {
					last.ExpectedNext, last.HasNext = 0, false
				}
				continue
			}
			out = append(out, q)
		}
		if len(out) == 0 {
			return MatchAll()
		}
		return Plan{Kind: KindAnd, Queries: out}

	case KindOr:
		branches := make([]Plan, 0, len(plan.Branches))
		for _, b := range plan.Branches {
			s := simplify(b)
			// A MatchAll branch absorbs the union, for the same reason it does
			// during decomposition.
			if s.IsMatchAll() {
				return MatchAll()
			}
			branches = append(branches, s)
		}
		switch len(branches) {
		case 0:
			return MatchAll()
		case 1:
			return branches[0]
		default:
			return Plan{Kind: KindOr, Branches: branches}
		}
	}
	return plan
}
