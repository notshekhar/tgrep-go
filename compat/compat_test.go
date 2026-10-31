// Package compat holds the differential tests: everything here reads bytes
// written by the reference Rust implementation and asserts this port agrees
// with them.
//
// These are the tests the byte-compatible format was chosen for. A unit test
// can only confirm the port matches what its author believed the format to be;
// these confirm it matches what the reference actually writes.
//
// testdata/refindex is produced by `tgrep index testdata/fixture`; see
// testdata/README.md for the command.
package compat

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/notshekhar/tgrep-go/meta"
	"github.com/notshekhar/tgrep-go/ondisk"
	"github.com/notshekhar/tgrep-go/pathindex"
	"github.com/notshekhar/tgrep-go/trigram"
)

const (
	refIndex = "../testdata/refindex"
	fixture  = "../testdata/fixture"
)

// skipWithoutRef keeps the suite runnable on a checkout that has not built the
// reference index, rather than failing for a reason unrelated to the code.
func skipWithoutRef(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(refIndex, "lookup.bin")); err != nil {
		t.Skip("no reference index; run: tgrep index testdata/fixture --index-path testdata/refindex")
	}
}

func readRef(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(refIndex, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return data
}

// filesTable decodes files.bin into the id→path table the postings index into.
func filesTable(t *testing.T) []string {
	t.Helper()
	body, err := ondisk.FileTableBody(readRef(t, "files.bin"))
	if err != nil {
		t.Fatalf("files.bin header: %v", err)
	}
	entries, err := ondisk.DecodeFileEntries(body)
	if err != nil {
		t.Fatalf("decode files.bin: %v", err)
	}
	paths := make([]string, len(entries))
	for _, e := range entries {
		if int(e.FileID) >= len(entries) {
			t.Fatalf("file id %d out of range for %d entries", e.FileID, len(entries))
		}
		if paths[e.FileID] != "" {
			t.Fatalf("duplicate file id %d", e.FileID)
		}
		paths[e.FileID] = e.Path
	}
	return paths
}

func TestFilesBinDecodesTheReferenceTable(t *testing.T) {
	skipWithoutRef(t)
	paths := filesTable(t)

	got := append([]string(nil), paths...)
	sort.Strings(got)
	want := []string{"data.txt", "docs/readme.md", "short.txt", "src/main.go", "src/main.rs"}
	if len(got) != len(want) {
		t.Fatalf("got %d paths %v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("path %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestFileEntryRoundTripsTheReferenceBytes(t *testing.T) {
	skipWithoutRef(t)
	data := readRef(t, "files.bin")
	body, err := ondisk.FileTableBody(data)
	if err != nil {
		t.Fatalf("header: %v", err)
	}
	entries, err := ondisk.DecodeFileEntries(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Re-encoding the header and every record must reproduce the reference
	// file exactly — the writer half of the format, checked without a builder.
	var rebuilt bytes.Buffer
	if err := ondisk.WriteFileTableHeader(&rebuilt); err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		encoded, err := ondisk.EncodeFileEntry(e.FileID, e.Path)
		if err != nil {
			t.Fatalf("encode %q: %v", e.Path, err)
		}
		rebuilt.Write(encoded)
	}
	if rebuilt.String() != string(data) {
		t.Errorf("re-encoded files.bin differs from the reference:\n got %x\nwant %x", rebuilt, data)
	}
}

func TestLookupTableIsSortedAndInBounds(t *testing.T) {
	skipWithoutRef(t)
	lookup := readRef(t, "lookup.bin")
	postings := readRef(t, "index.bin")

	if len(lookup)%ondisk.LookupEntrySize != 0 {
		t.Fatalf("lookup.bin size %d is not a multiple of %d", len(lookup), ondisk.LookupEntrySize)
	}
	n := len(lookup) / ondisk.LookupEntrySize

	var prev uint32
	for i := range n {
		e := ondisk.DecodeLookupEntry(lookup[i*ondisk.LookupEntrySize:])
		if i > 0 && e.Trigram <= prev {
			t.Fatalf("entry %d: trigram %#x <= previous %#x — binary search needs strict ascent", i, e.Trigram, prev)
		}
		prev = e.Trigram

		end := e.Offset + uint64(e.Length)*ondisk.PostingEntrySize
		if end > uint64(len(postings)) {
			t.Fatalf("entry %d (trigram %#x): postings [%d,%d) exceed index.bin length %d",
				i, e.Trigram, e.Offset, end, len(postings))
		}
		// Re-encoding the entry must reproduce its bytes.
		encoded := e.Encode()
		if string(encoded[:]) != string(lookup[i*ondisk.LookupEntrySize:(i+1)*ondisk.LookupEntrySize]) {
			t.Fatalf("entry %d does not round-trip", i)
		}
	}
}

// TestPostingsMatchOurOwnExtraction is the real proof of the trigram layer: it
// rebuilds, from the fixture files, exactly what the Rust builder wrote — every
// trigram, every file id, and both masks on every posting.
//
// If the merged-mask extraction, the locMask residue, or the nextMask Bloom
// function differed from the reference in any way, this fails.
func TestPostingsMatchOurOwnExtraction(t *testing.T) {
	skipWithoutRef(t)
	paths := filesTable(t)

	// What we think the index should hold: trigram → fileID → masks.
	want := map[uint32]map[uint32]trigram.Masks{}
	for id, path := range paths {
		content, err := os.ReadFile(filepath.Join(fixture, path))
		if err != nil {
			t.Fatalf("read fixture %s: %v", path, err)
		}
		for h, m := range trigram.ExtractMergedMasks(content) {
			if want[h] == nil {
				want[h] = map[uint32]trigram.Masks{}
			}
			want[h][uint32(id)] = m
		}
	}

	lookup := readRef(t, "lookup.bin")
	postings := readRef(t, "index.bin")
	n := len(lookup) / ondisk.LookupEntrySize

	if n != len(want) {
		t.Errorf("reference holds %d trigrams, our extraction produced %d", n, len(want))
	}

	for i := range n {
		e := ondisk.DecodeLookupEntry(lookup[i*ondisk.LookupEntrySize:])
		expected, ok := want[e.Trigram]
		if !ok {
			t.Errorf("reference has trigram %#x (%q) that our extraction never produced",
				e.Trigram, triBytes(e.Trigram))
			continue
		}
		if int(e.Length) != len(expected) {
			t.Errorf("trigram %#x (%q): reference lists %d files, we expected %d",
				e.Trigram, triBytes(e.Trigram), e.Length, len(expected))
		}
		var prevID int64 = -1
		for j := range int(e.Length) {
			at := e.Offset + uint64(j)*ondisk.PostingEntrySize
			p := ondisk.DecodePostingEntry(postings[at:])
			// Posting lists must ascend by file id: the query planner
			// intersects them with a merge that assumes exactly that.
			if int64(p.FileID) <= prevID {
				t.Errorf("trigram %#x: posting %d has file id %d after %d — not ascending",
					e.Trigram, j, p.FileID, prevID)
			}
			prevID = int64(p.FileID)

			ours, ok := expected[p.FileID]
			if !ok {
				t.Errorf("trigram %#x (%q): reference lists file %d (%s), we did not",
					e.Trigram, triBytes(e.Trigram), p.FileID, paths[p.FileID])
				continue
			}
			if p.LocMask != ours.LocMask || p.NextMask != ours.NextMask {
				t.Errorf("trigram %#x (%q) in %s: reference masks loc=%#08b next=%#08b, ours loc=%#08b next=%#08b",
					e.Trigram, triBytes(e.Trigram), paths[p.FileID],
					p.LocMask, p.NextMask, ours.LocMask, ours.NextMask)
			}
			delete(expected, p.FileID)
		}
		for id := range expected {
			t.Errorf("trigram %#x (%q): we expected file %d (%s), reference did not list it",
				e.Trigram, triBytes(e.Trigram), id, paths[id])
		}
		delete(want, e.Trigram)
	}
	for h := range want {
		t.Errorf("our extraction produced trigram %#x (%q) absent from the reference", h, triBytes(h))
	}
}

// triBytes renders a packed trigram as its three characters, for messages.
func triBytes(h uint32) string {
	return string([]byte{byte(h >> 16), byte(h >> 8), byte(h)})
}

func TestMetaMatchesTheReference(t *testing.T) {
	skipWithoutRef(t)
	m, err := meta.Load(refIndex)
	if err != nil {
		t.Fatalf("load meta: %v", err)
	}
	if m.Version != meta.Version {
		t.Errorf("version = %d, want %d", m.Version, meta.Version)
	}
	// The reference binds meta.json to files.bin by its blake3 hash; ours is
	// computed the same way, so the same table must give the same id.
	if want := meta.FileTableIDOf(readRef(t, "files.bin")); m.FileTableID == nil || *m.FileTableID != want {
		t.Errorf("file_table_id = %v, want the blake3 of files.bin %v", m.FileTableID, want)
	}
	if m.NumFiles != 5 {
		t.Errorf("num_files = %d, want 5", m.NumFiles)
	}
	if !m.Complete {
		t.Error("complete = false, want true for a finished build")
	}
	// num_trigrams must agree with the lookup table it describes.
	lookup := readRef(t, "lookup.bin")
	if got := uint64(len(lookup) / ondisk.LookupEntrySize); m.NumTrigrams != got {
		t.Errorf("num_trigrams = %d but lookup.bin holds %d entries", m.NumTrigrams, got)
	}

	// Round-tripping through our writer must reproduce the reference bytes,
	// pretty-printing and field order included.
	dir := t.TempDir()
	if err := m.Save(dir); err != nil {
		t.Fatalf("save: %v", err)
	}
	ours, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(ours) != string(readRef(t, "meta.json")) {
		t.Errorf("our meta.json differs from the reference:\n got %s\nwant %s", ours, readRef(t, "meta.json"))
	}
}

// TestContentIDMatchesTheReference validates the blake3 domain string, which
// is otherwise unverifiable: get one byte of that prefix wrong and every
// identity differs from the reference's while still being self-consistent.
func TestContentIDMatchesTheReference(t *testing.T) {
	skipWithoutRef(t)
	evidence, err := meta.ReadEvidence(refIndex)
	if err != nil {
		t.Fatalf("read filestamps: %v", err)
	}
	if len(evidence.ContentIDs) == 0 {
		t.Fatal("reference filestamps.json carries no content ids to compare against")
	}
	for path, want := range evidence.ContentIDs {
		content, err := os.ReadFile(filepath.Join(fixture, path))
		if err != nil {
			t.Fatalf("read fixture %s: %v", path, err)
		}
		// The fixture is plain ASCII, so the decoded bytes the reference
		// hashed are the bytes on disk.
		if got := meta.ContentIDOf(content); got != want {
			t.Errorf("%s: content id = %s, reference recorded %s", path, got, want)
		}
	}
}

func TestExtraPathsSidecarReads(t *testing.T) {
	skipWithoutRef(t)
	paths, found, err := pathindex.Read(refIndex)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !found {
		t.Fatal("sidecar not found, but the reference wrote one")
	}
	// The fixture holds no binary or unreadable files, so the delta is empty.
	if len(paths) != 0 {
		t.Errorf("got %d extra paths %v, want none", len(paths), paths)
	}

	// The reference wrote the v2 form, with hidden-file visibility this port
	// does not record, so ours is the v1 form — the one the reference still
	// writes for an index without visibility. It must read back the same.
	dir := t.TempDir()
	if err := pathindex.Write(dir, paths); err != nil {
		t.Fatalf("write: %v", err)
	}
	again, found, err := pathindex.Read(dir)
	if err != nil || !found || len(again) != len(paths) {
		t.Errorf("our sidecar read back as %v (found %v, err %v), want %v", again, found, err, paths)
	}
}

// TestPostingsAreDenseSixByteRecords guards the one assumption every reader in
// this port makes about index.bin: offsets are exact multiples of the record
// size and the file has no padding between lists.
func TestPostingsAreDenseSixByteRecords(t *testing.T) {
	skipWithoutRef(t)
	lookup := readRef(t, "lookup.bin")
	postings := readRef(t, "index.bin")
	if len(postings)%ondisk.PostingEntrySize != 0 {
		t.Fatalf("index.bin size %d is not a multiple of %d", len(postings), ondisk.PostingEntrySize)
	}

	n := len(lookup) / ondisk.LookupEntrySize
	var covered uint64
	for i := range n {
		e := ondisk.DecodeLookupEntry(lookup[i*ondisk.LookupEntrySize:])
		if e.Offset%ondisk.PostingEntrySize != 0 {
			t.Errorf("entry %d: offset %d is not a record boundary", i, e.Offset)
		}
		if e.Offset != covered {
			t.Errorf("entry %d: offset %d leaves a gap after %d — lists are expected to be contiguous",
				i, e.Offset, covered)
		}
		covered = e.Offset + uint64(e.Length)*ondisk.PostingEntrySize
	}
	if covered != uint64(len(postings)) {
		t.Errorf("lists cover %d bytes but index.bin is %d", covered, len(postings))
	}
}

// TestLookupEntryFieldWidths pins the field layout against the raw bytes, so a
// future edit to the struct cannot quietly change the format.
func TestLookupEntryFieldWidths(t *testing.T) {
	e := ondisk.LookupEntry{Trigram: 0x746865, Offset: 1024, Length: 7}
	encoded := e.Encode()
	if got := binary.LittleEndian.Uint32(encoded[0:4]); got != 0x746865 {
		t.Errorf("trigram field = %#x", got)
	}
	if got := binary.LittleEndian.Uint64(encoded[4:12]); got != 1024 {
		t.Errorf("offset field = %d", got)
	}
	if got := binary.LittleEndian.Uint32(encoded[12:16]); got != 7 {
		t.Errorf("length field = %d", got)
	}
	if back := ondisk.DecodeLookupEntry(encoded[:]); back != e {
		t.Errorf("round trip: got %+v, want %+v", back, e)
	}
}
