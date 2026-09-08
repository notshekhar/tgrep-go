// Package meta reads and writes the index's JSON sidecars: meta.json, which
// describes the index as a whole, and filestamps.json, which records what each
// indexed path looked like when it was indexed.
package meta

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"lukechampine.com/blake3"
)

const (
	MetaFilename       = "meta.json"
	FilestampsFilename = "filestamps.json"

	// Version is the format version this package writes.
	Version = 2
)

// contentIDDomain separates this hash from every other use of blake3.
//
// It must change whenever decoding, binary classification, or posting
// semantics change in a way that makes an identity recorded by an older index
// unsafe to compare against freshly decoded bytes — otherwise a stale index
// silently claims a file is unchanged when the meaning of "unchanged" moved.
var contentIDDomain = []byte("tgrep/content-id/v1\x00decode_for_index-output\x00binary-and-posting-semantics-v2")

// IndexMeta describes one built index. Field order here is the field order in
// meta.json; keep it as is so the file stays byte-comparable with the Rust
// writer's output.
type IndexMeta struct {
	Version     uint32 `json:"version"`
	NumFiles    uint64 `json:"num_files"`
	NumTrigrams uint64 `json:"num_trigrams"`
	CreatedAt   uint64 `json:"created_at"`
	UpdatedAt   uint64 `json:"updated_at"`
	RootPath    string `json:"root_path"`
	// Complete is false when the server was stopped during background
	// indexing, so the index covers only part of the repo. Absent in older
	// indexes, where it defaults to true — see UnmarshalJSON.
	Complete bool `json:"complete"`
}

// New builds metadata stamped with the current time.
func New(rootPath string, numFiles, numTrigrams uint64) IndexMeta {
	now := uint64(time.Now().Unix())
	return IndexMeta{
		Version:     Version,
		NumFiles:    numFiles,
		NumTrigrams: numTrigrams,
		CreatedAt:   now,
		UpdatedAt:   now,
		RootPath:    rootPath,
		Complete:    true,
	}
}

// UnmarshalJSON decodes metadata, defaulting Complete to true.
//
// The zero value of a Go bool is false, so a straight unmarshal of an index
// written before `complete` existed would report every one of them as partial.
func (m *IndexMeta) UnmarshalJSON(data []byte) error {
	type raw IndexMeta // shed the method, keep the layout
	decoded := raw{Complete: true}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*m = IndexMeta(decoded)
	return nil
}

// Save writes meta.json, pretty-printed the way the Rust writer prints it.
func (m IndexMeta) Save(indexDir string) error {
	encoded, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(indexDir, MetaFilename), encoded, 0o644)
}

// Load reads meta.json.
func Load(indexDir string) (IndexMeta, error) {
	data, err := os.ReadFile(filepath.Join(indexDir, MetaFilename))
	if err != nil {
		return IndexMeta{}, err
	}
	var m IndexMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return IndexMeta{}, fmt.Errorf("%s: %w", MetaFilename, err)
	}
	return m, nil
}

// FileStamp is the cheap change check for one path: if neither the modified
// time nor the size moved, the file is presumed unchanged.
type FileStamp struct {
	MTime uint64 `json:"mtime"`
	Size  uint64 `json:"size"`
}

// StampOf converts filesystem metadata into a stamp.
func StampOf(info fs.FileInfo) FileStamp {
	mtime := info.ModTime().Unix()
	if mtime < 0 {
		mtime = 0
	}
	return FileStamp{MTime: uint64(mtime), Size: uint64(info.Size())}
}

// ContentID identifies the decoded bytes that produced one path's postings.
//
// It exists because a stamp can lie: a file restored from backup, or written
// twice within one filesystem timestamp tick, keeps its mtime and size while
// its content changes. The stamp decides cheaply; this decides correctly.
type ContentID [16]byte

// ContentIDOf hashes the bytes as they were handed to the indexer — after
// decoding, not as they sit on disk.
func ContentIDOf(indexed []byte) ContentID {
	h := blake3.New(32, nil)
	h.Write(contentIDDomain)
	h.Write(indexed)
	var id ContentID
	copy(id[:], h.Sum(nil)[:16])
	return id
}

// String renders the identity as the 32 hex digits filestamps.json carries.
func (c ContentID) String() string { return hex.EncodeToString(c[:]) }

// ParseContentID reads the hex form back. The bool is false for anything that
// is not exactly 32 hex digits, which is treated as "no recorded identity"
// rather than as an error: a garbled entry costs one re-index, not a failure.
func ParseContentID(s string) (ContentID, bool) {
	var id ContentID
	if len(s) != 32 {
		return id, false
	}
	decoded, err := hex.DecodeString(s)
	if err != nil {
		return id, false
	}
	copy(id[:], decoded)
	return id, true
}

// Evidence is what one index generation recorded about each path it indexed.
type Evidence struct {
	Stamps     map[string]FileStamp
	ContentIDs map[string]ContentID
}

// NewEvidence returns an empty, ready-to-use evidence set.
func NewEvidence() *Evidence {
	return &Evidence{Stamps: map[string]FileStamp{}, ContentIDs: map[string]ContentID{}}
}

// Stamp returns the recorded stamp for path.
func (e *Evidence) Stamp(path string) (FileStamp, bool) {
	s, ok := e.Stamps[path]
	return s, ok
}

// ContentID returns the recorded content identity for path, if this generation
// recorded one.
func (e *Evidence) ContentID(path string) (ContentID, bool) {
	id, ok := e.ContentIDs[path]
	return id, ok
}

// Insert records a path. A nil id clears any identity previously held for it,
// so a generation that could not hash the content cannot leave the previous
// generation's identity behind to be trusted.
func (e *Evidence) Insert(path string, stamp FileStamp, id *ContentID) {
	if id != nil {
		e.ContentIDs[path] = *id
	} else {
		delete(e.ContentIDs, path)
	}
	e.Stamps[path] = stamp
}

// Remove drops a path entirely.
func (e *Evidence) Remove(path string) {
	delete(e.ContentIDs, path)
	delete(e.Stamps, path)
}

// evidenceEntry is the JSON shape of one filestamps.json value. The identity
// rides in a one-letter key, and is omitted when absent, so that a reader
// predating it simply ignores a field it does not know.
type evidenceEntry struct {
	MTime uint64 `json:"mtime"`
	Size  uint64 `json:"size"`
	C     string `json:"c,omitempty"`
}

// WriteEvidence writes filestamps.json.
//
// Unlike the binary files this is not byte-comparable with the Rust writer's
// output: that one serialises a HashMap, whose iteration order is randomised
// per process. Only the decoded content is meaningfully equal.
func WriteEvidence(e *Evidence, indexDir string) error {
	out := make(map[string]evidenceEntry, len(e.Stamps))
	for path, stamp := range e.Stamps {
		entry := evidenceEntry{MTime: stamp.MTime, Size: stamp.Size}
		if id, ok := e.ContentIDs[path]; ok {
			entry.C = id.String()
		}
		out[path] = entry
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(indexDir, FilestampsFilename), encoded, 0o644)
}

// ReadEvidence reads filestamps.json. A missing file is an empty set, not an
// error: an index built before the sidecar existed simply re-indexes.
func ReadEvidence(indexDir string) (*Evidence, error) {
	data, err := os.ReadFile(filepath.Join(indexDir, FilestampsFilename))
	if os.IsNotExist(err) {
		return NewEvidence(), nil
	}
	if err != nil {
		return nil, err
	}
	var raw map[string]evidenceEntry
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("%s: %w", FilestampsFilename, err)
	}
	e := NewEvidence()
	for path, entry := range raw {
		e.Stamps[path] = FileStamp{MTime: entry.MTime, Size: entry.Size}
		if id, ok := ParseContentID(entry.C); ok {
			e.ContentIDs[path] = id
		}
	}
	return e, nil
}
