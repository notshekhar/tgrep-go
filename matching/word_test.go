package matching

import (
	"slices"
	"testing"
)

// Ported from ripgrep-compatibility issue #184 upstream: -w means "no word
// character on either side", which plain \b gets wrong beside a non-word edge.
func TestWordMatchesBesideNonWordEdges(t *testing.T) {
	content := []byte("a- b\nx -a y\nba- c\nfoo-bar\nfoo bar\n")
	for _, tc := range []struct {
		pattern string
		lines   []int
	}{
		{"a-", []int{0}},       // "ba-" is preceded by a word character
		{"-a", []int{1}},       // "-bar" is followed by one
		{"foo", []int{3, 4}},   // ordinary word edges are unchanged
		{"o-b", nil},           // inside "foo-bar" on both sides
		{"a-|-a", []int{0, 1}}, // agreeing alternatives keep their edge
		{"[-+]a", []int{1}},
	} {
		m, err := Build([]string{tc.pattern}, Config{WordBoundary: true})
		if err != nil {
			t.Fatal(err)
		}
		var got []int
		for _, span := range m.FindSpans(content, true) {
			line := 0
			for _, c := range content[:span.Start] {
				if c == '\n' {
					line++
				}
			}
			if !slices.Contains(got, line) {
				got = append(got, line)
			}
		}
		if !slices.Equal(got, tc.lines) {
			t.Errorf("-w %q matched lines %v, want %v (%s)", tc.pattern, got, tc.lines, m)
		}
	}
}
