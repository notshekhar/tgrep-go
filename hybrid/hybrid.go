// Package hybrid merges an on-disk index with an in-memory overlay.
//
// A query sees the union of the two, with the overlay winning: a file it holds
// shadows the reader's copy, and a file it records as deleted disappears from
// the reader's results entirely. That is what lets a server answer correctly
// about an edit made a second ago while still serving the bulk of the
// repository from a mapping it never has to rebuild.
//
// The reader is swapped rather than mutated. A flush builds a fresh index
// beside the old one and installs it in one assignment, so an in-flight query
// keeps reading the mapping it started with and no lock is held for the length
// of a search.
package hybrid

import (
	"fmt"
	"sync"

	"github.com/notshekhar/tgrep-go/live"
	"github.com/notshekhar/tgrep-go/ondisk"
	"github.com/notshekhar/tgrep-go/reader"
)

// Index is a reader plus its overlay.
type Index struct {
	// mu guards the reader pointer only, never a whole query. Swapping it is
	// one assignment; searching through it takes the read lock just long
	// enough to copy the pointer.
	mu     sync.RWMutex
	rd     *reader.Reader
	Live   *live.Index
	Root   string
	closed bool
}

// Open maps an index directory and attaches an empty overlay.
func Open(indexDir, root string) (*Index, error) {
	rd, err := reader.Open(indexDir)
	if err != nil {
		return nil, err
	}
	// A structurally inconsistent reader is rejected here rather than left to
	// produce silently empty results later.
	if rd.IsDegenerate() {
		rd.Close()
		return nil, fmt.Errorf("%w: mapped sections are non-empty but the trigram count is zero", ondisk.ErrCorrupted)
	}
	// Validating also walks every lookup entry, which warms the mapping into
	// the page cache so the first real search does not pay for cold faults.
	if err := rd.ValidateLookup(); err != nil {
		rd.Close()
		return nil, err
	}
	return &Index{rd: rd, Live: live.New(), Root: root}, nil
}

// NewWith wraps an already-open reader, for callers that opened it themselves.
func NewWith(rd *reader.Reader, root string) *Index {
	return &Index{rd: rd, Live: live.New(), Root: root}
}

// reader returns the current reader.
func (ix *Index) reader() *reader.Reader {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.rd
}

// Reader exposes the current on-disk reader, for callers that need to stream
// its postings during a flush.
func (ix *Index) Reader() *reader.Reader { return ix.reader() }

// SwapReader installs a new reader and returns the old one.
//
// The old reader is returned rather than closed, because a query that started
// before the swap is still reading its mapping; the caller closes it once it
// knows nothing holds it. Unmapping under a live reader is a segfault, not an
// error.
func (ix *Index) SwapReader(fresh *reader.Reader) *reader.Reader {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	old := ix.rd
	ix.rd = fresh
	return old
}

// Close releases the reader.
func (ix *Index) Close() error {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if ix.closed || ix.rd == nil {
		return nil
	}
	ix.closed = true
	return ix.rd.Close()
}

// readerEntryActive reports whether a reader-side file is still the truth.
//
// It is not when the overlay holds a newer copy of that path, or has recorded
// it as deleted. This is the single place the overlay's precedence is decided,
// and every merge below goes through it.
func (ix *Index) readerEntryActive(rd *reader.Reader, id uint32) bool {
	path, ok := rd.FilePath(id)
	if !ok {
		return false
	}
	return !ix.Live.HasPath(path) && !ix.Live.IsDeleted(path)
}

// LookupTrigram returns candidate ids from both layers.
func (ix *Index) LookupTrigram(tri uint32) []uint32 {
	postings := ix.LookupTrigramWithMasks(tri)
	ids := make([]uint32, len(postings))
	for i, p := range postings {
		ids[i] = p.FileID
	}
	return ids
}

// LookupTrigramWithMasks returns candidate postings from both layers.
//
// Reader postings come first and overlay postings after, which keeps the whole
// list ascending: every overlay id has the top bit set and so exceeds every
// reader id. The intersection in package query depends on that ordering.
func (ix *Index) LookupTrigramWithMasks(tri uint32) []ondisk.PostingEntry {
	rd := ix.reader()
	fromDisk := rd.LookupTrigramWithMasks(tri)

	out := make([]ondisk.PostingEntry, 0, len(fromDisk))
	for _, p := range fromDisk {
		if ix.readerEntryActive(rd, p.FileID) {
			out = append(out, p)
		}
	}
	return append(out, ix.Live.LookupTrigramWithMasks(tri)...)
}

// FilePath resolves an id from whichever layer owns it.
func (ix *Index) FilePath(id uint32) (string, bool) {
	if live.IsOverlayID(id) {
		return ix.Live.FilePath(id)
	}
	return ix.reader().FilePath(id)
}

// AllFileIDs returns every active id across both layers, ascending.
func (ix *Index) AllFileIDs() []uint32 {
	rd := ix.reader()
	var ids []uint32
	for _, id := range rd.AllFileIDs() {
		if ix.readerEntryActive(rd, id) {
			ids = append(ids, id)
		}
	}
	return append(ids, ix.Live.AllFileIDs()...)
}

// AllPaths returns every active path across both layers.
func (ix *Index) AllPaths() []string {
	rd := ix.reader()
	var paths []string
	for id, path := range rd.AllPaths() {
		if ix.readerEntryActive(rd, uint32(id)) {
			paths = append(paths, path)
		}
	}
	return append(paths, ix.Live.AllPaths()...)
}

// HasActivePath reports whether a path is searchable in either layer.
func (ix *Index) HasActivePath(path string) bool {
	if ix.Live.HasPath(path) {
		return true
	}
	if ix.Live.IsDeleted(path) {
		return false
	}
	return ix.reader().ContainsPath(path)
}

// NumFiles is how many files are active across both layers.
func (ix *Index) NumFiles() int { return len(ix.AllPaths()) }

// NumTrigrams is the on-disk trigram count.
//
// Deliberately not a union: the two layers overlap heavily and counting them
// together would report a number that is neither the index's size nor the
// overlay's. The on-disk figure is the one that describes the index on disk.
func (ix *Index) NumTrigrams() int { return ix.reader().NumTrigrams() }

// LiveFiles is how many files the overlay holds.
func (ix *Index) LiveFiles() int { return ix.Live.NumFiles() }
