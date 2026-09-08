package index

import (
	"bufio"
	"container/heap"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"

	"github.com/notshekhar/tgrep-go/ondisk"
	"github.com/notshekhar/tgrep-go/trigram"
)

// External merge sort for index construction.
//
// A straightforward builder accumulates one posting for every (trigram, file)
// pair in the repository and sorts the lot at once. That slice is unbounded: a
// monorepo of ~500K files averaging a few thousand distinct trigrams each needs
// well over 10 GB before the first byte is written.
//
// This bounds peak memory to a fixed arena instead:
//
//  1. Postings accumulate in an arena sized by a byte budget.
//  2. When the arena fills it is sorted, delta+varint encoded, spilled to a
//     segment file, and its allocation reused.
//  3. WritePostings streams a k-way merge of the segments straight into
//     index.bin and lookup.bin.
//
// Peak memory is max(arena, merge read buffers) plus the largest single posting
// list, independent of repository size. Total work is unchanged — k sorts of
// n/k elements plus an n·log k merge is still O(n log n), with better cache
// locality per sort — at the cost of one spill write and one merge read.
//
// If the arena never fills, nothing spills and this degrades to exactly the
// in-memory path: one sort, one streaming write. Small repositories pay
// nothing for the default.

const (
	// DefaultBufferBytes is the arena budget before spilling. Large enough
	// that a spill segment amortises its sequential write, small enough that
	// merge fan-in stays modest even on a very large repository.
	DefaultBufferBytes = 64 << 20

	// segmentBufferBytes is the read-ahead held per open segment during the
	// merge, when fan-in is low enough to afford it.
	segmentBufferBytes = 256 << 10

	// minSegmentBufferBytes is the floor on a per-segment read buffer. Only a
	// single varint is ever required for correctness; this just keeps the
	// syscall count sane at high fan-in.
	minSegmentBufferBytes = 4 << 10

	// spillScratchFlushBytes is when the encode buffer is handed to the writer
	// during a spill.
	spillScratchFlushBytes = 128 << 10

	// postingSizeInMemory is the heap cost of one arena posting: a uint32
	// trigram plus a 6-byte posting entry, padded to 12 by alignment.
	postingSizeInMemory = 12
)

// spillSequence distinguishes spill directories created by different sorters
// in this process. Only uniqueness matters.
var spillSequence atomic.Uint64

// Sorter accumulates postings, spilling sorted segments when the arena fills.
type Sorter struct {
	arena    []TrigramPosting
	capacity int // arena length that triggers a spill

	budgetBytes int
	indexDir    string
	spillDir    string
	segments    []string
	scratch     []byte
}

// NewSorter returns a sorter holding at most budgetBytes of postings in memory.
func NewSorter(indexDir string, budgetBytes int) *Sorter {
	// Always allow a small arena, so a pathological budget cannot produce a
	// zero-capacity one that spills on every single posting.
	capacity := max(budgetBytes/postingSizeInMemory, 1024)
	return &Sorter{
		// Allocated once, exactly. Letting append grow it would overshoot the
		// budget — for the 64 MB default it doubles past the target and
		// reserves 96 MB, half again the bound this type exists to enforce.
		arena:       make([]TrigramPosting, 0, capacity),
		capacity:    capacity,
		budgetBytes: budgetBytes,
		indexDir:    indexDir,
	}
}

// SpilledSegments is how many segments have spilled so far, excluding the
// arena tail that WritePostings spills last.
func (s *Sorter) SpilledSegments() int { return len(s.segments) }

// Push appends one posting, spilling first if the arena is full.
func (s *Sorter) Push(p TrigramPosting) error {
	if len(s.arena) >= s.capacity {
		if err := s.spillArena(); err != nil {
			return err
		}
	}
	s.arena = append(s.arena, p)
	return nil
}

// PushFile appends every posting one file contributed.
func (s *Sorter) PushFile(fileID uint32, perTrigram map[uint32]trigram.Masks) error {
	for tri, masks := range perTrigram {
		err := s.Push(TrigramPosting{
			Trigram: tri,
			Entry:   ondisk.PostingEntry{FileID: fileID, LocMask: masks.LocMask, NextMask: masks.NextMask},
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Sorter) sortArena() {
	slices.SortFunc(s.arena, TrigramPosting.Compare)
}

// createSpillDir makes the directory segments are written into.
//
// Segments live next to the index rather than in the system temp directory:
// they can total gigabytes on a large repository, and TEMP is frequently on a
// smaller or differently quota'd volume than the workspace.
//
// The name carries the process id and a per-process sequence. The pid
// separates concurrent processes; the sequence separates concurrent sorters
// within one. Without the latter, two sorters sharing an index directory would
// write the same segment names into it and each would merge whatever the other
// last wrote — silently, since a foreign segment is still a structurally valid
// segment.
func (s *Sorter) createSpillDir() error {
	if s.spillDir != "" {
		return nil
	}
	path := filepath.Join(s.indexDir, fmt.Sprintf("spill-%d-%d.tmp", os.Getpid(), spillSequence.Add(1)-1))
	// A leftover directory from a killed build would make stale segments
	// visible to this run's merge. The pid can be recycled by the OS and the
	// sequence restarts at zero in a new process, so the name is reusable
	// across runs even though it is unique among live sorters.
	os.RemoveAll(path)
	if err := os.MkdirAll(path, 0o755); err != nil {
		return err
	}
	s.spillDir = path
	return nil
}

// Cleanup removes the spill directory. Safe to call more than once, and safe
// to defer immediately after NewSorter.
func (s *Sorter) Cleanup() {
	if s.spillDir != "" {
		os.RemoveAll(s.spillDir)
		s.spillDir = ""
		s.segments = nil
	}
}

func appendVarint(buf []byte, v uint64) []byte {
	for v >= 0x80 {
		buf = append(buf, byte(v)|0x80)
		v >>= 7
	}
	return append(buf, byte(v))
}

// spillArena sorts the arena and writes it out as a compact segment.
//
// The arena's backing allocation is deliberately retained (re-sliced to zero,
// not dropped) so steady-state indexing performs no further large allocations.
func (s *Sorter) spillArena() error {
	if len(s.arena) == 0 {
		return nil
	}
	s.sortArena()
	if err := s.createSpillDir(); err != nil {
		return err
	}

	path := filepath.Join(s.spillDir, fmt.Sprintf("seg-%05d.bin", len(s.segments)))
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(f, segmentBufferBytes)

	// Segment layout, groups ordered by ascending trigram:
	//
	//	varint(trigram - prevTrigram)
	//	varint(postingCount)
	//	postingCount x { varint(fileID - prevFileID), locMask, nextMask }
	//
	// Both deltas are non-negative because the arena is sorted by (trigram,
	// fileID), which makes this substantially smaller than the 6-byte fixed
	// on-disk record — roughly halving the bytes a spill and its merge move.
	s.scratch = s.scratch[:0]
	var prevTrigram uint32
	for start := 0; start < len(s.arena); {
		tri := s.arena[start].Trigram
		end := start + 1
		for end < len(s.arena) && s.arena[end].Trigram == tri {
			end++
		}
		s.scratch = appendVarint(s.scratch, uint64(tri-prevTrigram))
		s.scratch = appendVarint(s.scratch, uint64(end-start))
		prevTrigram = tri

		var prevFileID uint32
		for _, p := range s.arena[start:end] {
			s.scratch = appendVarint(s.scratch, uint64(p.Entry.FileID-prevFileID))
			s.scratch = append(s.scratch, p.Entry.LocMask, p.Entry.NextMask)
			prevFileID = p.Entry.FileID
		}

		if len(s.scratch) >= spillScratchFlushBytes {
			if _, err := w.Write(s.scratch); err != nil {
				f.Close()
				return err
			}
			s.scratch = s.scratch[:0]
		}
		start = end
	}
	if len(s.scratch) > 0 {
		if _, err := w.Write(s.scratch); err != nil {
			f.Close()
			return err
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// Release the encode buffer. It is only needed during a spill, and holding
	// its capacity would count against the caller's budget.
	s.scratch = nil

	s.segments = append(s.segments, path)
	s.arena = s.arena[:0]
	return nil
}

// WritePostings writes index.bin and lookup.bin, returning the number of
// distinct trigrams and how many spill segments were merged.
//
// The sorter is finished afterwards: its arena and every segment are released
// before the caller proceeds to the rest of the index.
func (s *Sorter) WritePostings(indexDir string) (trigrams, segments int, err error) {
	defer s.Cleanup()

	if len(s.segments) == 0 {
		// Never exceeded the budget: identical to the in-memory path.
		s.sortArena()
		arena := s.arena
		s.arena = nil
		n, err := writeSorted(indexDir, arena)
		return n, 0, err
	}

	// Fold the tail into a final segment so the merge has one uniform input.
	if err := s.spillArena(); err != nil {
		return 0, 0, err
	}
	s.arena = nil

	n, err := mergeSegments(indexDir, s.segments, s.budgetBytes)
	return n, len(s.segments), err
}

// segmentReader decodes one spill segment as a stream of posting groups.
type segmentReader struct {
	r           *bufio.Reader
	f           *os.File
	prevTrigram uint32

	// current group, refilled by advance
	trigram uint32
	entries []ondisk.PostingEntry
	done    bool
}

func openSegment(path string, bufBytes int) (*segmentReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return &segmentReader{f: f, r: bufio.NewReaderSize(f, bufBytes)}, nil
}

func (s *segmentReader) close() { s.f.Close() }

func (s *segmentReader) readVarint() (uint64, error) {
	return binary.ReadUvarint(s.r)
}

// advance loads the next group, setting done at clean end of file.
func (s *segmentReader) advance() error {
	delta, err := s.readVarint()
	if err == io.EOF {
		s.done = true
		return nil
	}
	if err != nil {
		return err
	}
	s.trigram = s.prevTrigram + uint32(delta)
	s.prevTrigram = s.trigram

	count, err := s.readVarint()
	if err != nil {
		return fmt.Errorf("segment truncated after trigram %#x: %w", s.trigram, err)
	}
	s.entries = slices.Grow(s.entries[:0], int(count))
	var prevFileID uint32
	for range count {
		d, err := s.readVarint()
		if err != nil {
			return fmt.Errorf("segment truncated in postings of trigram %#x: %w", s.trigram, err)
		}
		fileID := prevFileID + uint32(d)
		prevFileID = fileID
		masks := make([]byte, 2)
		if _, err := io.ReadFull(s.r, masks); err != nil {
			return fmt.Errorf("segment truncated in masks of trigram %#x: %w", s.trigram, err)
		}
		s.entries = append(s.entries, ondisk.PostingEntry{FileID: fileID, LocMask: masks[0], NextMask: masks[1]})
	}
	return nil
}

// segmentHeap orders open segments by the trigram each currently offers, so
// the merge always takes the globally smallest next group.
type segmentHeap []*segmentReader

func (h segmentHeap) Len() int           { return len(h) }
func (h segmentHeap) Less(i, j int) bool { return h[i].trigram < h[j].trigram }
func (h segmentHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *segmentHeap) Push(x any)        { *h = append(*h, x.(*segmentReader)) }
func (h *segmentHeap) Pop() (x any)      { old := *h; n := len(old); x, *h = old[n-1], old[:n-1]; return x }

// mergeSegments k-way merges the spill segments straight into the index.
//
// The read-ahead budget is shared across all open segments rather than given
// to each, so peak memory stays tied to the caller's budget instead of growing
// with fan-in.
func mergeSegments(indexDir string, segments []string, budgetBytes int) (int, error) {
	perSegment := max(min(budgetBytes/max(len(segments), 1), segmentBufferBytes), minSegmentBufferBytes)

	h := make(segmentHeap, 0, len(segments))
	closeAll := func() {
		for _, s := range h {
			s.close()
		}
	}
	for _, path := range segments {
		s, err := openSegment(path, perSegment)
		if err != nil {
			closeAll()
			return 0, err
		}
		if err := s.advance(); err != nil {
			s.close()
			closeAll()
			return 0, err
		}
		if s.done {
			s.close() // an empty segment contributes nothing
			continue
		}
		h = append(h, s)
	}
	heap.Init(&h)
	defer closeAll()

	w, err := newWriter(indexDir)
	if err != nil {
		return 0, err
	}

	var group []ondisk.PostingEntry
	for len(h) > 0 {
		tri := h[0].trigram
		group = group[:0]
		// Drain every segment currently offering this trigram.
		//
		// Each segment's own list already ascends by file id, and for any one
		// trigram a file contributes exactly one posting, so no file id can
		// appear twice in the concatenation. It can still arrive out of order
		// across segments — a spill may split one file's postings from
		// another's — so the concatenation is sorted before it is written.
		for len(h) > 0 && h[0].trigram == tri {
			s := h[0]
			group = append(group, s.entries...)
			if err := s.advance(); err != nil {
				w.abort()
				return 0, err
			}
			if s.done {
				heap.Pop(&h)
				s.close()
			} else {
				heap.Fix(&h, 0)
			}
		}
		slices.SortFunc(group, func(a, b ondisk.PostingEntry) int {
			switch {
			case a.FileID < b.FileID:
				return -1
			case a.FileID > b.FileID:
				return 1
			}
			return 0
		})
		if err := w.writeGroup(tri, group); err != nil {
			w.abort()
			return 0, err
		}
	}

	return w.close()
}
