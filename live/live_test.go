package live_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/notshekhar/tgrep-go/hybrid"
	"github.com/notshekhar/tgrep-go/index"
	"github.com/notshekhar/tgrep-go/live"
	"github.com/notshekhar/tgrep-go/matching"
	"github.com/notshekhar/tgrep-go/reader"
	"github.com/notshekhar/tgrep-go/search"
	"github.com/notshekhar/tgrep-go/trigram"
)

func TestOverlayIDsAreDistinguishable(t *testing.T) {
	ix := live.New()
	ix.Upsert("a.txt", []byte("hello world"))

	ids := ix.AllFileIDs()
	if len(ids) != 1 {
		t.Fatalf("got %d ids, want 1", len(ids))
	}
	if !live.IsOverlayID(ids[0]) {
		t.Errorf("id %#x is not marked as an overlay id", ids[0])
	}
	// Every overlay id must exceed every possible reader id, so that merged
	// posting lists stay ascending without a re-sort.
	if ids[0] < live.OverlayBit {
		t.Errorf("id %#x sorts below the reader's id space", ids[0])
	}
}

func TestUpsertReplacesRatherThanAccumulates(t *testing.T) {
	ix := live.New()
	ix.Upsert("a.txt", []byte("alpha beta"))
	ix.Upsert("a.txt", []byte("gamma delta"))

	if got := ix.NumFiles(); got != 1 {
		t.Errorf("NumFiles = %d after two upserts of one path, want 1", got)
	}
	// The old content's trigrams must be gone, not merely shadowed.
	if got := ix.LookupTrigram(trigram.Hash('a', 'l', 'p')); len(got) != 0 {
		t.Errorf("a trigram from the replaced content survives: %v", got)
	}
	if got := ix.LookupTrigram(trigram.Hash('g', 'a', 'm')); len(got) != 1 {
		t.Errorf("a trigram from the new content is missing: %v", got)
	}
}

// TestEmptiedPostingListsAreDropped guards a leak that would only show up as a
// slowly climbing trigram count on a long-running server.
func TestEmptiedPostingListsAreDropped(t *testing.T) {
	ix := live.New()
	for range 50 {
		ix.Upsert("a.txt", []byte("alpha beta gamma"))
	}
	first := ix.NumTrigrams()
	for range 50 {
		ix.Upsert("a.txt", []byte("alpha beta gamma"))
	}
	if got := ix.NumTrigrams(); got != first {
		t.Errorf("trigram count drifted from %d to %d across repeated upserts of identical content", first, got)
	}

	ix.Delete("a.txt")
	if got := ix.NumTrigrams(); got != 0 {
		t.Errorf("NumTrigrams = %d after deleting the only file, want 0", got)
	}
}

func TestDeleteIsRememberedEvenForUnknownPaths(t *testing.T) {
	ix := live.New()
	// The overlay never held this path, but the on-disk reader may — and this
	// set is the only thing that can shadow it.
	ix.Delete("never-seen.txt")
	if !ix.IsDeleted("never-seen.txt") {
		t.Error("a deletion of an unknown path was forgotten")
	}
}

func TestUpsertUndeletes(t *testing.T) {
	ix := live.New()
	ix.Delete("a.txt")
	ix.Upsert("a.txt", []byte("back again"))
	if ix.IsDeleted("a.txt") {
		t.Error("a path rewritten after deletion is still marked deleted")
	}
	if !ix.HasPath("a.txt") {
		t.Error("a path rewritten after deletion is not present")
	}
}

func TestPostingsAreAscending(t *testing.T) {
	ix := live.New()
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		ix.Upsert(name+".txt", []byte("shared content here"))
	}
	postings := ix.LookupTrigramWithMasks(trigram.Hash('s', 'h', 'a'))
	if len(postings) != 5 {
		t.Fatalf("got %d postings, want 5", len(postings))
	}
	for i := 1; i < len(postings); i++ {
		if postings[i].FileID <= postings[i-1].FileID {
			t.Errorf("postings are not ascending: %d after %d", postings[i].FileID, postings[i-1].FileID)
		}
	}
}

// buildHybrid indexes files on disk, then returns a hybrid index over them.
func buildHybrid(t *testing.T, files map[string]string) (*hybrid.Index, string) {
	t.Helper()
	root := t.TempDir()
	var paths []string
	for name, body := range files {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, name)
	}
	slices.Sort(paths)

	dir := t.TempDir()
	if _, err := index.BuildFromPaths(dir, root, paths, index.Options{Complete: true}); err != nil {
		t.Fatalf("build: %v", err)
	}
	rd, err := reader.Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ix := hybrid.NewWith(rd, root)
	t.Cleanup(func() { ix.Close() })
	return ix, root
}

// TestOverlayShadowsTheReader is the precedence rule the whole design rests
// on: once the overlay holds a path, the reader's copy of it must be invisible.
func TestOverlayShadowsTheReader(t *testing.T) {
	ix, _ := buildHybrid(t, map[string]string{
		"a.txt": "original content alpha",
		"b.txt": "untouched content beta",
	})

	// Before the edit, the reader's trigram is found.
	if got := ix.LookupTrigram(trigram.Hash('a', 'l', 'p')); len(got) != 1 {
		t.Fatalf(`"alp" resolves to %v before the edit, want one file`, got)
	}

	ix.Live.Upsert("a.txt", []byte("rewritten content gamma"))

	// The old content must no longer be reachable through the reader.
	if got := ix.LookupTrigram(trigram.Hash('a', 'l', 'p')); len(got) != 0 {
		t.Errorf(`"alp" still resolves to %v after a.txt was rewritten without it`, got)
	}
	// The new content must be.
	found := ix.LookupTrigram(trigram.Hash('g', 'a', 'm'))
	if len(found) != 1 || !live.IsOverlayID(found[0]) {
		t.Errorf(`"gam" resolves to %v, want one overlay id`, found)
	}
	// The untouched file is unaffected.
	if got := ix.LookupTrigram(trigram.Hash('b', 'e', 't')); len(got) != 1 {
		t.Errorf(`"bet" in the untouched file resolves to %v, want one file`, got)
	}
}

func TestDeletionHidesAReaderFile(t *testing.T) {
	ix, _ := buildHybrid(t, map[string]string{
		"a.txt": "alpha content",
		"b.txt": "beta content",
	})

	ix.Live.Delete("a.txt")

	if got := ix.LookupTrigram(trigram.Hash('a', 'l', 'p')); len(got) != 0 {
		t.Errorf("a deleted file's trigrams are still reachable: %v", got)
	}
	if ix.HasActivePath("a.txt") {
		t.Error("a deleted path is still active")
	}
	if !ix.HasActivePath("b.txt") {
		t.Error("an untouched path stopped being active")
	}
	paths := ix.AllPaths()
	if slices.Contains(paths, "a.txt") {
		t.Errorf("AllPaths still lists the deleted file: %v", paths)
	}
	if !slices.Contains(paths, "b.txt") {
		t.Errorf("AllPaths lost the untouched file: %v", paths)
	}
}

func TestFilePathResolvesAcrossBothLayers(t *testing.T) {
	ix, _ := buildHybrid(t, map[string]string{"a.txt": "alpha"})
	ix.Live.Upsert("new.txt", []byte("gamma content"))

	for _, id := range ix.AllFileIDs() {
		path, ok := ix.FilePath(id)
		if !ok || path == "" {
			t.Errorf("id %#x did not resolve to a path", id)
		}
	}
}

// TestSearchSeesLiveEdits is the end-to-end point of the overlay: a file
// rewritten in memory is searchable immediately, with the correct content.
func TestSearchSeesLiveEdits(t *testing.T) {
	ix, root := buildHybrid(t, map[string]string{
		"a.txt": "original alpha line\n",
		"b.txt": "untouched beta line\n",
	})

	// Rewrite the file on disk AND in the overlay, as the watcher would.
	newBody := "rewritten gamma line\n"
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte(newBody), 0o644); err != nil {
		t.Fatal(err)
	}
	ix.Live.Upsert("a.txt", []byte(newBody))

	s := search.New(root, ix)

	res, err := s.Run(search.Options{Patterns: []string{"gamma"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 1 || res.Files[0].Path != "a.txt" {
		t.Errorf("searching for the new content found %v, want a.txt", pathsOf(res))
	}

	// And the old content is gone.
	res, err = s.Run(search.Options{Patterns: []string{"original"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 0 {
		t.Errorf("searching for the replaced content found %v, want nothing", pathsOf(res))
	}
}

// TestSearchSeesAFileTheIndexNeverHad covers the other half: a file created
// after the index was built.
func TestSearchSeesAFileTheIndexNeverHad(t *testing.T) {
	ix, root := buildHybrid(t, map[string]string{"a.txt": "alpha line\n"})

	body := "brand new epsilon line\n"
	if err := os.WriteFile(filepath.Join(root, "new.txt"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	ix.Live.Upsert("new.txt", []byte(body))

	res, err := search.New(root, ix).Run(search.Options{Patterns: []string{"epsilon"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 1 || res.Files[0].Path != "new.txt" {
		t.Errorf("found %v, want new.txt", pathsOf(res))
	}
}

// TestInvertedSearchCoversBothLayers checks the MatchAll path through the
// hybrid index, which resolves candidates from AllPaths rather than postings.
func TestInvertedSearchCoversBothLayers(t *testing.T) {
	ix, root := buildHybrid(t, map[string]string{
		"a.txt": "has the token\n",
		"b.txt": "does not\n",
	})
	body := "also lacks it\n"
	if err := os.WriteFile(filepath.Join(root, "c.txt"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	ix.Live.Upsert("c.txt", []byte(body))

	res, err := search.New(root, ix).Run(search.Options{
		Patterns: []string{"token"},
		Match:    matching.Options{InvertMatch: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := pathsOf(res)
	for _, want := range []string{"b.txt", "c.txt"} {
		if !slices.Contains(got, want) {
			t.Errorf("inverted search missed %s (got %v)", want, got)
		}
	}
}

func pathsOf(res *search.Result) []string {
	var out []string
	for _, fr := range res.Files {
		out = append(out, fr.Path)
	}
	slices.Sort(out)
	return out
}

// TestWatcherPicksUpChanges drives the real watcher against the filesystem.
//
// Timing is unavoidable here — the watcher debounces — so the test waits for a
// condition rather than sleeping a fixed amount, and fails with what it saw if
// the condition never arrives.
func TestWatcherPicksUpChanges(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ix := live.New()
	w, err := live.NewWatcher(root, ix, live.WatchOptions{Debounce: 20 * time.Millisecond})
	if err != nil {
		t.Fatalf("watcher: %v", err)
	}
	defer w.Close()

	t.Run("a new file is indexed", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("second content\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		waitFor(t, func() bool { return ix.HasPath("b.txt") }, "b.txt to be indexed")
		if got := ix.LookupTrigram(trigram.Hash('s', 'e', 'c')); len(got) != 1 {
			t.Errorf(`"sec" from the new file resolves to %v`, got)
		}
	})

	t.Run("an edit replaces the old content", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("replaced words\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		waitFor(t, func() bool { return len(ix.LookupTrigram(trigram.Hash('r', 'e', 'p'))) == 1 }, "the edit to land")
		if got := ix.LookupTrigram(trigram.Hash('s', 'e', 'c')); len(got) != 0 {
			t.Errorf("the replaced content is still indexed: %v", got)
		}
	})

	t.Run("a deletion is recorded", func(t *testing.T) {
		if err := os.Remove(filepath.Join(root, "b.txt")); err != nil {
			t.Fatal(err)
		}
		waitFor(t, func() bool { return ix.IsDeleted("b.txt") }, "the deletion to be recorded")
		if ix.HasPath("b.txt") {
			t.Error("a deleted file is still present in the overlay")
		}
	})

	t.Run("a new directory is watched", func(t *testing.T) {
		sub := filepath.Join(root, "sub")
		if err := os.Mkdir(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sub, "c.txt"), []byte("nested omega\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		// fsnotify is not recursive: without the watcher registering new
		// directories itself, this file would never be seen.
		waitFor(t, func() bool { return ix.HasPath("sub/c.txt") }, "a file in a new directory to be indexed")
	})
}

// TestWatcherRespectsIgnoreRules is the invariant that keeps the watcher and
// the walk agreeing about what the index contains.
func TestWatcherRespectsIgnoreRules(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("*.log\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A git repository, because gitignore rules only apply inside one.
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	ix := live.New()
	w, err := live.NewWatcher(root, ix, live.WatchOptions{Debounce: 20 * time.Millisecond})
	if err != nil {
		t.Fatalf("watcher: %v", err)
	}
	defer w.Close()

	if err := os.WriteFile(filepath.Join(root, "kept.txt"), []byte("visible\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "noisy.log"), []byte("ignored\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	waitFor(t, func() bool { return ix.HasPath("kept.txt") }, "the admitted file to be indexed")
	// Give the ignored one the same chance to arrive before concluding it did not.
	time.Sleep(100 * time.Millisecond)
	if ix.HasPath("noisy.log") {
		t.Error("the watcher indexed a file the walk would have excluded")
	}
}

// TestWatcherSkipsBinaryFiles applies the other admission rule.
func TestWatcherSkipsBinaryFiles(t *testing.T) {
	root := t.TempDir()
	ix := live.New()
	w, err := live.NewWatcher(root, ix, live.WatchOptions{Debounce: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	if err := os.WriteFile(filepath.Join(root, "text.txt"), []byte("readable\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "blob.dat"), []byte{'a', 0, 'b'}, 0o644); err != nil {
		t.Fatal(err)
	}

	waitFor(t, func() bool { return ix.HasPath("text.txt") }, "the text file to be indexed")
	time.Sleep(100 * time.Millisecond)
	if ix.HasPath("blob.dat") {
		t.Error("a binary file was indexed")
	}
}

// waitFor polls until a condition holds, so a debounced watcher does not need
// a fixed sleep that is either flaky or slow.
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
