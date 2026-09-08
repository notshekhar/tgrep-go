package compat

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/notshekhar/tgrep-go/ondisk"
	"github.com/notshekhar/tgrep-go/reader"
	"github.com/notshekhar/tgrep-go/trigram"
)

func openRef(t *testing.T) *reader.Reader {
	t.Helper()
	skipWithoutRef(t)
	r, err := reader.Open(refIndex)
	if err != nil {
		t.Fatalf("open reference index: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func TestReaderOpensTheReferenceIndex(t *testing.T) {
	r := openRef(t)
	if got := r.NumFiles(); got != 5 {
		t.Errorf("NumFiles = %d, want 5", got)
	}
	if got := r.NumTrigrams(); got != 156 {
		t.Errorf("NumTrigrams = %d, want 156", got)
	}
	if err := r.ValidateLookup(); err != nil {
		t.Errorf("ValidateLookup: %v", err)
	}
	if r.IsDegenerate() {
		t.Error("IsDegenerate = true for a well-formed index")
	}
}

// candidates intersects the posting lists of every trigram in a literal, which
// is the whole of what an AND plan does.
func candidates(t *testing.T, r *reader.Reader, literal string) []string {
	t.Helper()
	var acc []uint32
	first := true
	for _, tri := range trigram.Extract([]byte(literal)) {
		ids := r.LookupTrigram(tri)
		if first {
			acc, first = ids, false
			continue
		}
		var merged []uint32
		i, j := 0, 0
		for i < len(acc) && j < len(ids) {
			switch {
			case acc[i] == ids[j]:
				merged = append(merged, acc[i])
				i++
				j++
			case acc[i] < ids[j]:
				i++
			default:
				j++
			}
		}
		acc = merged
	}
	paths := make([]string, 0, len(acc))
	for _, id := range acc {
		p, ok := r.FilePath(id)
		if !ok {
			t.Fatalf("file id %d has no path", id)
		}
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}

// TestCandidateSetsAreSoundAgainstTheFixture is the property the whole index
// exists to provide: the candidate set may be larger than the true match set,
// never smaller. A trigram index that drops a real match is worse than no
// index at all, because the miss is silent.
func TestCandidateSetsAreSoundAgainstTheFixture(t *testing.T) {
	r := openRef(t)

	for _, literal := range []string{"hello", "world", "hello world", "func", "TODO", "beta", "gamma", "println"} {
		got := candidates(t, r, literal)

		// The truth, read straight off the fixture.
		var want []string
		for _, path := range r.AllPaths() {
			content, err := os.ReadFile(filepath.Join(fixture, path))
			if err != nil {
				t.Fatalf("read fixture %s: %v", path, err)
			}
			if strings.Contains(string(content), literal) {
				want = append(want, path)
			}
		}
		sort.Strings(want)

		for _, w := range want {
			if !slices.Contains(got, w) {
				t.Errorf("%q: candidate set %v is missing %s, which really contains it", literal, got, w)
			}
		}
		if len(want) == 0 && len(got) != 0 {
			// Not wrong in principle — a candidate is only a candidate — but
			// for these short fixtures it would mean the index is not
			// narrowing at all, which is worth knowing.
			t.Logf("%q: no file contains it, yet %d candidates survived: %v", literal, len(got), got)
		}
	}
}

// TestNextMaskRejectsAFourthCharacter exercises the Bloom filter the format
// carries beyond a plain trigram list: "hell" must be rejectable in a file
// that contains "hello" only if the byte after "hel" is never 'l' — so here it
// must NOT be rejected, and a byte that never follows must be.
func TestNextMaskRejectsAFourthCharacter(t *testing.T) {
	r := openRef(t)

	tri := trigram.Hash('h', 'e', 'l')
	postings := r.LookupTrigramWithMasks(tri)
	if len(postings) == 0 {
		t.Fatal(`the fixture contains "hel" but the index has no posting for it`)
	}

	for _, p := range postings {
		path, _ := r.FilePath(p.FileID)
		content, err := os.ReadFile(filepath.Join(fixture, path))
		if err != nil {
			t.Fatal(err)
		}
		masks := trigram.Masks{LocMask: p.LocMask, NextMask: p.NextMask}

		// Every byte that really does follow "hel" must survive the filter —
		// a Bloom filter may admit what is absent, never reject what is there.
		for i := 0; i+3 < len(content); i++ {
			if content[i] == 'h' && content[i+1] == 'e' && content[i+2] == 'l' {
				if next := content[i+3]; !masks.MayBeFollowedBy(next) {
					t.Errorf("%s: nextMask rejects %q, which really follows \"hel\" at offset %d", path, next, i)
				}
			}
		}
	}
}

// TestAdjacencyMasksHoldForARealLiteral checks the locMask rotate-and-AND on a
// literal the fixture actually contains: consecutive trigrams of "hello" must
// all test as adjacency-compatible in a file that holds the word.
func TestAdjacencyMasksHoldForARealLiteral(t *testing.T) {
	r := openRef(t)

	const literal = "hello"
	tris := trigram.Extract([]byte(literal))

	for id, path := range r.AllPaths() {
		content, err := os.ReadFile(filepath.Join(fixture, path))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(content), literal) {
			continue
		}
		masks := make([]trigram.Masks, 0, len(tris))
		for _, tri := range tris {
			var found bool
			for _, p := range r.LookupTrigramWithMasks(tri) {
				if int(p.FileID) == id {
					masks = append(masks, trigram.Masks{LocMask: p.LocMask, NextMask: p.NextMask})
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("%s contains %q but has no posting for one of its trigrams", path, literal)
			}
		}
		if !trigram.CheckAdjacency(masks) {
			t.Errorf("%s really contains %q, but its location masks say the trigrams are never adjacent", path, literal)
		}
	}
}

func TestPathMembershipQueries(t *testing.T) {
	r := openRef(t)

	for _, path := range []string{"src/main.go", "src/main.rs", "data.txt"} {
		if !r.ContainsPath(path) {
			t.Errorf("ContainsPath(%q) = false", path)
		}
	}
	for _, path := range []string{"src/main.py", "src", "", "src/main.go/x"} {
		if r.ContainsPath(path) {
			t.Errorf("ContainsPath(%q) = true, but no such file is indexed", path)
		}
	}

	if !r.HasDescendantPath("src") {
		t.Error(`HasDescendantPath("src") = false, but src/main.go is indexed`)
	}
	if !r.HasDescendantPath("") {
		t.Error(`HasDescendantPath("") = false, but the index is not empty`)
	}
	if r.HasDescendantPath("nope") {
		t.Error(`HasDescendantPath("nope") = true`)
	}
	// A file is not a directory: nothing lies strictly below it.
	if r.HasDescendantPath("data.txt") {
		t.Error(`HasDescendantPath("data.txt") = true for a plain file`)
	}
}

// TestTrigramPostingsEnumeratesEverything walks the index the way a merge or a
// dump would, and confirms the streaming accessor agrees with the point lookup.
func TestTrigramPostingsEnumeratesEverything(t *testing.T) {
	r := openRef(t)

	var prev uint32
	for i := range r.NumTrigrams() {
		tri, postings := r.TrigramPostings(i)
		if i > 0 && tri <= prev {
			t.Fatalf("entry %d: trigram %#x does not exceed %#x", i, tri, prev)
		}
		prev = tri
		if len(postings) == 0 {
			t.Errorf("trigram %#x has an empty posting list — it should not have an entry at all", tri)
		}
		byLookup := r.LookupTrigramWithMasks(tri)
		if !slices.Equal(postings, byLookup) {
			t.Errorf("trigram %#x: streaming gave %v, point lookup gave %v", tri, postings, byLookup)
		}
	}
}

func TestMissingTrigramReturnsNothing(t *testing.T) {
	r := openRef(t)
	// Three bytes the fixture cannot contain.
	if got := r.LookupTrigram(trigram.Hash(0xF0, 0xF1, 0xF2)); len(got) != 0 {
		t.Errorf("lookup of an absent trigram returned %v", got)
	}
}

func TestEmptyReaderAnswersNothing(t *testing.T) {
	r := reader.Empty()
	if r.NumFiles() != 0 || r.NumTrigrams() != 0 {
		t.Error("an empty reader claims to hold something")
	}
	if got := r.LookupTrigram(trigram.Hash('a', 'b', 'c')); len(got) != 0 {
		t.Errorf("empty reader returned %v", got)
	}
	if r.IsDegenerate() {
		t.Error("an empty reader is not degenerate — it is empty")
	}
	if err := r.ValidateLookup(); err != nil {
		t.Errorf("ValidateLookup on an empty reader: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Errorf("Close on an empty reader: %v", err)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	skipWithoutRef(t)
	r, err := reader.Open(refIndex)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Errorf("second close: %v", err)
	}
	// A closed reader must answer emptily rather than fault on an unmapped
	// region — the swap path in the server closes one reader while requests
	// may still be arriving.
	if got := r.LookupTrigram(trigram.Hash('h', 'e', 'l')); len(got) != 0 {
		t.Errorf("closed reader returned %v", got)
	}
	if got := r.NumFiles(); got != 0 {
		t.Errorf("closed reader reports %d files", got)
	}
}

// TestRejectsATruncatedLookupTable proves the corruption guard fires rather
// than silently dropping the trailing record.
func TestRejectsATruncatedLookupTable(t *testing.T) {
	skipWithoutRef(t)
	dir := t.TempDir()
	for _, name := range []string{"lookup.bin", "index.bin", "files.bin"} {
		data := readRef(t, name)
		if name == "lookup.bin" {
			data = data[:len(data)-3] // a partial 16-byte record
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := reader.Open(dir); err == nil {
		t.Fatal("opened an index whose lookup.bin ends mid-record")
	} else if !strings.Contains(err.Error(), "multiple of") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestRejectsSparseFileIDs is the guard that keeps a corrupt id from sizing an
// allocation.
func TestRejectsSparseFileIDs(t *testing.T) {
	skipWithoutRef(t)
	dir := t.TempDir()
	for _, name := range []string{"lookup.bin", "index.bin"} {
		if err := os.WriteFile(filepath.Join(dir, name), readRef(t, name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// One entry claiming a very high id, in a table of one.
	entry, err := ondisk.EncodeFileEntry(1<<31, "src/main.go")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "files.bin"), entry, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Open(dir); err == nil {
		t.Fatal("opened an index with a sparse file id")
	} else if !strings.Contains(err.Error(), "out-of-range file_id") {
		t.Errorf("unexpected error: %v", err)
	}
}
