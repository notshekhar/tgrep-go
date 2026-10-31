// Package reader opens a built index for querying.
//
// lookup.bin and index.bin are memory-mapped rather than read: a lookup is
// then a binary search straight into the mapped pages, with no parse step, no
// in-memory table, and no cost for the trigrams a query never asks about. On a
// large repository the lookup table alone is tens of megabytes, and a query
// touches a handful of its entries.
//
// files.bin is read normally — every query that returns a match needs a path,
// and the table is small next to the postings.
package reader

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/edsrzf/mmap-go"

	"github.com/notshekhar/tgrep-go/ondisk"
)

// Reader is a read-only view of one index directory.
//
// It is safe for concurrent use: everything it holds is immutable between Open
// and Close. Close is the exception and is serialised, because a mapping that
// is unmapped while another goroutine reads it takes the process down rather
// than returning an error.
type Reader struct {
	mu       sync.RWMutex
	lookup   mmap.MMap
	postings mmap.MMap

	filePaths []string
	// pathOrder holds file ids sorted by their path, so a membership test is a
	// binary search rather than a second copy of every path in a set.
	pathOrder  []int
	numEntries int
}

// Open maps an index directory for reading.
func Open(indexDir string) (*Reader, error) {
	lookupPath := filepath.Join(indexDir, "lookup.bin")
	postingsPath := filepath.Join(indexDir, "index.bin")
	filesPath := filepath.Join(indexDir, "files.bin")

	for _, p := range []string{lookupPath, postingsPath, filesPath} {
		if _, err := os.Stat(p); err != nil {
			return nil, fmt.Errorf("index not found at %s: %w", indexDir, err)
		}
	}

	r := &Reader{}

	lookupLen, err := fileLen(lookupPath)
	if err != nil {
		return nil, err
	}
	postingsLen, err := fileLen(postingsPath)
	if err != nil {
		return nil, err
	}

	// An empty index is a legitimate state (a tree with nothing indexable in
	// it), and mapping a zero-length file fails on every platform, so the two
	// mappings are simply left nil.
	if lookupLen > 0 && postingsLen > 0 {
		// A lookup.bin whose length is not a whole number of entries would
		// silently truncate its last record, and integer division would hide
		// that from the binary search. Refuse it loudly instead.
		if lookupLen%ondisk.LookupEntrySize != 0 {
			return nil, fmt.Errorf("%w: lookup.bin size %d is not a multiple of %d",
				ondisk.ErrCorrupted, lookupLen, ondisk.LookupEntrySize)
		}
		if r.lookup, err = mapFile(lookupPath); err != nil {
			return nil, err
		}
		if r.postings, err = mapFile(postingsPath); err != nil {
			r.lookup.Unmap()
			return nil, err
		}
		r.numEntries = len(r.lookup) / ondisk.LookupEntrySize
	}

	filesData, err := os.ReadFile(filesPath)
	if err != nil {
		r.Close()
		return nil, err
	}
	body, err := ondisk.FileTableBody(filesData)
	if err != nil {
		r.Close()
		return nil, err
	}
	entries, err := ondisk.DecodeFileEntries(body)
	if err != nil {
		r.Close()
		return nil, err
	}

	// File ids must be dense in 0..N with no duplicates. Without this check a
	// corrupt record declaring an id near uint32 max would size the path slice
	// from it, turning a bad byte into a multi-gigabyte allocation.
	n := len(entries)
	seen := make([]bool, n)
	r.filePaths = make([]string, n)
	for _, e := range entries {
		id := int(e.FileID)
		if id >= n {
			r.Close()
			return nil, fmt.Errorf("%w: files.bin contains out-of-range file_id %d (entry count %d); ids must be dense in 0..%d",
				ondisk.ErrCorrupted, e.FileID, n, n)
		}
		if seen[id] {
			r.Close()
			return nil, fmt.Errorf("%w: files.bin contains duplicate file_id %d", ondisk.ErrCorrupted, e.FileID)
		}
		seen[id] = true
		r.filePaths[id] = e.Path
	}

	r.pathOrder = make([]int, n)
	for i := range r.pathOrder {
		r.pathOrder[i] = i
	}
	slices.SortFunc(r.pathOrder, func(a, b int) int {
		return cmp.Compare(r.filePaths[a], r.filePaths[b])
	})

	return r, nil
}

// Empty returns a reader that holds nothing and answers no query.
//
// It exists so a caller swapping in a freshly built index can release the
// previous mapping first, without ever holding a nil reader.
func Empty() *Reader { return &Reader{} }

func fileLen(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func mapFile(path string) (mmap.MMap, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	// The mapping outlives the descriptor: once the pages are mapped the file
	// handle is no longer needed, and holding it open would keep the file
	// undeletable on Windows for no gain.
	defer f.Close()
	return mmap.Map(f, mmap.RDONLY, 0)
}

// Close releases the mappings so the underlying files can be replaced.
//
// On Windows a mapped file cannot be overwritten at all, so the index swap
// after a rebuild depends on this being called first; on Unix it is merely
// tidy. Calling it twice is safe.
func (r *Reader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var err error
	if r.lookup != nil {
		err = r.lookup.Unmap()
		r.lookup = nil
	}
	if r.postings != nil {
		if e := r.postings.Unmap(); err == nil {
			err = e
		}
		r.postings = nil
	}
	r.filePaths = nil
	r.pathOrder = nil
	r.numEntries = 0
	return err
}

// lookupEntry decodes the i-th record of the lookup table.
func (r *Reader) lookupEntry(i int) ondisk.LookupEntry {
	return ondisk.DecodeLookupEntry(r.lookup[i*ondisk.LookupEntrySize:])
}

// postingsAt decodes one posting list.
func (r *Reader) postingsAt(offset uint64, length uint32) []ondisk.PostingEntry {
	end := offset + uint64(length)*ondisk.PostingEntrySize
	if end > uint64(len(r.postings)) {
		// A corrupt range reaching past the file yields nothing rather than
		// panicking: Open validated the table it was handed, so reaching here
		// means the file changed underneath a live mapping.
		return nil
	}
	out := make([]ondisk.PostingEntry, length)
	for i := range out {
		out[i] = ondisk.DecodePostingEntry(r.postings[offset+uint64(i)*ondisk.PostingEntrySize:])
	}
	return out
}

// searchLookup binary searches the table for a trigram, returning its index.
func (r *Reader) searchLookup(tri uint32) (int, bool) {
	i := sort.Search(r.numEntries, func(i int) bool {
		return r.lookupEntry(i).Trigram >= tri
	})
	if i < r.numEntries && r.lookupEntry(i).Trigram == tri {
		return i, true
	}
	return 0, false
}

// LookupTrigram returns the ids of every file containing the trigram, ascending.
func (r *Reader) LookupTrigram(tri uint32) []uint32 {
	postings := r.LookupTrigramWithMasks(tri)
	ids := make([]uint32, len(postings))
	for i, p := range postings {
		ids[i] = p.FileID
	}
	return ids
}

// LookupTrigramWithMasks returns the full posting list for a trigram,
// including the masks a query uses to reject candidates without reading them.
func (r *Reader) LookupTrigramWithMasks(tri uint32) []ondisk.PostingEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.lookup == nil {
		return nil
	}
	i, ok := r.searchLookup(tri)
	if !ok {
		return nil
	}
	e := r.lookupEntry(i)
	return r.postingsAt(e.Offset, e.Length)
}

// FilePath returns the path for a file id.
func (r *Reader) FilePath(id uint32) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if int(id) >= len(r.filePaths) {
		return "", false
	}
	return r.filePaths[id], true
}

// ContainsPath reports whether the index holds this exact path.
func (r *Reader) ContainsPath(path string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	i := sort.Search(len(r.pathOrder), func(i int) bool {
		return r.filePaths[r.pathOrder[i]] >= path
	})
	return i < len(r.pathOrder) && r.filePaths[r.pathOrder[i]] == path
}

// HasDescendantPath reports whether the index holds any path strictly below
// directory. An empty directory means the root, which every path is below.
func (r *Reader) HasDescendantPath(directory string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if directory == "" {
		return len(r.pathOrder) > 0
	}
	prefix := directory + "/"
	i := sort.Search(len(r.pathOrder), func(i int) bool {
		return r.filePaths[r.pathOrder[i]] >= prefix
	})
	return i < len(r.pathOrder) && strings.HasPrefix(r.filePaths[r.pathOrder[i]], prefix)
}

// NumFiles is how many files the index holds paths for.
func (r *Reader) NumFiles() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.filePaths)
}

// NumTrigrams is how many distinct trigrams the index holds.
func (r *Reader) NumTrigrams() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.numEntries
}

// AllFileIDs returns every id in the index.
func (r *Reader) AllFileIDs() []uint32 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := make([]uint32, len(r.filePaths))
	for i := range ids {
		ids[i] = uint32(i)
	}
	return ids
}

// AllPaths returns every path, indexed by file id.
//
// The slice is the reader's own and must not be modified.
func (r *Reader) AllPaths() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.filePaths
}

// IsDegenerate reports a structurally inconsistent reader: no trigrams, yet a
// non-empty mapping that should have produced some.
//
// A well-formed index may hold files that yield no trigrams at all (anything
// shorter than three bytes), so zero trigrams alone proves nothing. It is only
// suspicious alongside a mapped section that is not empty, which is what a
// stale-metadata zero-length mapping looks like from in here.
func (r *Reader) IsDegenerate() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.numEntries == 0 && (len(r.lookup) > 0 || len(r.postings) > 0)
}

// ValidateLookup checks that the table ascends strictly and that every posting
// range lies inside index.bin.
//
// It walks every entry in order, which as a side effect warms the mapped pages
// into the page cache, so the binary searches that follow never take a cold
// fault mid-query.
func (r *Reader) ValidateLookup() error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.lookup == nil {
		return nil // an empty index has nothing to validate
	}
	postingsLen := uint64(len(r.postings))
	var prev uint32
	for i := range r.numEntries {
		e := r.lookupEntry(i)
		if i > 0 && e.Trigram <= prev {
			return fmt.Errorf("%w: lookup.bin not sorted at entry %d: trigram %#x <= previous %#x",
				ondisk.ErrCorrupted, i, e.Trigram, prev)
		}
		prev = e.Trigram
		if e.Offset+uint64(e.Length)*ondisk.PostingEntrySize > postingsLen {
			return fmt.Errorf("%w: lookup entry %d (trigram %#x): posting range [offset=%d, length=%d] exceeds index.bin length %d",
				ondisk.ErrCorrupted, i, e.Trigram, e.Offset, e.Length, postingsLen)
		}
	}
	return nil
}

// TrigramPostings decodes the i-th trigram entry with its full posting list.
//
// Paired with NumTrigrams this lets a caller drive the decode itself — across
// goroutines, and fused with its own per-entry work — instead of materialising
// the whole index first.
func (r *Reader) TrigramPostings(i int) (uint32, []ondisk.PostingEntry) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.lookup == nil || i < 0 || i >= r.numEntries {
		return 0, nil
	}
	e := r.lookupEntry(i)
	return e.Trigram, r.postingsAt(e.Offset, e.Length)
}
