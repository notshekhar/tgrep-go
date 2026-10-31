package matching

import "testing"

// Ported from ripgrep-compatibility issue #183 upstream: smart case looks at
// literals, never at uppercase letters that are regex syntax.
func TestSmartCaseCountsLiteralsNotSyntax(t *testing.T) {
	for _, tc := range []struct {
		pattern string
		folds   bool
	}{
		{"abc", true},
		{"ABC", false},
		{`a\Sc`, true},
		{`a\Wc`, true},
		{`a\Dc`, true},
		{`a\pLc`, true},
		{`\w+`, false},
		{`\p{Lu}`, false},
		{"", false},
		{"^$", false},
		{"[[:upper:]]", false},
		{"[a-z]", true},
		{"[a-zA]", false},
		{"[A-Z]", false},
		{`\x41`, false},
		{`\x61`, true},
		{`\x{00C9}`, false},
		{"é", true},
		{"É", false},
		{"(?P<UPPER>abc)", true},
		{"(?<UPPER>abc)", true},
		{"(?U)abc", true},
		{`\QABC\E`, false},
		{`\Qabc\E`, true},
		{`\101`, false},
	} {
		if got := smartCaseFolds([]string{tc.pattern}, false); got != tc.folds {
			t.Errorf("smartCaseFolds(%q) = %v, want %v", tc.pattern, got, tc.folds)
		}
	}
}

func TestSmartCaseConsidersEveryPattern(t *testing.T) {
	if !smartCaseFolds([]string{"abc", `\S`}, false) {
		t.Error(`"abc" and \S must fold: \S is syntax`)
	}
	if smartCaseFolds([]string{"abc", "XYZ"}, false) {
		t.Error("an uppercase literal in any pattern must keep the search case-sensitive")
	}
	if smartCaseFolds([]string{`a\Sc`}, true) {
		t.Error(`as a fixed string, a\Sc holds a literal uppercase S`)
	}
	if !smartCaseFolds([]string{`a\sc`}, true) {
		t.Error(`as a fixed string, a\sc is all lowercase`)
	}
}
