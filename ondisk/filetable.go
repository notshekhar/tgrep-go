package ondisk

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"runtime"
	"strings"
	"unicode/utf8"
)

// FileTableVersion is the files.bin format this package writes, and the
// version meta.json records.
//
// Version 3 introduced the header. A table without one is a legacy (v1/v2)
// table and is still read as before.
const FileTableVersion uint32 = 3

// fileTableMagic opens a versioned files.bin.
//
// It is two records with the reserved id 0xFFFFFFFF and an empty path, so a
// reader that predates the header fails its dense, unique-id check instead of
// silently reading the version bytes as a path.
var fileTableMagic = []byte("\xff\xff\xff\xff\x00\x00\xff\xff\xff\xff\x00\x00")

// FileTableHeaderLen is the size of the magic plus the u32 version.
var FileTableHeaderLen = len(fileTableMagic) + 4

// WriteFileTableHeader writes the header every new files.bin starts with.
func WriteFileTableHeader(w io.Writer) error {
	if _, err := w.Write(fileTableMagic); err != nil {
		return err
	}
	return binary.Write(w, binary.LittleEndian, FileTableVersion)
}

// FileTableBody strips the header from a files.bin, accepting a legacy table
// that has none. A header naming any other version is refused: its records
// may not mean what this reader thinks they mean.
func FileTableBody(data []byte) ([]byte, error) {
	if !bytes.HasPrefix(data, fileTableMagic) {
		return data, nil
	}
	if len(data) < FileTableHeaderLen {
		return nil, corruptedf("files.bin header is truncated")
	}
	if v := binary.LittleEndian.Uint32(data[len(fileTableMagic):]); v != FileTableVersion {
		return nil, corruptedf("unsupported files.bin format version %d", v)
	}
	return data[FileTableHeaderLen:], nil
}

// ErrUnsafePath reports a stored path that is not a normalized, root-relative
// path.
var ErrUnsafePath = errors.New("index path must be a normalized root-relative path")

// ValidatePath reports whether path can be stored in, and trusted from, an
// index.
//
// Every stored path is later joined to the search root and opened, so one that
// is absolute, climbs with "..", or is spelled in a way that names a different
// file elsewhere would read outside the root. A backslash is refused on every
// platform: on Unix it is an ordinary file-name byte, so translating it would
// name a different file, and on Windows it would be a second separator. Such
// files stay out of indexes; a scan without one still searches them.
func ValidatePath(path string) error {
	if path == "" || !utf8.ValidString(path) || strings.ContainsAny(path, "\\\x00") {
		return ErrUnsafePath
	}
	if strings.HasPrefix(path, "/") || (runtime.GOOS == "windows" && strings.Contains(path, ":")) {
		return ErrUnsafePath
	}
	for part := range strings.SplitSeq(path, "/") {
		if part == "" || part == "." || part == ".." {
			return ErrUnsafePath
		}
	}
	return nil
}
