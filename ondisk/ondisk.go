// Package ondisk defines the binary format of the trigram index.
//
// The layout is byte-identical to the Rust implementation's, deliberately: an
// index written by either can be read by the other, which is what lets the two
// be diffed against each other rather than each trusted on its own tests.
//
// Three files compose the searchable-content index. files-extra.bin, defined
// in package pathindex, complements files.bin with paths that carry no
// searchable content and are listable only by --files.
//
// # lookup.bin — sorted trigram → postings pointer
//
// Fixed 16-byte entries, sorted ascending by trigram, so a lookup is a binary
// search over the mapped file with no in-memory structure at all:
//
//	┌──────────────┬──────────────┬──────────────┐
//	│ trigram      │ offset       │ length       │
//	│ u32 (4B LE)  │ u64 (8B LE)  │ u32 (4B LE)  │
//	└──────────────┴──────────────┴──────────────┘
//
// offset is a byte offset into index.bin; length is a COUNT of posting
// entries, not a byte count.
//
// # index.bin — concatenated posting lists
//
// Each entry is 6 bytes: file_id(u32 LE) + loc_mask(u8) + next_mask(u8).
//
// # files.bin — file ID → path
//
// A versioned header (see filetable.go), then variable-length records:
// file_id(u32 LE) + path_len(u16 LE) + path bytes.
// IDs are dense in 0..N; the reader enforces that, because a corrupt record
// claiming an id near u32 max would otherwise size an allocation from it.
package ondisk

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

const (
	// LookupEntrySize is the fixed width of a lookup.bin record.
	LookupEntrySize = 16 // 4 + 8 + 4
	// PostingEntrySize is the fixed width of an index.bin record.
	PostingEntrySize = 6 // 4 + 1 + 1
	// MaxPathLen is the longest path files.bin can hold, bounded by its u16
	// length prefix.
	MaxPathLen = 0xFFFF
)

// ErrCorrupted marks every malformed-index error, so callers can distinguish
// "this index is broken" from an I/O failure without matching on strings.
var ErrCorrupted = errors.New("index corrupted")

func corruptedf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrCorrupted, fmt.Sprintf(format, args...))
}

// LookupEntry is one record of lookup.bin.
type LookupEntry struct {
	Trigram uint32
	Offset  uint64 // byte offset into index.bin
	Length  uint32 // number of posting entries, not bytes
}

// Encode writes the entry in its on-disk form.
func (e LookupEntry) Encode() [LookupEntrySize]byte {
	var buf [LookupEntrySize]byte
	binary.LittleEndian.PutUint32(buf[0:4], e.Trigram)
	binary.LittleEndian.PutUint64(buf[4:12], e.Offset)
	binary.LittleEndian.PutUint32(buf[12:16], e.Length)
	return buf
}

// DecodeLookupEntry reads one record. buf must be at least LookupEntrySize.
func DecodeLookupEntry(buf []byte) LookupEntry {
	return LookupEntry{
		Trigram: binary.LittleEndian.Uint32(buf[0:4]),
		Offset:  binary.LittleEndian.Uint64(buf[4:12]),
		Length:  binary.LittleEndian.Uint32(buf[12:16]),
	}
}

// PostingEntry is one record of index.bin: a file that contains the trigram,
// plus the masks that let a query reject it without reading it.
type PostingEntry struct {
	FileID uint32
	// LocMask has bit i set if the trigram occurs at an offset where
	// offset%8 == i.
	LocMask uint8
	// NextMask is an 8-bit Bloom filter of the bytes that immediately follow
	// this trigram in the file.
	NextMask uint8
}

// Encode writes the posting in its on-disk form.
func (p PostingEntry) Encode() [PostingEntrySize]byte {
	var buf [PostingEntrySize]byte
	binary.LittleEndian.PutUint32(buf[0:4], p.FileID)
	buf[4] = p.LocMask
	buf[5] = p.NextMask
	return buf
}

// DecodePostingEntry reads one posting. buf must be at least PostingEntrySize.
func DecodePostingEntry(buf []byte) PostingEntry {
	return PostingEntry{
		FileID:   binary.LittleEndian.Uint32(buf[0:4]),
		LocMask:  buf[4],
		NextMask: buf[5],
	}
}

// FileEntry is one record of files.bin.
type FileEntry struct {
	FileID uint32
	Path   string
}

// EncodeFileEntry returns the on-disk bytes for one files.bin record.
//
// A path too long for the u16 prefix is an error rather than a truncation:
// records are read back sequentially, so one short write does not corrupt a
// single entry, it desynchronises every entry after it.
func EncodeFileEntry(fileID uint32, path string) ([]byte, error) {
	if len(path) > MaxPathLen {
		return nil, tooLong(path)
	}
	buf := make([]byte, 0, 6+len(path))
	buf = binary.LittleEndian.AppendUint32(buf, fileID)
	buf = binary.LittleEndian.AppendUint16(buf, uint16(len(path)))
	return append(buf, path...), nil
}

// WriteFileEntry writes one files.bin record without an intermediate buffer.
func WriteFileEntry(w io.Writer, fileID uint32, path string) error {
	if len(path) > MaxPathLen {
		return tooLong(path)
	}
	var hdr [6]byte
	binary.LittleEndian.PutUint32(hdr[0:4], fileID)
	binary.LittleEndian.PutUint16(hdr[4:6], uint16(len(path)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := io.WriteString(w, path)
	return err
}

// tooLong builds the over-length error without embedding a 64KiB path in it —
// that would be both log spam and an echo of untrusted content.
func tooLong(path string) error {
	const preview = 80
	end := 0
	for end < len(path) && end < preview {
		_, size := utf8.DecodeRuneInString(path[end:])
		if end+size > preview {
			break
		}
		end += size
	}
	return corruptedf("path too long for index (%d bytes, max %d): %q…", len(path), MaxPathLen, path[:end])
}

// DecodeFileEntries reads every record of a files.bin body (see FileTableBody).
//
// A truncation is an error rather than a short read: silently accepting a
// partial file table yields queries that return empty paths for high IDs,
// which reads as "no match" instead of as the corruption it is. So is a path
// that fails ValidatePath — a tampered table must not be able to point a
// search outside its root.
func DecodeFileEntries(data []byte) ([]FileEntry, error) {
	var entries []FileEntry
	pos := 0
	for pos < len(data) {
		if pos+6 > len(data) {
			return nil, corruptedf("files.bin truncated: %d trailing bytes < 6-byte header", len(data)-pos)
		}
		fileID := binary.LittleEndian.Uint32(data[pos : pos+4])
		pathLen := int(binary.LittleEndian.Uint16(data[pos+4 : pos+6]))
		pos += 6
		if pos+pathLen > len(data) {
			return nil, corruptedf("files.bin truncated: declared path_len %d exceeds remaining %d bytes",
				pathLen, len(data)-pos)
		}
		path := string(data[pos : pos+pathLen])
		if err := ValidatePath(path); err != nil {
			return nil, corruptedf("files.bin file_id %d: %v: %q", fileID, err, path)
		}
		entries = append(entries, FileEntry{FileID: fileID, Path: path})
		pos += pathLen
	}
	return entries, nil
}
