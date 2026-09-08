// Package live is a mutable, in-memory trigram index.
//
// It holds the files that have changed since the on-disk index was built, so a
// running server can answer correctly about an edit made a moment ago without
// rebuilding anything. Everything here is bounded by how much has changed, not
// by how large the repository is.
//
// File ids carry OverlayBit so that one id space can address both layers: a
// caller holding an id always knows which layer to ask, and the merged index
// never has to renumber anything.
package live

import (
	"sort"
	"sync"

	"github.com/notshekhar/tgrep-go/ondisk"
	"github.com/notshekhar/tgrep-go/trigram"
)

// OverlayBit marks a file id as belonging to the overlay rather than the
// on-disk reader.
//
// The top bit is safe to steal: an index addressing more than 2^31 files would
// have exhausted the u32 id space long before, and the builder's dense-id
// invariant means ids are the file count, not a hash.
const OverlayBit uint32 = 1 << 31

// IsOverlayID reports whether an id belongs to the overlay.
func IsOverlayID(id uint32) bool { return id&OverlayBit != 0 }

// maskKey identifies one (trigram, file) pair.
type maskKey struct {
	trigram uint32
	fileID  uint32
}

// Index is the overlay. It is safe for concurrent use.
type Index struct {
	mu sync.RWMutex

	// inverted maps a trigram to the overlay ids containing it.
	inverted map[uint32]map[uint32]struct{}
	// masks holds the per-pair evidence, so the overlay can answer the same
	// mask-filtered queries the on-disk index can.
	masks map[maskKey]trigram.Masks

	filePaths map[uint32]string
	pathToID  map[string]uint32
	// deleted are paths removed since the index was built. They must be
	// remembered rather than simply dropped: the on-disk reader still lists
	// them, and only this set can suppress it.
	deleted map[string]struct{}

	nextID     uint32
	dirtyCount int
}

// New returns an empty overlay.
func New() *Index {
	return &Index{
		inverted:  map[uint32]map[uint32]struct{}{},
		masks:     map[maskKey]trigram.Masks{},
		filePaths: map[uint32]string{},
		pathToID:  map[string]uint32{},
		deleted:   map[string]struct{}{},
	}
}

// Upsert indexes a file's current contents, replacing anything held for it.
func (ix *Index) Upsert(relPath string, content []byte) {
	ix.Commit(relPath, trigram.ExtractMergedMasks(content))
}

// Commit installs already-extracted trigrams for a path.
//
// Separate from Upsert so a caller can do the extraction — the expensive part —
// outside any lock, which is what keeps a burst of file events from serialising
// on the overlay.
func (ix *Index) Commit(relPath string, perTrigram map[uint32]trigram.Masks) {
	ix.mu.Lock()
	defer ix.mu.Unlock()

	ix.removeLocked(relPath)

	id := ix.nextID | OverlayBit
	ix.nextID++
	ix.filePaths[id] = relPath
	ix.pathToID[relPath] = id
	// A path being written again is no longer deleted, however it got here.
	delete(ix.deleted, relPath)

	for tri, masks := range perTrigram {
		set, ok := ix.inverted[tri]
		if !ok {
			set = map[uint32]struct{}{}
			ix.inverted[tri] = set
		}
		set[id] = struct{}{}
		ix.masks[maskKey{tri, id}] = masks
	}
	ix.dirtyCount++
}

// Delete records that a path is gone.
//
// The path is remembered even if the overlay never held it, because the
// on-disk reader probably does and this set is the only thing that can shadow
// it.
func (ix *Index) Delete(relPath string) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	ix.removeLocked(relPath)
	ix.deleted[relPath] = struct{}{}
	ix.dirtyCount++
}

// removeLocked drops everything the overlay holds for a path. The caller holds
// the write lock.
func (ix *Index) removeLocked(relPath string) {
	id, ok := ix.pathToID[relPath]
	if !ok {
		return
	}
	for tri, set := range ix.inverted {
		if _, present := set[id]; !present {
			continue
		}
		delete(set, id)
		delete(ix.masks, maskKey{tri, id})
		// An emptied posting list must go, or the trigram count drifts
		// upwards with every edit and never comes back down.
		if len(set) == 0 {
			delete(ix.inverted, tri)
		}
	}
	delete(ix.filePaths, id)
	delete(ix.pathToID, relPath)
}

// LookupTrigram returns the overlay ids containing a trigram, ascending.
func (ix *Index) LookupTrigram(tri uint32) []uint32 {
	postings := ix.LookupTrigramWithMasks(tri)
	ids := make([]uint32, len(postings))
	for i, p := range postings {
		ids[i] = p.FileID
	}
	return ids
}

// LookupTrigramWithMasks returns the overlay's postings for a trigram.
//
// Sorted by id, because every consumer intersects these lists with the
// reader's using a merge that assumes ascending order.
func (ix *Index) LookupTrigramWithMasks(tri uint32) []ondisk.PostingEntry {
	ix.mu.RLock()
	defer ix.mu.RUnlock()

	set := ix.inverted[tri]
	if len(set) == 0 {
		return nil
	}
	out := make([]ondisk.PostingEntry, 0, len(set))
	for id := range set {
		masks := ix.masks[maskKey{tri, id}]
		out = append(out, ondisk.PostingEntry{FileID: id, LocMask: masks.LocMask, NextMask: masks.NextMask})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FileID < out[j].FileID })
	return out
}

// FilePath returns the path for an overlay id.
func (ix *Index) FilePath(id uint32) (string, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	p, ok := ix.filePaths[id]
	return p, ok
}

// IsDeleted reports whether a path has been removed since the index was built.
func (ix *Index) IsDeleted(path string) bool {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	_, ok := ix.deleted[path]
	return ok
}

// HasPath reports whether the overlay holds a live copy of a path.
func (ix *Index) HasPath(path string) bool {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	_, ok := ix.pathToID[path]
	return ok
}

// NumFiles is how many files the overlay holds.
func (ix *Index) NumFiles() int {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return len(ix.filePaths)
}

// NumTrigrams is how many distinct trigrams the overlay holds.
func (ix *Index) NumTrigrams() int {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return len(ix.inverted)
}

// DirtyCount is how many mutations have landed since the last reset, which is
// what a flush policy watches.
func (ix *Index) DirtyCount() int {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.dirtyCount
}

// ResetDirty clears the mutation counter after a flush.
func (ix *Index) ResetDirty() {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	ix.dirtyCount = 0
}

// AllFileIDs returns every overlay id, ascending.
func (ix *Index) AllFileIDs() []uint32 {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	ids := make([]uint32, 0, len(ix.filePaths))
	for id := range ix.filePaths {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// AllPaths returns every path the overlay holds, sorted.
func (ix *Index) AllPaths() []string {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	paths := make([]string, 0, len(ix.pathToID))
	for p := range ix.pathToID {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}

// DeletedPaths returns the paths recorded as removed, sorted.
func (ix *Index) DeletedPaths() []string {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	paths := make([]string, 0, len(ix.deleted))
	for p := range ix.deleted {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}

// Clear empties the overlay, for use after its contents have been folded into
// a freshly built on-disk index.
func (ix *Index) Clear() {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	ix.inverted = map[uint32]map[uint32]struct{}{}
	ix.masks = map[maskKey]trigram.Masks{}
	ix.filePaths = map[uint32]string{}
	ix.pathToID = map[string]uint32{}
	ix.deleted = map[string]struct{}{}
	ix.dirtyCount = 0
	// nextID is deliberately NOT reset: an id handed out before the clear may
	// still be held by an in-flight query, and reusing it would make that
	// query resolve to the wrong file.
}
