package compat

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/notshekhar/tgrep-go/index"
	"github.com/notshekhar/tgrep-go/ondisk"
	"github.com/notshekhar/tgrep-go/reader"
)

// buildLikeReference builds a Go index over the fixture using the reference's
// own file order.
//
// Taking the order from the reference rather than from a walk is deliberate:
// it isolates the builder from the walker. File ids are assigned by position,
// so a walk-order difference would show up here as a total mismatch and tell
// us nothing about whether the posting encoding is right. The walker gets its
// own parity test.
func buildLikeReference(t *testing.T, opts index.Options) (dir string, paths []string) {
	t.Helper()
	skipWithoutRef(t)
	paths = filesTable(t)

	dir = t.TempDir()
	if _, err := index.BuildFromPaths(dir, fixture, paths, opts); err != nil {
		t.Fatalf("build: %v", err)
	}
	return dir, paths
}

// TestBuiltIndexIsByteIdenticalToTheReference is the point of the whole
// byte-compatible format decision. Every deterministic file the builder writes
// must match the Rust implementation's output exactly.
func TestBuiltIndexIsByteIdenticalToTheReference(t *testing.T) {
	dir, _ := buildLikeReference(t, index.Options{Complete: true})

	for _, name := range []string{"lookup.bin", "index.bin", "files.bin", "files-extra.bin"} {
		ours, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("read our %s: %v", name, err)
			continue
		}
		want := readRef(t, name)
		if !bytes.Equal(ours, want) {
			t.Errorf("%s differs from the reference (%d bytes vs %d)\n%s",
				name, len(ours), len(want), firstDifference(ours, want))
		}
	}
}

// firstDifference locates the first differing byte, which is far more useful
// than a diff of two binary blobs.
func firstDifference(got, want []byte) string {
	n := min(len(got), len(want))
	for i := range n {
		if got[i] != want[i] {
			lo := max(i-8, 0)
			hi := min(i+8, n)
			return fmt.Sprintf("  first difference at byte %d: got %#x, want %#x\n  ours [%d:%d] = % x\n  ref  [%d:%d] = % x",
				i, got[i], want[i], lo, hi, got[lo:hi], lo, hi, want[lo:hi])
		}
	}
	return fmt.Sprintf("  identical for the first %d bytes, then lengths diverge", n)
}

// TestBuiltIndexIsReadableByOurReader closes the loop: what we wrote, we can
// read, and it answers the same queries as the reference index does.
func TestBuiltIndexIsReadableByOurReader(t *testing.T) {
	dir, paths := buildLikeReference(t, index.Options{Complete: true})

	ours, err := reader.Open(dir)
	if err != nil {
		t.Fatalf("open our index: %v", err)
	}
	defer ours.Close()

	ref := openRef(t)

	if ours.NumFiles() != ref.NumFiles() {
		t.Errorf("NumFiles: ours %d, reference %d", ours.NumFiles(), ref.NumFiles())
	}
	if ours.NumTrigrams() != ref.NumTrigrams() {
		t.Errorf("NumTrigrams: ours %d, reference %d", ours.NumTrigrams(), ref.NumTrigrams())
	}
	if err := ours.ValidateLookup(); err != nil {
		t.Errorf("our index fails its own validation: %v", err)
	}

	// Every trigram in the reference must resolve identically in ours,
	// masks included.
	for i := range ref.NumTrigrams() {
		tri, refPostings := ref.TrigramPostings(i)
		ourPostings := ours.LookupTrigramWithMasks(tri)
		if len(ourPostings) != len(refPostings) {
			t.Errorf("trigram %#x: we hold %d postings, reference holds %d", tri, len(ourPostings), len(refPostings))
			continue
		}
		for j := range refPostings {
			if ourPostings[j] != refPostings[j] {
				t.Errorf("trigram %#x posting %d: ours %+v, reference %+v", tri, j, ourPostings[j], refPostings[j])
			}
		}
	}

	for id, path := range paths {
		got, ok := ours.FilePath(uint32(id))
		if !ok || got != path {
			t.Errorf("file id %d: ours %q, expected %q", id, got, path)
		}
	}
}

// spillCorpus writes a tree big enough to overflow the arena.
//
// The fixture cannot do this job: the arena floors at 1024 postings however
// small the budget, and the fixture yields about 160. Without enough postings
// to cross that floor the "spilling" build silently takes the in-memory path
// and the merge is never executed at all.
//
// Content is varied per file so the files share some trigrams and not others,
// which is what makes the merge do real work instead of concatenating
// identical lists.
func spillCorpus(t *testing.T) (root string, paths []string) {
	t.Helper()
	root = t.TempDir()
	for i := range 200 {
		name := fmt.Sprintf("file%03d.txt", i)
		body := fmt.Sprintf("package p%d\nfunc handler%d(w http.ResponseWriter) {\n\tlog.Printf(%q, %d)\n}\nshared line every file\n",
			i, i, "request "+string(rune('a'+i%26))+"-%d", i)
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, name)
	}
	return root, paths
}

// TestExternalSpillProducesTheSameIndex proves the k-way merge writes exactly
// what a single in-memory sort writes.
//
// This is the path that only appears on repositories too large to keep as test
// data, so it has to be provoked: a tiny budget turns a 200-file corpus into a
// many-segment merge, and the same corpus built with the default budget never
// spills at all. The two must agree byte for byte.
func TestExternalSpillProducesTheSameIndex(t *testing.T) {
	root, paths := spillCorpus(t)

	whole := t.TempDir()
	unspilled, err := index.BuildFromPaths(whole, root, paths, index.Options{Complete: true})
	if err != nil {
		t.Fatalf("build without spilling: %v", err)
	}
	if unspilled.Segments != 0 {
		t.Fatalf("the default budget spilled %d segment(s); this test needs it not to", unspilled.Segments)
	}

	spilled := t.TempDir()
	outcome, err := index.BuildFromPaths(spilled, root, paths, index.Options{
		Complete:    true,
		BufferBytes: 1, // floors to a 1024-posting arena, so this corpus spills
	})
	if err != nil {
		t.Fatalf("build with spilling: %v", err)
	}
	// Without this the test can pass while testing nothing, which is exactly
	// how it read before: the fixture never crossed the arena floor.
	if outcome.Segments < 2 {
		t.Fatalf("merged %d segment(s); the merge needs at least 2 to be exercised", outcome.Segments)
	}
	t.Logf("merged %d spill segments", outcome.Segments)

	for _, name := range []string{"lookup.bin", "index.bin", "files.bin"} {
		got, err := os.ReadFile(filepath.Join(spilled, name))
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(filepath.Join(whole, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: a spilling build differs from an in-memory one\n%s", name, firstDifference(got, want))
		}
	}

	// The spill directory must not survive the build.
	entries, err := os.ReadDir(spilled)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			t.Errorf("build left %s behind in the index directory", e.Name())
		}
	}
}

// TestSpilledIndexIsQueryable reads a merged index back and checks it answers
// the same candidate sets as the unspilled one — the merge could produce a
// well-formed index that is nonetheless missing postings.
func TestSpilledIndexIsQueryable(t *testing.T) {
	root, paths := spillCorpus(t)

	dir := t.TempDir()
	outcome, err := index.BuildFromPaths(dir, root, paths, index.Options{Complete: true, BufferBytes: 1})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if outcome.Segments < 2 {
		t.Fatalf("only %d segment(s) merged", outcome.Segments)
	}

	r, err := reader.Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer r.Close()
	if err := r.ValidateLookup(); err != nil {
		t.Errorf("merged index fails validation: %v", err)
	}
	if r.NumFiles() != len(paths) {
		t.Errorf("NumFiles = %d, want %d", r.NumFiles(), len(paths))
	}

	// "shared line every file" appears in all 200 files, so its trigrams must
	// each carry the full posting list — the strongest single check that the
	// merge lost nothing.
	for _, tri := range []string{"sha", "red", "eve"} {
		h := uint32(tri[0])<<16 | uint32(tri[1])<<8 | uint32(tri[2])
		if got := len(r.LookupTrigram(h)); got != len(paths) {
			t.Errorf("trigram %q appears in %d files after the merge, want %d", tri, got, len(paths))
		}
	}
}

// TestSpillingAtManyBudgets sweeps the arena size across the range where the
// merge fan-in changes, since an off-by-one in the k-way merge can easily be
// invisible at one particular segment count and fatal at another.
//
// Every budget must produce the index the unspilled build produces.
func TestSpillingAtManyBudgets(t *testing.T) {
	root, paths := spillCorpus(t)

	whole := t.TempDir()
	if _, err := index.BuildFromPaths(whole, root, paths, index.Options{Complete: true}); err != nil {
		t.Fatalf("reference build: %v", err)
	}
	want, err := os.ReadFile(filepath.Join(whole, "index.bin"))
	if err != nil {
		t.Fatal(err)
	}
	wantLookup, err := os.ReadFile(filepath.Join(whole, "lookup.bin"))
	if err != nil {
		t.Fatal(err)
	}

	for _, budget := range []int{1, 12 * 1024, 12 * 4096, 12 * 16384, 12 * 65536} {
		t.Run(fmt.Sprintf("budget=%d", budget), func(t *testing.T) {
			dir := t.TempDir()
			outcome, err := index.BuildFromPaths(dir, root, paths, index.Options{
				Complete:    true,
				BufferBytes: budget,
			})
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			got, err := os.ReadFile(filepath.Join(dir, "index.bin"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("index.bin differs at %d segment(s)\n%s", outcome.Segments, firstDifference(got, want))
			}
			gotLookup, err := os.ReadFile(filepath.Join(dir, "lookup.bin"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(gotLookup, wantLookup) {
				t.Errorf("lookup.bin differs at %d segment(s)\n%s", outcome.Segments, firstDifference(gotLookup, wantLookup))
			}
			t.Logf("%d segment(s)", outcome.Segments)
		})
	}
}

// TestBuildIsDeterministic guards the property the byte-identity tests rest
// on: the same tree indexed twice produces the same bytes, whatever order the
// concurrent readers happen to finish in.
func TestBuildIsDeterministic(t *testing.T) {
	skipWithoutRef(t)
	paths := filesTable(t)

	var first []byte
	for run := range 5 {
		dir := t.TempDir()
		if _, err := index.BuildFromPaths(dir, fixture, paths, index.Options{Complete: true}); err != nil {
			t.Fatalf("build %d: %v", run, err)
		}
		got, err := os.ReadFile(filepath.Join(dir, "index.bin"))
		if err != nil {
			t.Fatal(err)
		}
		if run == 0 {
			first = got
			continue
		}
		if !bytes.Equal(got, first) {
			t.Fatalf("run %d produced different bytes from run 0\n%s", run, firstDifference(got, first))
		}
	}
}

// TestSkippedFilesDoNotConsumeIDs is the guard on the invariant the reader
// enforces: ids must be dense over indexed files, so a binary file admitted by
// the caller must land in the sidecar rather than leaving a hole.
func TestSkippedFilesDoNotConsumeIDs(t *testing.T) {
	root := t.TempDir()
	write := func(name string, content []byte) {
		if err := os.WriteFile(filepath.Join(root, name), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.txt", []byte("hello world\n"))
	write("b.bin", []byte{'x', 0, 'y', 'z', 0, 'q'}) // a NUL makes it binary
	write("c.txt", []byte("goodbye world\n"))

	dir := t.TempDir()
	outcome, err := index.BuildFromPaths(dir, root, []string{"a.txt", "b.bin", "c.txt"}, index.Options{Complete: true})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if outcome.NumFiles != 2 {
		t.Errorf("NumFiles = %d, want 2 (the binary file carries no postings)", outcome.NumFiles)
	}
	if len(outcome.SkippedBinary) != 1 || outcome.SkippedBinary[0] != "b.bin" {
		t.Errorf("SkippedBinary = %v, want [b.bin]", outcome.SkippedBinary)
	}

	// The reader is the real judge: it rejects a non-dense table outright.
	r, err := reader.Open(dir)
	if err != nil {
		t.Fatalf("our reader rejected our own index: %v", err)
	}
	defer r.Close()

	if p, _ := r.FilePath(0); p != "a.txt" {
		t.Errorf("id 0 = %q, want a.txt", p)
	}
	if p, _ := r.FilePath(1); p != "c.txt" {
		t.Errorf("id 1 = %q, want c.txt — the binary file must not have taken this id", p)
	}
	if r.ContainsPath("b.bin") {
		t.Error("the binary file is in the content index")
	}
}

// TestEmptyBuildIsAValidIndex covers the tree with nothing indexable in it,
// which must produce an index that opens and answers emptily rather than one
// that fails to open.
func TestEmptyBuildIsAValidIndex(t *testing.T) {
	dir := t.TempDir()
	if _, err := index.BuildFromPaths(dir, t.TempDir(), nil, index.Options{Complete: true}); err != nil {
		t.Fatalf("build: %v", err)
	}
	r, err := reader.Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer r.Close()
	if r.NumFiles() != 0 || r.NumTrigrams() != 0 {
		t.Errorf("empty index reports %d files, %d trigrams", r.NumFiles(), r.NumTrigrams())
	}
	if r.IsDegenerate() {
		t.Error("an empty index reads as degenerate")
	}
}

// TestGroupsAreContiguousAndOrdered re-checks, on our own output, the two
// structural invariants the reader depends on.
func TestGroupsAreContiguousAndOrdered(t *testing.T) {
	dir, _ := buildLikeReference(t, index.Options{Complete: true})

	lookup, err := os.ReadFile(filepath.Join(dir, "lookup.bin"))
	if err != nil {
		t.Fatal(err)
	}
	postings, err := os.ReadFile(filepath.Join(dir, "index.bin"))
	if err != nil {
		t.Fatal(err)
	}

	var covered uint64
	var prev uint32
	n := len(lookup) / ondisk.LookupEntrySize
	for i := range n {
		e := ondisk.DecodeLookupEntry(lookup[i*ondisk.LookupEntrySize:])
		if i > 0 && e.Trigram <= prev {
			t.Fatalf("entry %d: trigram %#x does not exceed %#x", i, e.Trigram, prev)
		}
		prev = e.Trigram
		if e.Length == 0 {
			t.Errorf("entry %d (trigram %#x) has an empty posting list", i, e.Trigram)
		}
		if e.Offset != covered {
			t.Fatalf("entry %d: offset %d leaves a gap after %d", i, e.Offset, covered)
		}
		var prevID int64 = -1
		for j := range int(e.Length) {
			p := ondisk.DecodePostingEntry(postings[e.Offset+uint64(j)*ondisk.PostingEntrySize:])
			if int64(p.FileID) <= prevID {
				t.Errorf("trigram %#x: file id %d follows %d", e.Trigram, p.FileID, prevID)
			}
			prevID = int64(p.FileID)
		}
		covered = e.Offset + uint64(e.Length)*ondisk.PostingEntrySize
	}
	if covered != uint64(len(postings)) {
		t.Errorf("groups cover %d bytes, index.bin is %d", covered, len(postings))
	}
}
