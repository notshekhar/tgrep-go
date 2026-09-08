package query

import (
	"testing"
)

func mustBuild(t *testing.T, pattern string, ci bool) Plan {
	t.Helper()
	p, err := Build(pattern, ci)
	if err != nil {
		t.Fatalf("build %q: %v", pattern, err)
	}
	return p
}

func TestLiteralBecomesAnAndOfItsWindows(t *testing.T) {
	got := mustBuild(t, "hello", false).String()
	// "hel"+l, "ell"+o, "llo" — sorted by hash, the last carrying no
	// following byte because nothing follows it in the literal.
	want := `And("ell"+"o" "hel"+"l" "llo")`
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestShortPatternsCannotNarrow(t *testing.T) {
	// Nothing shorter than three bytes produces a trigram at all, so the only
	// honest plan is to scan everything.
	for _, pattern := range []string{"", "a", "ab"} {
		if plan := mustBuild(t, pattern, false); !plan.IsMatchAll() {
			t.Errorf("%q planned as %s, want MatchAll", pattern, plan)
		}
	}
	// Exactly three is the shortest that can.
	if plan := mustBuild(t, "abc", false); plan.IsMatchAll() {
		t.Error(`"abc" planned as MatchAll, but it is exactly one trigram`)
	}
}

func TestCaseInsensitiveLowercasesTheLiteral(t *testing.T) {
	insensitive := mustBuild(t, "HELLO", true).String()
	sensitive := mustBuild(t, "hello", false).String()
	if insensitive != sensitive {
		t.Errorf("case-insensitive HELLO planned as %s, want the same as lowercase hello: %s", insensitive, sensitive)
	}

	// An inline (?i) must take the same path as the flag.
	inline := mustBuild(t, "(?i)HELLO", false).String()
	if inline != sensitive {
		t.Errorf("(?i)HELLO planned as %s, want %s", inline, sensitive)
	}
}

// TestNonAsciiIsFoldedTheWayTheIndexFoldsIt pins the one place where matching
// the index matters more than matching Unicode: the builder ASCII-lowercases
// only, so the planner must too, or a case-insensitive query would look up a
// trigram that was never written.
func TestNonAsciiIsFoldedTheWayTheIndexFoldsIt(t *testing.T) {
	if got := asciiLower("ÉCOLE"); got != "École" {
		t.Errorf("asciiLower(ÉCOLE) = %q, want %q — non-ASCII must be left alone", got, "École")
	}
}

func TestOptionalConstructsNarrowNothing(t *testing.T) {
	for _, pattern := range []string{"(hello)*", "(hello)?", "(hello){0,3}", ".*", "^$", `\b`} {
		if plan := mustBuild(t, pattern, false); !plan.IsMatchAll() {
			t.Errorf("%q planned as %s, want MatchAll — it can match with no bytes present", pattern, plan)
		}
	}
}

func TestRequiredRepetitionsNarrow(t *testing.T) {
	for _, pattern := range []string{"(hello)+", "(hello){1,3}", "(hello){2}"} {
		plan := mustBuild(t, pattern, false)
		if plan.IsMatchAll() {
			t.Errorf("%q planned as MatchAll, but the body must appear at least once", pattern)
		}
	}
}

func TestAlternationBecomesAUnion(t *testing.T) {
	plan := mustBuild(t, "hello|world", false)
	if plan.Kind != KindOr {
		t.Fatalf("hello|world planned as %s, want an Or", plan)
	}
	if len(plan.Branches) != 2 {
		t.Errorf("got %d branches, want 2", len(plan.Branches))
	}
}

// TestOneUnindexableBranchAbsorbsTheAlternation is a soundness rule: a file
// matching only the branch we cannot plan would be missing from a candidate
// set built from the others.
func TestOneUnindexableBranchAbsorbsTheAlternation(t *testing.T) {
	for _, pattern := range []string{"hello|.", "hello|ab", "a.|hello"} {
		if plan := mustBuild(t, pattern, false); !plan.IsMatchAll() {
			t.Errorf("%q planned as %s, want MatchAll — one branch cannot be narrowed", pattern, plan)
		}
	}
}

// TestConcatJoinsAdjacentLiterals checks that trigrams spanning the seam
// between two literal children are found — the reason literal runs are
// accumulated rather than planned one child at a time.
func TestConcatJoinsAdjacentLiterals(t *testing.T) {
	// The escape splits this into separate literal nodes in the parse tree,
	// but "a.b" is still three consecutive bytes of the match.
	plan := mustBuild(t, `a\.bcd`, false)
	if plan.Kind != KindAnd {
		t.Fatalf(`a\.bcd planned as %s, want an And`, plan)
	}
	want := map[string]bool{"a.b": false, ".bc": false, "bcd": false}
	for _, q := range plan.Queries {
		if _, ok := want[triBytes(q.Hash)]; ok {
			want[triBytes(q.Hash)] = true
		}
	}
	for tri, found := range want {
		if !found {
			t.Errorf("plan %s is missing the trigram %q", plan, tri)
		}
	}
}

// TestWildcardBreaksTheLiteralRun is the other half: no trigram may span a
// construct whose bytes are unknown.
func TestWildcardBreaksTheLiteralRun(t *testing.T) {
	plan := mustBuild(t, "abc.def", false)
	if plan.Kind != KindAnd {
		t.Fatalf("planned as %s, want an And", plan)
	}
	for _, q := range plan.Queries {
		switch triBytes(q.Hash) {
		case "bcd", "cde":
			t.Errorf("plan %s requires %q, which spans the wildcard and need not be present", plan, triBytes(q.Hash))
		}
	}
}

// TestRepeatedTrigramLosesItsNextByte covers the dedup rule: when one trigram
// arrives from two literal runs with different following bytes, neither may be
// required.
func TestRepeatedTrigramLosesItsNextByte(t *testing.T) {
	plan := mustBuild(t, "mutex.*mutex_lock", false)
	if plan.Kind != KindAnd {
		t.Fatalf("planned as %s, want an And", plan)
	}
	for _, q := range plan.Queries {
		if triBytes(q.Hash) == "tex" && q.HasNext {
			t.Errorf("plan %s constrains %q to be followed by %q, but it appears in two different contexts",
				plan, "tex", string(rune(q.ExpectedNext)))
		}
	}
	// A trigram appearing in only one context keeps its constraint.
	var checked bool
	for _, q := range plan.Queries {
		if triBytes(q.Hash) == "ex_" {
			checked = true
			if !q.HasNext || q.ExpectedNext != 'l' {
				t.Errorf("%q should still require a following 'l'", "ex_")
			}
		}
	}
	if !checked {
		t.Errorf("plan %s never contained %q", plan, "ex_")
	}
}

func TestPlanQueriesAreSortedAndUnique(t *testing.T) {
	plan := mustBuild(t, "abcabcabc", false)
	if plan.Kind != KindAnd {
		t.Fatalf("planned as %s", plan)
	}
	for i := 1; i < len(plan.Queries); i++ {
		if plan.Queries[i].Hash <= plan.Queries[i-1].Hash {
			t.Errorf("queries not strictly ascending at %d: %#x after %#x",
				i, plan.Queries[i].Hash, plan.Queries[i-1].Hash)
		}
	}
}

func TestMultiPatternUnionsThePlans(t *testing.T) {
	plan, err := BuildMulti([]string{"hello", "world"}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Kind != KindOr || len(plan.Branches) != 2 {
		t.Errorf("planned as %s, want a two-branch Or", plan)
	}

	// One unplannable pattern absorbs the rest, for the usual reason.
	plan, err = BuildMulti([]string{"hello", "ab"}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.IsMatchAll() {
		t.Errorf("planned as %s, want MatchAll", plan)
	}
}

func TestFixedStringSkipsRegexSyntax(t *testing.T) {
	// As a regex this is a character class; as a fixed string it is five
	// literal bytes and plans as such.
	plan := BuildLiteral("[abc]", false)
	if plan.Kind != KindAnd {
		t.Fatalf("planned as %s, want an And over the literal bytes", plan)
	}
	found := false
	for _, q := range plan.Queries {
		if triBytes(q.Hash) == "[ab" {
			found = true
		}
	}
	if !found {
		t.Errorf("plan %s does not treat '[' as a literal byte", plan)
	}
}

func TestInvalidPatternIsAnError(t *testing.T) {
	if _, err := Build("(unclosed", false); err == nil {
		t.Error("expected a parse error")
	}
}
