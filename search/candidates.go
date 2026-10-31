package search

import (
	"slices"

	"github.com/notshekhar/tgrep-go/matching"
	"github.com/notshekhar/tgrep-go/ondisk"
	"github.com/notshekhar/tgrep-go/query"
)

// Candidates returns the paths that could contain a match, without opening any
// of them.
//
// This is the narrowing half of a search on its own, for a host program that
// already has its own matcher and only wants to be told which files are worth
// reading. Embedding tgrep that way is often the right shape: the host keeps
// its own line semantics, its own result ordering and its own output, and
// borrows nothing but the index.
//
// The contract that makes it safe to build on:
//
//   - `exhaustive` false means the plan could not narrow and the caller must
//     search everything. It is not an error and not an empty result — `.*`
//     genuinely requires reading every file — but a caller that treats it as
//     "no candidates" would silently return nothing.
//   - When exhaustive is true the returned paths are a SUPERSET of the files
//     that really match. Over-inclusion costs a file read; under-inclusion
//     would be a wrong answer, which is why package query gives up so readily.
//
// Paths come back sorted, so a caller feeding them to a parallel searcher
// still gets a deterministic order to reassemble into.
func Candidates(index Index, patterns []string, cfg matching.Config) (paths []string, exhaustive bool, err error) {
	plan, err := query.BuildMulti(patterns, cfg.FixedString, cfg.FoldsCase(patterns))
	if err != nil {
		return nil, false, err
	}
	if plan.IsMatchAll() {
		return nil, false, nil
	}

	lookup := func(hash uint32) []ondisk.PostingEntry { return index.LookupTrigramWithMasks(hash) }
	ids := query.Execute(plan, lookup)

	// Ids ascend, and a path table is dense, so the paths come out in id order
	// rather than sorted by name. Callers get name order because that is what
	// a stable result listing needs.
	paths = make([]string, 0, len(ids))
	for _, id := range ids {
		if p, ok := index.FilePath(id); ok {
			paths = append(paths, p)
		}
	}
	slices.Sort(paths)
	return paths, true, nil
}
