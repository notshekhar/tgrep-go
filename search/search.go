// Package search runs a query against an index and the files it names.
//
// The shape of a search is: plan the pattern into trigram lookups, resolve
// candidates from the index, then run the real matcher over only those files.
// The index never decides what matches — it only decides what is worth
// opening. Every result this package returns has been confirmed by the
// matcher against the file's actual bytes, so an index that is stale or
// over-broad costs time, never correctness.
//
// The one direction that would be a correctness bug is an index that is too
// NARROW: a file it fails to nominate is never opened and never matches. That
// is why package query gives up to MatchAll so readily, and why the soundness
// property is tested there directly.
package search

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"

	"github.com/notshekhar/tgrep-go/matching"
	"github.com/notshekhar/tgrep-go/ondisk"
	"github.com/notshekhar/tgrep-go/query"
	"github.com/notshekhar/tgrep-go/trigram"
)

// Options control a search.
type Options struct {
	// Patterns are the expressions to search for. A file matches if any does.
	Patterns []string
	// Matcher configures how the patterns are compiled.
	Matcher matching.Config
	// Match configures how one file's results are assembled.
	Match matching.Options

	// MaxFileSize skips larger files. Zero means the walker's default; a
	// negative value means no limit.
	MaxFileSize int64
	// SearchBinary searches binary files too, instead of skipping them.
	SearchBinary bool
	// Threads is how many files are searched concurrently. Zero sizes the
	// pool from the machine.
	Threads int
	// PathFilter, when set, drops candidate paths for which it returns false.
	// This is where a glob or a type filter is applied.
	PathFilter func(path string) bool
}

// FileResult is one file's contribution to a search.
type FileResult struct {
	// Path is relative to the search root.
	Path string
	// Lines are the lines to display, matches and context together.
	Lines []matching.Line
	// MatchedLines and Matches count what was found, before any display
	// limit — a count-only search needs these without the lines.
	MatchedLines int
	Matches      int
	// Err is set when the file could not be read. It does not abort the
	// search: one unreadable file in a large tree must not cost the rest.
	Err error
}

// Stats describe a whole search.
type Stats struct {
	// Candidates is how many files the index nominated.
	Candidates int
	// Searched is how many were actually opened and matched.
	Searched int
	// MatchedFiles is how many produced at least one match.
	MatchedFiles int
	// ScannedEverything is true when the plan could not narrow and every file
	// had to be read. It is the signal that a pattern is unindexable, which is
	// worth surfacing rather than hiding.
	ScannedEverything bool
}

// Result is a complete search.
type Result struct {
	Files []FileResult
	Stats Stats
}

// Index is what a search needs from an index.
//
// An interface rather than a concrete reader so that the same search code runs
// against a plain on-disk index and against one merged with a live overlay.
// The server uses the second; a one-shot CLI search uses the first, and
// neither needs to know about the other.
type Index interface {
	// LookupTrigramWithMasks returns the postings for a trigram, ascending by
	// file id.
	LookupTrigramWithMasks(hash uint32) []ondisk.PostingEntry
	// FilePath resolves a file id.
	FilePath(id uint32) (string, bool)
	// AllPaths returns every searchable path, which is what a plan that
	// cannot narrow falls back to.
	AllPaths() []string
}

// Searcher runs searches against one open index.
type Searcher struct {
	root  string
	index Index
}

// New returns a searcher over an index and the root it covers.
func New(root string, index Index) *Searcher {
	return &Searcher{root: root, index: index}
}

// Run executes a search.
//
// Results are returned in path order regardless of which goroutine finished
// first, so the same search always prints the same thing.
func (s *Searcher) Run(opts Options) (*Result, error) {
	matcher, err := matching.Build(opts.Patterns, opts.Matcher)
	if err != nil {
		return nil, err
	}

	candidates, scannedEverything, err := s.candidates(opts)
	if err != nil {
		return nil, err
	}
	if opts.PathFilter != nil {
		kept := candidates[:0]
		for _, p := range candidates {
			if opts.PathFilter(p) {
				kept = append(kept, p)
			}
		}
		candidates = kept
	}

	result := &Result{Stats: Stats{Candidates: len(candidates), ScannedEverything: scannedEverything}}
	if len(candidates) == 0 {
		return result, nil
	}

	workers := opts.Threads
	if workers <= 0 {
		workers = runtime.NumCPU()
	}
	workers = min(workers, len(candidates))

	// Each worker writes into its own slot, so the hot loop never touches a
	// shared lock and the results can be reassembled in path order at the end.
	found := make([][]FileResult, workers)
	jobs := make(chan string, workers*4)
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			for path := range jobs {
				if fr, ok := s.searchFile(path, matcher, opts); ok {
					found[slot] = append(found[slot], fr)
				}
			}
		}(w)
	}
	for _, path := range candidates {
		jobs <- path
	}
	close(jobs)
	wg.Wait()

	for _, batch := range found {
		result.Files = append(result.Files, batch...)
	}
	sort.Slice(result.Files, func(i, j int) bool { return result.Files[i].Path < result.Files[j].Path })

	result.Stats.Searched = len(candidates)
	for _, fr := range result.Files {
		if fr.MatchedLines > 0 {
			result.Stats.MatchedFiles++
		}
	}
	return result, nil
}

// candidates resolves the paths worth opening.
//
// scannedEverything reports that the plan could not narrow at all, which is a
// legitimate outcome — `.*` genuinely requires reading every file — but one
// the caller may want to say out loud, since it is the difference between a
// search that took milliseconds and one that took seconds.
func (s *Searcher) candidates(opts Options) (paths []string, scannedEverything bool, err error) {
	// SmartCase is deliberately not passed here, and that is safe rather than
	// an oversight. Smart case only turns insensitivity ON when no pattern
	// contains an uppercase letter — and for such a pattern the planner's
	// lowercasing is a no-op, so the plan it builds is identical either way.
	// When a pattern does contain uppercase, smart case leaves the search
	// sensitive, which is exactly what passing false asks for.
	plan, err := query.BuildMulti(opts.Patterns, opts.Matcher.FixedString, opts.Matcher.CaseInsensitive)
	if err != nil {
		return nil, false, err
	}

	// An inverted search asks for the lines that do NOT match, so a file
	// holding none of the pattern's trigrams is exactly the file most likely
	// to be reported. Narrowing by the index would discard precisely the
	// right answers.
	if plan.IsMatchAll() || opts.Match.InvertMatch {
		return append([]string(nil), s.index.AllPaths()...), true, nil
	}

	lookup := func(hash uint32) []ondisk.PostingEntry { return s.index.LookupTrigramWithMasks(hash) }
	ids := query.Execute(plan, lookup)

	// A single-literal pattern additionally supports the adjacency test, which
	// the plan cannot carry because its queries are sorted by hash rather than
	// by position.
	if len(opts.Patterns) == 1 && opts.Matcher.FixedString {
		ids = query.AdjacencyFilter(ids, []byte(literalFor(opts)), lookup)
	}

	paths = make([]string, 0, len(ids))
	for _, id := range ids {
		if p, ok := s.index.FilePath(id); ok {
			paths = append(paths, p)
		}
	}
	return paths, false, nil
}

// literalFor returns the single fixed-string pattern, lowercased when the
// search folds case, so it lines up with what the index recorded.
func literalFor(opts Options) string {
	p := opts.Patterns[0]
	if !opts.Matcher.CaseInsensitive {
		return p
	}
	out := []byte(p)
	for i, b := range out {
		if b >= 'A' && b <= 'Z' {
			out[i] = b + 32
		}
	}
	return string(out)
}

// searchFile reads and matches one file. ok is false when the file produced
// nothing worth reporting.
func (s *Searcher) searchFile(path string, matcher *matching.Matcher, opts Options) (FileResult, bool) {
	abs := filepath.Join(s.root, filepath.FromSlash(path))

	info, err := os.Stat(abs)
	if err != nil {
		return FileResult{Path: path, Err: err}, true
	}
	if limit := opts.MaxFileSize; limit > 0 && info.Size() > limit {
		return FileResult{}, false
	}

	content, err := os.ReadFile(abs)
	if err != nil {
		// A file the index knows about but that has since been deleted is an
		// ordinary stale-index condition, not a failure worth reporting.
		if errors.Is(err, os.ErrNotExist) {
			return FileResult{}, false
		}
		return FileResult{Path: path, Err: err}, true
	}
	if !opts.SearchBinary && trigram.IsBinary(content) {
		return FileResult{}, false
	}

	fm := matching.Find(content, matcher, opts.Match)
	if !fm.Matched() && !opts.Match.PassThru {
		return FileResult{}, false
	}
	return FileResult{
		Path:         path,
		Lines:        fm.Emit(opts.Match),
		MatchedLines: fm.Count(),
		Matches:      fm.CountMatches(),
	}, true
}
