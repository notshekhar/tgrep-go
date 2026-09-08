package query

import (
	"slices"

	"github.com/notshekhar/tgrep-go/ondisk"
	"github.com/notshekhar/tgrep-go/trigram"
)

// Lookup resolves one trigram to its posting list, ascending by file id.
type Lookup func(hash uint32) []ondisk.PostingEntry

// Execute evaluates a plan and returns candidate file ids, ascending.
//
// A MatchAll plan returns no candidates and the caller must scan every file
// instead — the empty result means "this plan cannot narrow", not "nothing
// matches". Callers distinguish the two with Plan.IsMatchAll.
func Execute(plan Plan, lookup Lookup) []uint32 {
	switch plan.Kind {
	case KindAnd:
		return executeAnd(plan.Queries, lookup)
	case KindOr:
		lists := make([][]uint32, 0, len(plan.Branches))
		for _, branch := range plan.Branches {
			lists = append(lists, Execute(branch, lookup))
		}
		return unionSorted(lists)
	default:
		return nil
	}
}

// executeAnd intersects the posting lists of every required trigram.
func executeAnd(queries []TrigramQuery, lookup Lookup) []uint32 {
	if len(queries) == 0 {
		return nil
	}

	// Resolve every list first, then intersect starting from the shortest.
	// The result can only shrink, so beginning with the smallest list keeps
	// every subsequent merge as short as possible — and on a real query the
	// rarest trigram is often orders of magnitude shorter than the commonest.
	lists := make([][]uint32, 0, len(queries))
	for _, q := range queries {
		lists = append(lists, filterByNextMask(lookup(q.Hash), q))
		// An empty list means no file holds this trigram, so the intersection
		// is empty however the rest resolve.
		if len(lists[len(lists)-1]) == 0 {
			return nil
		}
	}
	slices.SortFunc(lists, func(a, b []uint32) int { return len(a) - len(b) })

	result := lists[0]
	for _, list := range lists[1:] {
		result = intersectSorted(result, list)
		if len(result) == 0 {
			break
		}
	}
	return result
}

// filterByNextMask drops postings whose Bloom filter proves the required
// following byte never appears after this trigram in that file.
//
// This is the constraint a plain trigram index cannot express. Searching for
// "hello", a file containing "hella" and "jello" holds every trigram of the
// literal, and only the next-byte filter can tell that "hel" is never followed
// by 'l' there.
//
// A Bloom filter admits bytes that are absent but never rejects one that is
// present, so this can only ever remove files that genuinely cannot match.
func filterByNextMask(postings []ondisk.PostingEntry, q TrigramQuery) []uint32 {
	ids := make([]uint32, 0, len(postings))
	for _, p := range postings {
		if q.HasNext {
			masks := trigram.Masks{LocMask: p.LocMask, NextMask: p.NextMask}
			if !masks.MayBeFollowedBy(q.ExpectedNext) {
				continue
			}
		}
		ids = append(ids, p.FileID)
	}
	return ids
}

// intersectSorted returns the ids present in both ascending lists.
func intersectSorted(a, b []uint32) []uint32 {
	out := make([]uint32, 0, min(len(a), len(b)))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			out = append(out, a[i])
			i++
			j++
		case a[i] < b[j]:
			i++
		default:
			j++
		}
	}
	return out
}

// unionSorted merges ascending lists, removing duplicates.
func unionSorted(lists [][]uint32) []uint32 {
	var total int
	for _, l := range lists {
		total += len(l)
	}
	if total == 0 {
		return nil
	}
	out := make([]uint32, 0, total)
	for _, l := range lists {
		out = append(out, l...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// AdjacencyFilter narrows candidates using the location masks, testing that a
// literal's trigrams could actually sit at consecutive offsets in each file.
//
// It takes the literal itself rather than a Plan, deliberately. Adjacency is a
// statement about position, and a plan's queries have been sorted by trigram
// hash and deduplicated by the time anyone can reach them — so running this
// over Plan.Queries would compare masks of trigrams that are not neighbours
// and reject files that genuinely match. The literal is the only form that
// still carries the order this test depends on.
//
// It also applies only to a single contiguous literal. Across a wildcard the
// pattern does not require adjacency at all, and demanding it would again drop
// real matches.
//
// Like the Bloom filter this can reject but never invent: a zero intersection
// of rotated masks proves the two trigrams never occupy adjacent offsets.
func AdjacencyFilter(candidates []uint32, literal []byte, lookup Lookup) []uint32 {
	if len(literal) < 4 || len(candidates) == 0 {
		// Fewer than two trigrams means there is no adjacency to test.
		return candidates
	}

	// masksByFile[i] maps a file id to its masks for the trigram at offset i
	// of the literal — consecutive windows, in order.
	windows := len(literal) - 2
	masksByFile := make([]map[uint32]trigram.Masks, windows)
	for i := range windows {
		hash := trigram.Hash(literal[i], literal[i+1], literal[i+2])
		m := make(map[uint32]trigram.Masks)
		for _, p := range lookup(hash) {
			m[p.FileID] = trigram.Masks{LocMask: p.LocMask, NextMask: p.NextMask}
		}
		masksByFile[i] = m
	}

	out := candidates[:0]
	run := make([]trigram.Masks, 0, windows)
	for _, id := range candidates {
		run = run[:0]
		complete := true
		for i := range windows {
			m, ok := masksByFile[i][id]
			if !ok {
				complete = false
				break
			}
			run = append(run, m)
		}
		// A file missing one of the trigrams cannot match, but excluding it is
		// the intersection's job; here it just means there is nothing to test.
		if complete && !trigram.CheckAdjacency(run) {
			continue
		}
		out = append(out, id)
	}
	return out
}
