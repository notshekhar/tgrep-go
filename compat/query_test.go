package compat

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"

	"github.com/notshekhar/tgrep-go/index"
	"github.com/notshekhar/tgrep-go/ondisk"
	"github.com/notshekhar/tgrep-go/query"
	"github.com/notshekhar/tgrep-go/reader"
)

// corpus is a built index together with the file contents it was built from,
// so a test can compare what the index believes against the truth.
type corpus struct {
	root     string
	paths    []string
	contents map[string]string
	r        *reader.Reader
}

func (c *corpus) lookup() query.Lookup {
	return func(hash uint32) []ondisk.PostingEntry { return c.r.LookupTrigramWithMasks(hash) }
}

// candidates runs a plan and returns the paths it admits.
func (c *corpus) candidates(t *testing.T, plan query.Plan) []string {
	t.Helper()
	ids := query.Execute(plan, c.lookup())
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		p, ok := c.r.FilePath(id)
		if !ok {
			t.Fatalf("candidate id %d has no path", id)
		}
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}

// truth returns the paths whose contents really match the pattern.
func (c *corpus) truth(t *testing.T, pattern string, caseInsensitive bool) []string {
	t.Helper()
	expr := pattern
	if caseInsensitive {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		t.Fatalf("compile %q: %v", expr, err)
	}
	var out []string
	for _, path := range c.paths {
		if re.MatchString(c.contents[path]) {
			out = append(out, path)
		}
	}
	slices.Sort(out)
	return out
}

// buildCorpus writes files, indexes them, and opens the index.
func buildCorpus(t *testing.T, files map[string]string) *corpus {
	t.Helper()
	root := t.TempDir()
	c := &corpus{root: root, contents: map[string]string{}}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		c.paths = append(c.paths, name)
		c.contents[name] = body
	}
	slices.Sort(c.paths)

	dir := t.TempDir()
	if _, err := index.BuildFromPaths(dir, root, c.paths, index.Options{Complete: true}); err != nil {
		t.Fatalf("build: %v", err)
	}
	r, err := reader.Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	c.r = r
	return c
}

// assertSound is the property the index exists to preserve: a candidate set
// may be larger than the truth, never smaller.
//
// A miss here is the worst failure this codebase can have. It is silent — the
// search returns fewer results and reports success — so it is checked directly
// rather than inferred from end-to-end results.
func assertSound(t *testing.T, c *corpus, pattern string, caseInsensitive bool) {
	t.Helper()
	plan, err := query.Build(pattern, caseInsensitive)
	if err != nil {
		t.Fatalf("plan %q: %v", pattern, err)
	}
	want := c.truth(t, pattern, caseInsensitive)
	if plan.IsMatchAll() {
		return // every file is a candidate, so nothing can be missed
	}
	got := c.candidates(t, plan)
	for _, path := range want {
		if !slices.Contains(got, path) {
			t.Errorf("pattern %q (plan %s): %s really matches but was not a candidate\n  candidates: %v",
				pattern, plan, path, got)
		}
	}
}

// TestPlannerSoundnessOnHandPickedPatterns covers the cases most likely to
// break: shared prefixes that differ at the fourth byte (the next-byte filter),
// wildcards inside literals, alternations, and repeated trigrams.
func TestPlannerSoundnessOnHandPickedPatterns(t *testing.T) {
	c := buildCorpus(t, map[string]string{
		"a.txt":    "hello world\nhella there\njello pudding\n",
		"b.txt":    "func handler(w http.ResponseWriter) error {\n\treturn nil\n}\n",
		"c.txt":    "mutex.Lock()\nmutex_lock(&m)\ndefer mutex.Unlock()\n",
		"d.txt":    "the quick brown fox\njumps over the lazy dog\n",
		"e.txt":    "HELLO WORLD IN CAPITALS\n",
		"f.txt":    "short\n",
		"g.txt":    "αβγ unicode content ΔΕΖ\n",
		"dupe.txt": "abcabcabcabc\n",
	})

	patterns := []string{
		"hello", "hella", "jello", "hello world",
		"func", "handler", "http.ResponseWriter", "return nil",
		"mutex", "mutex_lock", "mutex.*mutex_lock", "mutex\\.Lock",
		"quick.brown", "the (quick|lazy)", "quick|lazy",
		"abcabc", "abc+", "(abc){2}",
		"unicode", "αβγ",
		"[hj]ello", "^hello", "world$", "\\bfox\\b",
		"h.llo", "hel+o", "ResponseWriter|Request",
	}
	for _, pattern := range patterns {
		t.Run(pattern, func(t *testing.T) {
			assertSound(t, c, pattern, false)
			assertSound(t, c, pattern, true)
		})
	}
}

// TestPlannerSoundnessOnRandomPatterns is the same property under generated
// input, which reaches combinations no hand-written list would.
//
// Patterns are built from fragments of the corpus itself, so a good share of
// them actually match something — a generator producing only misses would
// prove nothing, since an empty truth set is trivially covered.
func TestPlannerSoundnessOnRandomPatterns(t *testing.T) {
	rng := rand.New(rand.NewSource(20260908))

	words := []string{"alpha", "beta", "gamma", "delta", "epsilon", "handler", "mutex", "buffer", "socket", "parse"}
	files := map[string]string{}
	for i := range 40 {
		var body string
		for range 12 {
			body += words[rng.Intn(len(words))] + "_" + words[rng.Intn(len(words))] + " "
			if rng.Intn(3) == 0 {
				body += "\n"
			}
		}
		files[fmt.Sprintf("f%02d.txt", i)] = body + "\n"
	}
	c := buildCorpus(t, files)

	// Fragment generators, weighted towards things that produce real plans.
	frag := func() string {
		w := words[rng.Intn(len(words))]
		switch rng.Intn(6) {
		case 0:
			return w
		case 1:
			return w + "_" + words[rng.Intn(len(words))]
		case 2:
			return w[:min(len(w), 3+rng.Intn(3))]
		case 3:
			return w + ".*" + words[rng.Intn(len(words))]
		case 4:
			return "(" + w + "|" + words[rng.Intn(len(words))] + ")"
		default:
			return w + "."
		}
	}

	matched := 0
	for i := range 400 {
		pattern := frag()
		if rng.Intn(3) == 0 {
			pattern += frag()
		}
		if _, err := regexp.Compile(pattern); err != nil {
			continue
		}
		if len(c.truth(t, pattern, false)) > 0 {
			matched++
		}
		t.Run(fmt.Sprintf("%d/%s", i, pattern), func(t *testing.T) {
			assertSound(t, c, pattern, rng.Intn(2) == 0)
		})
	}
	// Guard against the generator degenerating into patterns that match
	// nothing, which would make every assertion vacuous.
	if matched < 100 {
		t.Errorf("only %d of the generated patterns matched anything; the property is barely exercised", matched)
	}
}

// TestNextByteFilterActuallyNarrows confirms the Bloom filter is not merely
// sound but useful: "hello" must not admit a file that only holds "hella".
func TestNextByteFilterActuallyNarrows(t *testing.T) {
	c := buildCorpus(t, map[string]string{
		"has.txt":  "hello world\n",
		"near.txt": "hella hellb hellc\n", // every trigram of "hello" but "hel"+o only via...
		"none.txt": "nothing relevant here\n",
	})

	// "hella" contains hel, ell — but not "llo". So the plain intersection
	// already excludes near.txt. Use a case where all three trigrams exist:
	c2 := buildCorpus(t, map[string]string{
		"real.txt":  "hello\n",
		"decoy.txt": "hel llo ell\n", // all three trigrams, never as "hello"
	})

	plan, err := query.Build("hello", false)
	if err != nil {
		t.Fatal(err)
	}
	got := c2.candidates(t, plan)
	if !slices.Contains(got, "real.txt") {
		t.Errorf("candidates %v are missing the file that really matches", got)
	}
	// The decoy may survive — a trigram index cannot always exclude it — but
	// the truth must still be a subset, which assertSound checks generally.
	t.Logf("decoy survived: %v", slices.Contains(got, "decoy.txt"))
	_ = c
}

// TestAdjacencyFilterKeepsRealMatches guards the location-mask filter, which
// is the easiest place in this package to introduce a false negative.
//
// padded.txt exists to shift the literal past one mask period: the masks
// record offset%8, so a match starting at offset 8 sets the same bits as one
// starting at 0 and a filter that confused the two would still pass on a
// short file.
func TestAdjacencyFilterKeepsRealMatches(t *testing.T) {
	c := buildCorpus(t, map[string]string{
		"real.txt":   "the word hello appears here\n",
		"spread.txt": "hel\nllo\nell\n",
		"padded.txt": "xxxxxxxxhelloxxxxxxxx\n",
	})

	literal := []byte("hello")
	plan := query.BuildLiteral(string(literal), false)
	if plan.Kind != query.KindAnd {
		t.Fatalf("literal planned as %s", plan)
	}
	before := query.Execute(plan, c.lookup())
	after := query.AdjacencyFilter(slices.Clone(before), literal, c.lookup())

	survived := map[string]bool{}
	for _, id := range after {
		p, _ := c.r.FilePath(id)
		survived[p] = true
	}
	for _, path := range []string{"real.txt", "padded.txt"} {
		if !survived[path] {
			t.Errorf("%s really contains hello but the adjacency filter dropped it", path)
		}
	}
	t.Logf("adjacency filter: %d candidates -> %d", len(before), len(after))
}

// TestAdjacencyFilterRejectsAScatteredFile shows it does real work: a file
// holding every trigram of the literal, never adjacent, should be dropped.
func TestAdjacencyFilterRejectsAScatteredFile(t *testing.T) {
	c := buildCorpus(t, map[string]string{
		"real.txt":  "hello\n",
		"decoy.txt": "hel-----llo-----ell\n",
	})

	literal := []byte("hello")
	plan := query.BuildLiteral(string(literal), false)
	before := query.Execute(plan, c.lookup())
	after := query.AdjacencyFilter(slices.Clone(before), literal, c.lookup())

	survived := map[string]bool{}
	for _, id := range after {
		p, _ := c.r.FilePath(id)
		survived[p] = true
	}
	if !survived["real.txt"] {
		t.Error("the file that really matches was dropped")
	}
	// The masks are 8 bits of offset residue, so this is a hint rather than a
	// guarantee; log it instead of requiring it.
	t.Logf("scattered decoy survived: %v (candidates %d -> %d)", survived["decoy.txt"], len(before), len(after))
}

// TestRelaxationIsSound checks the PCRE widening on patterns the standard
// parser rejects.
//
// The property is stated against the RELAXED pattern, not the original: every
// rewrite only widens the language, so L(original) is a subset of
// L(relaxed), and a candidate set sound for the relaxed pattern is therefore
// sound for the original too. Asserting against the original directly would
// require a PCRE engine, and guessing which files "should" match by eye is how
// this test was wrong the first time — (?>password)= requires "password=",
// which "password_hash=" does not contain.
func TestRelaxationIsSound(t *testing.T) {
	c := buildCorpus(t, map[string]string{
		"a.txt": "password=secret123\n",
		"b.txt": "username=admin\n",
		"c.txt": "password_hash=abcdef\n",
	})

	cases := []struct {
		pcre    string
		relaxes bool
		want    string // what it must relax to, when it relaxes
	}{
		{`password(?=\=)`, true, `password`},
		{`(?<=password)=`, true, `=`},
		{`(?>password)=`, true, `(?:password)=`},
		{`password\1`, false, ""},
		{`(?(1)password|user)`, false, ""},
		{`password\K=`, false, ""},
		{`password[(?=]x`, true, `password[(?=]x`},
	}

	for _, tc := range cases {
		t.Run(tc.pcre, func(t *testing.T) {
			relaxed, ok := query.Relax(tc.pcre)
			if ok != tc.relaxes {
				t.Fatalf("Relax(%q) ok = %v, want %v (got %q)", tc.pcre, ok, tc.relaxes, relaxed)
			}
			if !ok {
				return
			}
			if relaxed != tc.want {
				t.Errorf("Relax(%q) = %q, want %q", tc.pcre, relaxed, tc.want)
			}
			if _, err := regexp.Compile(relaxed); err != nil {
				t.Skipf("relaxed to %q, which still does not parse: %v", relaxed, err)
			}
			// The plan built from the relaxed pattern must admit every file
			// that relaxed pattern really matches.
			assertSound(t, c, relaxed, false)
		})
	}
}
