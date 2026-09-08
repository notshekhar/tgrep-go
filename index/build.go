package index

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/notshekhar/tgrep-go/meta"
	"github.com/notshekhar/tgrep-go/pathindex"
	"github.com/notshekhar/tgrep-go/trigram"
)

// IndexDirName is where an index lives by default, under the root it covers.
const IndexDirName = ".tgrep"

// DefaultIndexDir is the index directory for a root, when none was given.
func DefaultIndexDir(root string) string { return filepath.Join(root, IndexDirName) }

// DefaultMaxFileSize is the size above which a file is neither indexed nor
// searched. Matching the reference, it can be lifted to index anything.
const DefaultMaxFileSize int64 = 64 << 20

// Strategy selects how postings are accumulated before they are written.
type Strategy int

const (
	// External accumulates postings in a fixed arena that spills sorted
	// segments when full, then merges them into the index. Peak memory is
	// independent of repository size. If the arena never fills this is exactly
	// InMemory, so small repositories pay nothing for it — which is why it is
	// the default.
	External Strategy = iota
	// InMemory accumulates every posting and sorts once. Faster for a small
	// tree, unbounded for a large one.
	InMemory
)

// Options tune a build.
type Options struct {
	// Strategy picks the posting accumulator. The zero value is External.
	Strategy Strategy
	// BufferBytes is the arena budget for External. Zero means
	// DefaultBufferBytes.
	BufferBytes int
	// MaxFileSize skips files larger than this. Zero means
	// DefaultMaxFileSize; a negative value means no limit.
	MaxFileSize int64
	// Complete records whether the index covers the whole tree. A background
	// build that was interrupted sets this false so a later run knows not to
	// trust the index as a complete picture.
	Complete bool
	// Progress, if set, is called once per file as it is read. It may be
	// called from several goroutines at once.
	Progress func(path string)
}

func (o Options) bufferBytes() int {
	if o.BufferBytes <= 0 {
		return DefaultBufferBytes
	}
	return o.BufferBytes
}

func (o Options) maxFileSize() int64 {
	switch {
	case o.MaxFileSize == 0:
		return DefaultMaxFileSize
	case o.MaxFileSize < 0:
		return 0 // no limit
	default:
		return o.MaxFileSize
	}
}

// Outcome reports what a build produced.
type Outcome struct {
	// NumFiles is how many files carry postings in the index.
	NumFiles int
	// NumTrigrams is how many distinct trigrams the index holds.
	NumTrigrams int
	// Segments is how many spill segments the merge consumed. Zero means the
	// arena never filled.
	Segments int
	// SkippedBinary, SkippedLarge and Unreadable count the admitted paths that
	// produced no postings. They are listed in files-extra.bin so --files can
	// still name them.
	SkippedBinary []string
	SkippedLarge  []string
	Unreadable    []string
	// Evidence records what each indexed file looked like when it was read.
	Evidence *meta.Evidence
}

// BuildFromPaths builds an index over exactly these paths, in this order.
//
// The order is the contract: file ids are assigned by position, so a caller
// that wants to reproduce another index's ids byte for byte must present the
// same order that index recorded. Paths are relative to root and use forward
// slashes, which is how they are stored and how a query gets them back.
//
// Reading and trigram extraction run across all cores, since that is where a
// build spends its time; postings funnel through the sorter on one goroutine,
// which is where the ordering guarantee comes from.
func BuildFromPaths(indexDir, root string, paths []string, opts Options) (*Outcome, error) {
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		return nil, err
	}

	sorter := NewSorter(indexDir, opts.bufferBytes())
	defer sorter.Cleanup()

	outcome := &Outcome{Evidence: meta.NewEvidence()}
	maxSize := opts.maxFileSize()

	// One file's extracted trigrams, on its way to the sorter.
	//
	// slot is its position in paths, which orders the drain; it is NOT the
	// file id. Ids are assigned during the drain, densely over the files that
	// actually carry postings, because a file skipped as binary or unreadable
	// must not consume one — the reader rejects a table whose ids are not
	// dense in 0..N, and rightly so.
	type extracted struct {
		slot    uint32
		path    string
		masks   map[uint32]trigram.Masks
		stamp   meta.FileStamp
		content meta.ContentID
		// why is empty when the file was indexed, and otherwise names the
		// reason it produced nothing.
		why string
	}

	workers := min(runtime.NumCPU(), len(paths))
	if workers < 1 {
		workers = 1
	}
	jobs := make(chan uint32, workers*4)
	results := make(chan extracted, workers*4)

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for slot := range jobs {
				path := paths[slot]
				out := extracted{slot: slot, path: path}
				abs := filepath.Join(root, filepath.FromSlash(path))

				info, err := os.Stat(abs)
				if err != nil {
					out.why = "unreadable"
					results <- out
					continue
				}
				out.stamp = meta.StampOf(info)

				if maxSize > 0 && info.Size() > maxSize {
					out.why = "large"
					results <- out
					continue
				}
				content, err := os.ReadFile(abs)
				if err != nil {
					out.why = "unreadable"
					results <- out
					continue
				}
				if opts.Progress != nil {
					opts.Progress(path)
				}
				// The binary check is what keeps a 200 MB executable from
				// contributing a few million meaningless trigrams — and, more
				// to the point, from being searched later.
				if trigram.IsBinary(content) {
					out.why = "binary"
					results <- out
					continue
				}
				out.masks = trigram.ExtractMergedMasks(content)
				out.content = meta.ContentIDOf(content)
				results <- out
			}
		}()
	}

	go func() {
		for slot := range uint32(len(paths)) {
			jobs <- slot
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()

	// Results arrive in completion order, but postings must be pushed in file
	// id order: the arena is sorted by (trigram, fileID) and a spill segment
	// encodes file ids as non-negative deltas, so an id arriving out of order
	// would underflow the encoding. Buffering by slot and draining in order
	// costs one map entry per in-flight file, not one per file in the tree.
	pending := make(map[uint32]extracted, workers*4)
	indexed := make([]string, 0, len(paths))
	var next uint32
	var pushErr error

	drain := func() {
		for {
			e, ok := pending[next]
			if !ok {
				return
			}
			delete(pending, next)
			next++

			switch e.why {
			case "binary":
				outcome.SkippedBinary = append(outcome.SkippedBinary, e.path)
			case "large":
				outcome.SkippedLarge = append(outcome.SkippedLarge, e.path)
			case "unreadable":
				outcome.Unreadable = append(outcome.Unreadable, e.path)
			default:
				// The id is this file's position among the files that carry
				// postings — assigned here, in drain order, so it matches the
				// order files.bin is written in below.
				id := uint32(len(indexed))
				indexed = append(indexed, e.path)
				if pushErr == nil {
					pushErr = sorter.PushFile(id, e.masks)
				}
				contentID := e.content
				outcome.Evidence.Insert(e.path, e.stamp, &contentID)
			}
		}
	}

	for e := range results {
		pending[e.slot] = e
		drain()
	}
	drain()
	if pushErr != nil {
		return nil, pushErr
	}
	if len(pending) != 0 {
		return nil, fmt.Errorf("internal: %d files never drained", len(pending))
	}

	trigrams, segments, err := sorter.WritePostings(indexDir)
	if err != nil {
		return nil, err
	}
	outcome.NumTrigrams = trigrams
	outcome.Segments = segments
	outcome.NumFiles = len(indexed)

	if err := WriteFilesAndMeta(indexDir, root, indexed, trigrams, opts.Complete); err != nil {
		return nil, err
	}

	// Paths admitted by the walk but absent from files.bin still have to be
	// listable by --files, so they go in the sidecar rather than being
	// forgotten.
	extra := make([]string, 0, len(outcome.SkippedBinary)+len(outcome.SkippedLarge)+len(outcome.Unreadable))
	extra = append(extra, outcome.SkippedBinary...)
	extra = append(extra, outcome.SkippedLarge...)
	extra = append(extra, outcome.Unreadable...)
	if err := pathindex.Write(indexDir, extra); err != nil {
		return nil, err
	}
	if err := meta.WriteEvidence(outcome.Evidence, indexDir); err != nil {
		return nil, err
	}

	return outcome, nil
}

// ErrNoPaths is returned when a build is asked to index nothing.
var ErrNoPaths = errors.New("no paths to index")
