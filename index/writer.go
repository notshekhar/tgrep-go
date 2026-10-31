// Package index builds the on-disk trigram index.
//
// The write path is deliberately streaming: postings arrive grouped by
// ascending trigram and are appended to index.bin while lookup.bin records
// where each group landed. Nothing is held whole in memory except the group
// currently being written, so the writer's peak cost is one posting list, not
// one index.
package index

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"

	"github.com/notshekhar/tgrep-go/meta"
	"github.com/notshekhar/tgrep-go/ondisk"
)

// TrigramPosting is one (trigram, file) pair before grouping.
type TrigramPosting struct {
	Trigram uint32
	Entry   ondisk.PostingEntry
}

// Compare orders postings the way both the spill segments and the final index
// require: by trigram, then by file id.
//
// The file-id tiebreak is not cosmetic. Posting lists are intersected by a
// merge that walks two lists in step, and the reader's tests assert strict
// ascent; an unordered list would make intersection silently drop matches.
func (p TrigramPosting) Compare(q TrigramPosting) int {
	switch {
	case p.Trigram != q.Trigram:
		if p.Trigram < q.Trigram {
			return -1
		}
		return 1
	case p.Entry.FileID != q.Entry.FileID:
		if p.Entry.FileID < q.Entry.FileID {
			return -1
		}
		return 1
	}
	return 0
}

const (
	// Postings and lookup entries are written in chunks rather than one record
	// at a time: the buffered writer would otherwise take one call per 6 bytes
	// across hundreds of millions of records.
	postingWriteChunkEntries = 8192
	lookupWriteChunkEntries  = 4096
)

// writer streams lookup.bin and index.bin together.
//
// The two files are written in lockstep because a lookup entry cannot be
// formed until its posting list's length is known, and the offset it records
// is simply how many bytes of index.bin came before.
type writer struct {
	postingsFile *os.File
	lookupFile   *os.File
	postings     *bufio.Writer
	lookup       *bufio.Writer

	postingScratch []byte
	lookupScratch  []byte

	offset   uint64 // bytes written to index.bin so far
	trigrams int    // groups written
}

func newWriter(indexDir string) (*writer, error) {
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		return nil, err
	}
	postingsFile, err := os.Create(filepath.Join(indexDir, "index.bin"))
	if err != nil {
		return nil, err
	}
	lookupFile, err := os.Create(filepath.Join(indexDir, "lookup.bin"))
	if err != nil {
		postingsFile.Close()
		return nil, err
	}
	return &writer{
		postingsFile:   postingsFile,
		lookupFile:     lookupFile,
		postings:       bufio.NewWriterSize(postingsFile, 256<<10),
		lookup:         bufio.NewWriterSize(lookupFile, 256<<10),
		postingScratch: make([]byte, 0, postingWriteChunkEntries*ondisk.PostingEntrySize),
		lookupScratch:  make([]byte, 0, lookupWriteChunkEntries*ondisk.LookupEntrySize),
	}, nil
}

// writeGroup appends one trigram's complete posting list.
//
// Groups must arrive in strictly ascending trigram order and each list must
// ascend by file id; the caller (a sort, or a k-way merge) guarantees both.
func (w *writer) writeGroup(trigram uint32, entries []ondisk.PostingEntry) error {
	if len(entries) == 0 {
		// A trigram with no postings must not get a lookup entry at all —
		// the reader treats an entry's presence as proof of a non-empty list.
		return nil
	}

	entry := ondisk.LookupEntry{
		Trigram: trigram,
		Offset:  w.offset,
		Length:  uint32(len(entries)),
	}
	if len(w.lookupScratch)+ondisk.LookupEntrySize > cap(w.lookupScratch) {
		if err := w.flushLookup(); err != nil {
			return err
		}
	}
	encoded := entry.Encode()
	w.lookupScratch = append(w.lookupScratch, encoded[:]...)

	for start := 0; start < len(entries); start += postingWriteChunkEntries {
		end := min(start+postingWriteChunkEntries, len(entries))
		w.postingScratch = w.postingScratch[:0]
		for _, e := range entries[start:end] {
			encoded := e.Encode()
			w.postingScratch = append(w.postingScratch, encoded[:]...)
		}
		if _, err := w.postings.Write(w.postingScratch); err != nil {
			return err
		}
	}

	w.offset += uint64(len(entries)) * ondisk.PostingEntrySize
	w.trigrams++
	return nil
}

func (w *writer) flushLookup() error {
	if len(w.lookupScratch) == 0 {
		return nil
	}
	if _, err := w.lookup.Write(w.lookupScratch); err != nil {
		return err
	}
	w.lookupScratch = w.lookupScratch[:0]
	return nil
}

// close flushes both files to stable storage and returns how many trigram
// groups were written.
//
// Synced, not just flushed: the index is published by renaming its directory
// into place, and a rename can reach the disk before the data it names. A
// crash in that window would leave a complete-looking index of zeroes.
func (w *writer) close() (int, error) {
	if err := w.flushLookup(); err != nil {
		w.abort()
		return 0, err
	}
	for _, f := range []struct {
		buf  *bufio.Writer
		file *os.File
	}{{w.postings, w.postingsFile}, {w.lookup, w.lookupFile}} {
		if err := f.buf.Flush(); err != nil {
			w.abort()
			return 0, err
		}
		if err := f.file.Sync(); err != nil {
			w.abort()
			return 0, err
		}
	}
	if err := w.postingsFile.Close(); err != nil {
		w.lookupFile.Close()
		return 0, err
	}
	if err := w.lookupFile.Close(); err != nil {
		return 0, err
	}
	return w.trigrams, nil
}

// abort closes the descriptors after a failed write, ignoring further errors —
// the first one is the one worth reporting.
func (w *writer) abort() {
	w.postingsFile.Close()
	w.lookupFile.Close()
}

// writeSorted writes a fully sorted posting slice as index.bin + lookup.bin,
// grouping equal trigrams into one list each.
func writeSorted(indexDir string, postings []TrigramPosting) (int, error) {
	w, err := newWriter(indexDir)
	if err != nil {
		return 0, err
	}
	group := make([]ondisk.PostingEntry, 0, 64)
	for start := 0; start < len(postings); {
		trigram := postings[start].Trigram
		end := start + 1
		for end < len(postings) && postings[end].Trigram == trigram {
			end++
		}
		group = group[:0]
		for _, p := range postings[start:end] {
			group = append(group, p.Entry)
		}
		if err := w.writeGroup(trigram, group); err != nil {
			w.abort()
			return 0, err
		}
		start = end
	}
	return w.close()
}

// WriteFilesAndMeta writes files.bin and meta.json.
//
// File ids are assigned by position in paths — id 0 is paths[0] — which is the
// contract every posting already encodes, so the caller must pass exactly the
// order it indexed in.
func WriteFilesAndMeta(indexDir, root string, paths []string, trigramCount int, complete bool) error {
	table, err := encodeFileTable(paths)
	if err != nil {
		return err
	}
	if err := writeSynced(filepath.Join(indexDir, "files.bin"), table); err != nil {
		return err
	}

	// The root is canonicalised so that an index built through a symlink or a
	// relative path still records the one path a later run can compare against.
	canonRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(canonRoot); err == nil {
		canonRoot = resolved
	}

	m := meta.New(canonRoot, uint64(len(paths)), uint64(trigramCount))
	m.Complete = complete
	id := meta.FileTableIDOf(table)
	m.FileTableID = &id
	return m.Save(indexDir)
}

// encodeFileTable renders files.bin in memory. The meta needs its hash, and a
// table is a few bytes per path, so holding it beats reading it back.
func encodeFileTable(paths []string) ([]byte, error) {
	var buf bytes.Buffer
	if err := ondisk.WriteFileTableHeader(&buf); err != nil {
		return nil, err
	}
	for id, path := range paths {
		if err := ondisk.WriteFileEntry(&buf, uint32(id), path); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

func writeSynced(path string, data []byte) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
