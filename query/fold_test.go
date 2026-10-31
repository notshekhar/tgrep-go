package query

import (
	"regexp"
	"slices"
	"testing"

	"github.com/notshekhar/tgrep-go/ondisk"
	"github.com/notshekhar/tgrep-go/trigram"
)

// foldCorpus is content a case-insensitive query can match through Unicode
// folding that the index does not perform: ſ (U+017F) folds to s, K (U+212A,
// Kelvin) to k, and É/é are a non-ASCII pair. Every file the matcher accepts
// must survive the plan, or an indexed search silently loses it.
var foldCorpus = []string{
	"SHELLSHOCK",
	"BaShDoOr",
	"ſhellſhocK",
	"baſhdoor",
	"PREFIXhelloSUFFIX",
	"prefixHELLOsuffix",
	"HELLO",
	"hello",
	"prefixÉsuffix",
	"prefixésuffix",
	"AbC",
	"aBC",
	"ABC",
	"CAFÉ",
	"café",
	"unrelated decoy",
}

// lookupOver indexes contents the way the builder does, one file id each.
func lookupOver(contents []string) Lookup {
	inverted := make(map[uint32][]ondisk.PostingEntry)
	for id, content := range contents {
		for tri, m := range trigram.ExtractMergedMasks([]byte(content)) {
			inverted[tri] = append(inverted[tri], ondisk.PostingEntry{
				FileID: uint32(id), LocMask: m.LocMask, NextMask: m.NextMask,
			})
		}
	}
	return func(hash uint32) []ondisk.PostingEntry { return inverted[hash] }
}

// assertKeepsEveryMatch checks the soundness rule against the real matcher:
// a candidate set may be too wide, never too narrow.
func assertKeepsEveryMatch(t *testing.T, label string, plan Plan, matcher *regexp.Regexp, lookup Lookup) []uint32 {
	t.Helper()
	candidates := Execute(plan, lookup)
	hits := 0
	for id, content := range foldCorpus {
		if !matcher.MatchString(content) {
			continue
		}
		hits++
		if !slices.Contains(candidates, uint32(id)) {
			t.Errorf("%s lost %q: plan %s", label, content, plan)
		}
	}
	if hits == 0 {
		t.Fatalf("%s: the corpus must exercise the pattern", label)
	}
	return candidates
}

func TestCaseFoldedPlansNarrowWithoutLosingMatches(t *testing.T) {
	lookup := lookupOver(foldCorpus)
	cases := []struct {
		pattern string
		ci      bool
	}{
		{"(?i)shellshock|bashdoor", false},
		{"shellshock|bashdoor", true},
		{"(?i:shellshock)|(?i:bashdoor)", false},
		{"(?im)^shellshock|bashdoor", false},
		{"PREFIX(?i:hello)SUFFIX", false},
		{"(?i)prefix(?-i:HELLO)suffix", false},
		{"(?-i)HELLO", true},
		{"[Hh][Ee][Ll][Ll][Oo]", false},
		{"(?i)prefixésuffix", false},
		// A case-sensitive neighbour shares the run with folded letters, so
		// the whole run must be looked up in the index's lowercased form: the
		// file holding "ABC" has no "Abc" trigram to find.
		{"A(?i:bc)", false},
		{"(?i:ab)C", false},
	}
	for _, c := range cases {
		plan := mustBuild(t, c.pattern, c.ci)
		if plan.IsMatchAll() {
			t.Errorf("%q must narrow candidates", c.pattern)
			continue
		}
		expr := c.pattern
		if c.ci {
			expr = "(?i)" + expr
		}
		candidates := assertKeepsEveryMatch(t, c.pattern, plan, regexp.MustCompile(expr), lookup)
		if len(candidates) == len(foldCorpus) {
			t.Errorf("%q kept every file: %s", c.pattern, plan)
		}
	}
}

func TestCaseFoldedLiteralsKeepUnicodeVariants(t *testing.T) {
	lookup := lookupOver(foldCorpus)
	cases := []struct {
		literal string
		narrows bool
	}{
		{"café", true},
		{"CAFÉ", true},
		{"shellshock", true},
		{"prefixésuffix", true},
		{"hello", true},
		{"ab", false},
		{"", false},
	}
	for _, c := range cases {
		plan := BuildLiteral(c.literal, true)
		if plan.IsMatchAll() == c.narrows {
			t.Errorf("BuildLiteral(%q, ci) = %s, narrows want %v", c.literal, plan, c.narrows)
		}
		if plan.IsMatchAll() {
			continue
		}
		matcher := regexp.MustCompile("(?i)" + regexp.QuoteMeta(c.literal))
		assertKeepsEveryMatch(t, c.literal, plan, matcher, lookup)
	}
}

func TestAmbiguousFoldsRequireNoTrigrams(t *testing.T) {
	for _, pattern := range []string{"(?i)k", "(?i)K", "(?i)é", "(?i)hello|.", "(?i:hello)?", "[A-Z][a-z][0-9]"} {
		if plan := mustBuild(t, pattern, false); !plan.IsMatchAll() {
			t.Errorf("%q planned as %s, want MatchAll", pattern, plan)
		}
	}
	// The mandatory prefix of an optional tail is still usable.
	if plan := mustBuild(t, "(?i)hello?", false); plan.IsMatchAll() {
		t.Error(`"(?i)hello?" must keep the mandatory "hell"`)
	}
}

func TestLongCaseFoldedLiteralStillPlans(t *testing.T) {
	literal, content := "", ""
	for range 4096 {
		literal += "[(HeLLo.+)é]"
		content += "[(HELLO.+)É]"
	}
	plan := BuildLiteral(literal, true)
	if plan.IsMatchAll() {
		return // degrading to a scan is allowed; dropping the file is not
	}
	if got := Execute(plan, lookupOver([]string{content})); !slices.Equal(got, []uint32{0}) {
		t.Errorf("long literal lost its only file: %v", got)
	}
}
