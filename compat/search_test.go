package compat

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/notshekhar/tgrep-go/index"
	"github.com/notshekhar/tgrep-go/matching"
	"github.com/notshekhar/tgrep-go/reader"
	"github.com/notshekhar/tgrep-go/search"
	"github.com/notshekhar/tgrep-go/walker"
)

// hit is one match, in the form --vimgrep reports: a path, a 1-based line, a
// 1-based byte column, and the line's text.
type hit struct {
	Path string
	Line int
	Col  int
	Text string
}

func (h hit) String() string { return fmt.Sprintf("%s:%d:%d:%s", h.Path, h.Line, h.Col, h.Text) }

// oracleSearch runs the reference in --vimgrep mode, which emits exactly one
// row per match — the only output format of the two that is unambiguous enough
// to compare mechanically.
func oracleSearch(t *testing.T, root, pattern string, extraArgs ...string) []hit {
	t.Helper()
	bin := referenceBinary(t)

	args := append([]string{"--vimgrep", "--index-path", t.TempDir()}, extraArgs...)
	args = append(args, pattern, root)
	out, err := exec.Command(bin, args...).CombinedOutput()
	// Exit status 1 just means no matches, which is a legitimate result.
	if err != nil && len(out) > 0 && strings.Contains(string(out), "error") {
		t.Fatalf("reference search failed: %v\n%s", err, out)
	}

	var hits []hit
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || strings.HasPrefix(line, "warning:") || strings.HasPrefix(line, "note:") {
			continue
		}
		// path:line:col:text — the text may itself contain colons, so only
		// the first three separators are structural.
		parts := strings.SplitN(line, ":", 4)
		if len(parts) < 4 {
			continue
		}
		lineNo, err1 := strconv.Atoi(parts[1])
		col, err2 := strconv.Atoi(parts[2])
		if err1 != nil || err2 != nil {
			continue
		}
		rel, err := filepath.Rel(root, parts[0])
		if err != nil {
			rel = parts[0]
		}
		hits = append(hits, hit{Path: filepath.ToSlash(rel), Line: lineNo, Col: col, Text: parts[3]})
	}
	sortHits(hits)
	return hits
}

func sortHits(hits []hit) {
	slices.SortFunc(hits, func(a, b hit) int {
		if a.Path != b.Path {
			return strings.Compare(a.Path, b.Path)
		}
		if a.Line != b.Line {
			return a.Line - b.Line
		}
		return a.Col - b.Col
	})
}

// indexed builds a Go index over root and returns a searcher for it.
func indexed(t *testing.T, root string) *search.Searcher {
	t.Helper()
	res, err := walker.Walk(root, walker.Options{})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	dir := t.TempDir()
	if _, err := index.BuildFromPaths(dir, root, res.Files, index.Options{Complete: true}); err != nil {
		t.Fatalf("build: %v", err)
	}
	r, err := reader.Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	return search.New(root, r)
}

// ourSearch runs a search and flattens it into the same shape as the oracle.
func ourSearch(t *testing.T, s *search.Searcher, opts search.Options) []hit {
	t.Helper()
	// One row per match is what --vimgrep means, so every span has to be
	// located rather than just the first on each line.
	opts.Match.AllSpans = true

	res, err := s.Run(opts)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	var hits []hit
	for _, fr := range res.Files {
		if fr.Err != nil {
			t.Errorf("%s: %v", fr.Path, fr.Err)
			continue
		}
		for _, line := range fr.Lines {
			if line.Kind != matching.KindMatch {
				continue
			}
			cols := line.Columns()
			if len(cols) == 0 {
				// An inverted match reports a whole line with nothing located
				// inside it. The reference still emits one row for it, at
				// column 1, so it needs one here too — otherwise an inverted
				// search compares as empty and the case tests nothing.
				cols = []int{1}
			}
			for _, col := range cols {
				hits = append(hits, hit{Path: fr.Path, Line: line.Number, Col: col, Text: string(line.Text)})
			}
		}
	}
	sortHits(hits)
	return hits
}

// compareHits reports the differences in both directions.
func compareHits(t *testing.T, label string, got, want []hit) {
	t.Helper()
	inWant := map[string]bool{}
	for _, h := range want {
		inWant[h.String()] = true
	}
	inGot := map[string]bool{}
	for _, h := range got {
		inGot[h.String()] = true
	}

	var missing, extra []string
	for _, h := range want {
		if !inGot[h.String()] {
			missing = append(missing, h.String())
		}
	}
	for _, h := range got {
		if !inWant[h.String()] {
			extra = append(extra, h.String())
		}
	}
	if len(missing) > 0 {
		t.Errorf("%s: the reference found these and we did not:\n  %s", label, strings.Join(first(missing, 10), "\n  "))
	}
	if len(extra) > 0 {
		t.Errorf("%s: we found these and the reference did not:\n  %s", label, strings.Join(first(extra, 10), "\n  "))
	}
}

// searchCorpus is a tree with the shapes a matcher tends to get wrong:
// repeated matches on one line, matches at line edges, CRLF, unicode, a very
// long line, and a file with no trailing newline.
func searchCorpus(t *testing.T) string {
	t.Helper()
	return tree(t, map[string]string{
		"a.go":        "package main\n\nfunc handler() {\n\tlog.Print(\"handler start\")\n}\n",
		"b.go":        "func handler(w Writer) { handler2(w); handler3(w) }\n",
		"c.txt":       "handler at the very start\nand handler at the end: handler\n",
		"crlf.txt":    "handler one\r\nhandler two\r\nplain\r\n",
		"unicode.txt": "héllo wörld\nhandler ünicode\nαβγ handler δεζ\n",
		"long.txt":    "prefix " + strings.Repeat("x", 4000) + " handler " + strings.Repeat("y", 4000) + "\n",
		"noeol.txt":   "handler with no trailing newline",
		"empty.txt":   "",
		"nested/d.rs": "fn handler() -> Result<()> {\n    Ok(())\n}\n",
		"case.txt":    "Handler HANDLER handler HaNdLeR\n",
	})
}

// TestSearchResultsMatchTheReference is the end-to-end property: the same
// pattern over the same tree must produce exactly the same matches, down to
// the byte column.
func TestSearchResultsMatchTheReference(t *testing.T) {
	root := searchCorpus(t)
	s := indexed(t, root)

	cases := []struct {
		name    string
		pattern string
		opts    search.Options
		refArgs []string
	}{
		{name: "plain literal", pattern: "handler"},
		{
			name: "case insensitive", pattern: "handler",
			opts:    search.Options{Matcher: matching.Config{CaseInsensitive: true}},
			refArgs: []string{"-i"},
		},
		{
			name: "fixed string", pattern: "handler(",
			opts:    search.Options{Matcher: matching.Config{FixedString: true}},
			refArgs: []string{"-F"},
		},
		{
			name: "word boundary", pattern: "handler",
			opts:    search.Options{Matcher: matching.Config{WordBoundary: true}},
			refArgs: []string{"-w"},
		},
		{name: "regex with alternation", pattern: "handler[0-9]|Ok"},
		{name: "anchored at line start", pattern: "^handler"},
		{name: "anchored at line end", pattern: "handler$"},
		{name: "dot wildcard", pattern: "h.ndler"},
		{name: "repetition", pattern: "hand+ler"},
		{name: "unicode literal", pattern: "wörld"},
		{name: "no matches anywhere", pattern: "zzzznotpresent"},
		{name: "matches everything short", pattern: "e"},
		{
			name: "invert match", pattern: "handler",
			opts:    search.Options{Match: matching.Options{InvertMatch: true}},
			refArgs: []string{"-v"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := tc.opts
			opts.Patterns = []string{tc.pattern}

			got := ourSearch(t, s, opts)
			want := oracleSearch(t, root, tc.pattern, tc.refArgs...)
			compareHits(t, tc.pattern, got, want)
			if !t.Failed() && len(want) > 0 {
				t.Logf("%d matches agreed", len(want))
			}
		})
	}
}

// TestSearchOnRealRepositories runs the same comparison over whole checkouts,
// where the corpus is code nobody wrote to make a test pass.
func TestSearchOnRealRepositories(t *testing.T) {
	spec := os.Getenv("TGREP_REAL_REPOS")
	if spec == "" {
		t.Skip("set TGREP_REAL_REPOS to a colon-separated list of checkouts")
	}
	patterns := []string{"func ", "error", "TODO", "^import", "return nil$", "[A-Z][a-z]+Error"}

	for _, repo := range strings.Split(spec, ":") {
		if repo = strings.TrimSpace(repo); repo == "" {
			continue
		}
		abs, err := filepath.Abs(repo)
		if err != nil {
			t.Fatal(err)
		}
		if resolved, err := filepath.EvalSymlinks(abs); err == nil {
			abs = resolved
		}
		if _, err := os.Stat(abs); err != nil {
			continue
		}

		t.Run(filepath.Base(abs), func(t *testing.T) {
			s := indexed(t, abs)
			total := 0
			for _, pattern := range patterns {
				t.Run(pattern, func(t *testing.T) {
					got := ourSearch(t, s, search.Options{Patterns: []string{pattern}})
					want := oracleSearch(t, abs, pattern)
					compareHits(t, pattern, got, want)
					total += len(want)
					t.Logf("%d matches", len(want))
				})
			}
			// Two empty result sets compare equal, so a repository where
			// nothing matched would pass while testing nothing at all.
			if total == 0 {
				t.Errorf("no pattern matched anything in %s; the comparison is vacuous", abs)
			}
		})
	}
}

// TestContextLinesMatchTheReference checks the context window separately,
// since --vimgrep suppresses context and the comparison has to be on the
// default format's line numbers instead.
func TestContextLinesMatchTheReference(t *testing.T) {
	root := tree(t, map[string]string{
		"ctx.txt": "one\ntwo\nTARGET\nfour\nfive\nsix\nTARGET\neight\n",
	})
	s := indexed(t, root)

	for _, tc := range []struct{ before, after int }{{0, 0}, {1, 0}, {0, 1}, {1, 1}, {2, 2}, {5, 5}} {
		t.Run(fmt.Sprintf("B%d_A%d", tc.before, tc.after), func(t *testing.T) {
			res, err := s.Run(search.Options{
				Patterns: []string{"TARGET"},
				Match:    matching.Options{BeforeContext: tc.before, AfterContext: tc.after},
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Files) != 1 {
				t.Fatalf("expected one file, got %d", len(res.Files))
			}

			var lines []int
			for _, l := range res.Files[0].Lines {
				lines = append(lines, l.Number)
			}

			// The two matches are on lines 3 and 7 of an 8-line file; their
			// windows overlap once the context reaches 2, and the overlap must
			// appear once, not twice.
			want := expectedWindow([]int{3, 7}, 8, tc.before, tc.after)
			if !slices.Equal(lines, want) {
				t.Errorf("context B%d A%d: got lines %v, want %v", tc.before, tc.after, lines, want)
			}
		})
	}
}

// expectedWindow is the independently computed answer for the context test, so
// the assertion does not just re-run the implementation it is checking.
func expectedWindow(matches []int, total, before, after int) []int {
	show := map[int]bool{}
	for _, m := range matches {
		for i := m - before; i <= m+after; i++ {
			if i >= 1 && i <= total {
				show[i] = true
			}
		}
	}
	var out []int
	for i := 1; i <= total; i++ {
		if show[i] {
			out = append(out, i)
		}
	}
	return out
}

// TestUnsupportedPatternsAreRejectedNotMisread covers the one place Go's
// engine cannot follow the reference: RE2 has no backreferences or lookaround.
// The requirement is that such a pattern is refused with a clear message, not
// silently compiled into something that means something else.
func TestUnsupportedPatternsAreRejectedNotMisread(t *testing.T) {
	root := tree(t, map[string]string{"a.txt": "abcabc\n"})
	s := indexed(t, root)

	for _, pattern := range []string{`(abc)\1`, `foo(?=bar)`, `(?<=foo)bar`} {
		_, err := s.Run(search.Options{Patterns: []string{pattern}})
		if err == nil {
			t.Errorf("%q was accepted, but RE2 cannot express it", pattern)
			continue
		}
		if !strings.Contains(err.Error(), "not supported") {
			t.Errorf("%q rejected with an unhelpful error: %v", pattern, err)
		}
	}
}

// TestMaxCountLimitsMatches checks the block-counting rule: several matches on
// one line spend one unit between them.
func TestMaxCountLimitsMatches(t *testing.T) {
	root := tree(t, map[string]string{
		"m.txt": "hit hit hit\nplain\nhit\nplain\nhit\n",
	})
	s := indexed(t, root)

	for _, limit := range []int{1, 2, 3} {
		res, err := s.Run(search.Options{
			Patterns: []string{"hit"},
			Match:    matching.Options{MaxCount: limit, AllSpans: true},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Files) == 0 {
			t.Fatalf("limit %d: no results", limit)
		}
		if got := res.Files[0].MatchedLines; got > limit {
			t.Errorf("limit %d: reported %d matching lines", limit, got)
		}
	}
}

// TestAdjacentMatchesAreNotMerged covers a defect found by the differential
// harness on a real checkout, not by any hand-written case.
//
// Searching "context|Context" over "ContextContext" is two matches, at columns
// 1 and 8. The first ends exactly where the second begins, and a span merge
// written with <= instead of < collapses them into one — reporting a single
// match where ripgrep and the reference both report two, and losing a column
// an editor needs to jump to.
//
// It only shows up when two matches ABUT, which is rare enough that a fixture
// will not contain it by accident: this was found in TypeScript's own compiler
// output, in the identifier "shouldEnterSuppressNewDiagnosticsContextContext".
func TestAdjacentMatchesAreNotMerged(t *testing.T) {
	root := tree(t, map[string]string{
		"a.ts": "let shouldEnterSuppressNewDiagnosticsContextContext = 1;\n" +
			"see scriptrunincontextcontextifiedobject-options here\n" +
			"plain context and Context on one line\n" +
			"contextcontextcontext\n",
	})
	s := indexed(t, root)

	got := ourSearch(t, s, search.Options{Patterns: []string{"context|Context"}})
	want := oracleSearch(t, root, "context|Context")
	compareHits(t, "context|Context", got, want)

	// Stated directly as well, so the intent survives even if the reference
	// binary is unavailable and the comparison above skips.
	byLine := map[int]int{}
	for _, h := range got {
		byLine[h.Line]++
	}
	for line, wantCount := range map[int]int{1: 2, 2: 2, 3: 2, 4: 3} {
		if byLine[line] != wantCount {
			t.Errorf("line %d has %d matches, want %d — abutting matches were merged", line, byLine[line], wantCount)
		}
	}
}

// TestSmartCaseMatchesTheReference locks in a case that works only by a subtle
// coincidence, and would fail silently if either half of it changed.
//
// Smart case turns insensitivity on only when no pattern contains an uppercase
// letter — and for such a pattern the planner's ASCII lowercasing is a no-op,
// so the candidate plan is the same whether or not smart case is threaded into
// it. The index's merged-case extraction then supplies the uppercase variants.
// If the planner ever stopped lowercasing, or the index stopped merging cases,
// a smart-case search would quietly return fewer results.
func TestSmartCaseMatchesTheReference(t *testing.T) {
	root := tree(t, map[string]string{
		"a.txt": "FOO uppercase\nfoo lowercase\nFoo mixed\n",
		"b.txt": "BAR and bar and Bar\n",
	})
	s := indexed(t, root)

	cases := []struct {
		pattern string
		cfg     matching.Config
		refArgs []string
	}{
		{"foo", matching.Config{SmartCase: true}, []string{"-S"}},
		{"Foo", matching.Config{SmartCase: true}, []string{"-S"}},
		{"FOO", matching.Config{SmartCase: true}, []string{"-S"}},
		{"bar", matching.Config{SmartCase: true}, []string{"-S"}},
		{"Bar", matching.Config{SmartCase: true}, []string{"-S"}},
	}
	for _, tc := range cases {
		t.Run(tc.pattern, func(t *testing.T) {
			got := ourSearch(t, s, search.Options{Patterns: []string{tc.pattern}, Matcher: tc.cfg})
			want := oracleSearch(t, root, tc.pattern, tc.refArgs...)
			compareHits(t, tc.pattern+" -S", got, want)
			t.Logf("%d matches", len(want))
		})
	}
}

// TestIndexedSearchKeepsUnicodeFoldedMatches drives the whole indexed path —
// planner, executor and matcher — over files that match only through Unicode
// case folding, which the index's ASCII lowercasing cannot see. Each search
// must find every file a full scan finds.
func TestIndexedSearchKeepsUnicodeFoldedMatches(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"ascii.txt":  "SHELLSHOCK\n",
		"long-s.txt": "ſhellſhocK\n",
		"lower.txt":  "café au lait\n",
		"upper.txt":  "CAFÉ AU LAIT\n",
		"mixed.txt":  "ABC\n",
		"decoy.txt":  "nothing to see\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s := indexed(t, root)

	for _, tc := range []struct {
		name    string
		pattern string
		cfg     matching.Config
		want    []string
	}{
		{"smart case", "shellshock", matching.Config{SmartCase: true}, []string{"ascii.txt", "long-s.txt"}},
		{"-i", "shellshock", matching.Config{CaseInsensitive: true}, []string{"ascii.txt", "long-s.txt"}},
		{"-F -i", "café", matching.Config{FixedString: true, CaseInsensitive: true}, []string{"lower.txt", "upper.txt"}},
		{"-F smart case", "café", matching.Config{FixedString: true, SmartCase: true}, []string{"lower.txt", "upper.txt"}},
		{"scoped flag beside a sensitive letter", "A(?i:bc)", matching.Config{}, []string{"mixed.txt"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := s.Run(search.Options{Patterns: []string{tc.pattern}, Matcher: tc.cfg})
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, fr := range res.Files {
				got = append(got, fr.Path)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("%q found %v, want %v (scanned everything: %v)", tc.pattern, got, tc.want, res.Stats.ScannedEverything)
			}
			if res.Stats.ScannedEverything {
				t.Errorf("%q should still narrow through the index", tc.pattern)
			}
		})
	}
}

// TestLateNulMakesAFileBinaryForSearch pins ripgrep's rule against the index's
// cheaper one: the index only looks for a NUL in the first 8KB, but a search
// that reaches a file by traversal skips it for a NUL anywhere.
func TestLateNulMakesAFileBinaryForSearch(t *testing.T) {
	root := t.TempDir()
	late := "needle\n" + strings.Repeat("x", 12_000) + "\x00\n"
	if err := os.WriteFile(filepath.Join(root, "late.ts"), []byte(late), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "text.ts"), []byte("needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := indexed(t, root)
	for _, binary := range []bool{false, true} {
		res, err := s.Run(search.Options{Patterns: []string{"needle"}, SearchBinary: binary})
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, fr := range res.Files {
			got = append(got, fr.Path)
		}
		want := []string{"text.ts"}
		if binary {
			want = []string{"late.ts", "text.ts"}
		}
		if !slices.Equal(got, want) {
			t.Errorf("SearchBinary=%v found %v, want %v", binary, got, want)
		}
	}
}
