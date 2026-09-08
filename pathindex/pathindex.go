// Package pathindex persists the paths that --files can list but the content
// index does not hold.
//
// files.bin already stores every path with trigram postings. This sidecar
// stores only the remaining admitted paths — binary files, binary extensions,
// and files that could not be read while indexing — so both disk and resident
// memory stay proportional to that usually small delta rather than to the
// whole tree twice.
package pathindex

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
)

// Filename is the sidecar's name inside the index directory.
const Filename = "files-extra.bin"

const (
	magic       = "TGRPXP01"
	headerLen   = len(magic) + 8 // magic + u64 count
	pathLenSize = 4              // u32 length prefix per path
)

// ErrCorrupted marks a malformed sidecar.
var ErrCorrupted = errors.New("files-extra.bin corrupted")

func corruptedf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrCorrupted, fmt.Sprintf(format, args...))
}

// Write replaces the sidecar with exactly these paths.
func Write(indexDir string, paths []string) error {
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		return err
	}
	f, err := os.Create(filepath.Join(indexDir, Filename))
	if err != nil {
		return err
	}
	defer f.Close()

	w := bufio.NewWriter(f)
	if _, err := w.WriteString(magic); err != nil {
		return err
	}
	var count [8]byte
	binary.LittleEndian.PutUint64(count[:], uint64(len(paths)))
	if _, err := w.Write(count[:]); err != nil {
		return err
	}
	for _, path := range paths {
		if len(path) > math.MaxUint32 {
			return corruptedf("path is too long (%d bytes)", len(path))
		}
		var n [pathLenSize]byte
		binary.LittleEndian.PutUint32(n[:], uint32(len(path)))
		if _, err := w.Write(n[:]); err != nil {
			return err
		}
		if _, err := w.WriteString(path); err != nil {
			return err
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	return f.Close()
}

// Read returns the sidecar's paths.
//
// found is false for an index written before the sidecar existed. That is not
// an error and must not be treated as an empty set: the caller preserves
// correctness by falling back to a filesystem walk, where an empty set would
// silently drop every binary path from --files.
func Read(indexDir string) (paths []string, found bool, err error) {
	data, err := os.ReadFile(filepath.Join(indexDir, Filename))
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}

	if len(data) < headerLen {
		return nil, false, corruptedf("header is truncated (%d bytes, expected at least %d)", len(data), headerLen)
	}
	if string(data[:len(magic)]) != magic {
		return nil, false, corruptedf("invalid header")
	}

	count := binary.LittleEndian.Uint64(data[len(magic):headerLen])
	// Bound the declared count by what the remaining bytes could possibly
	// hold before sizing anything from it: each record costs at least its
	// 4-byte length prefix, so a corrupt count near u64 max is caught here
	// rather than in an allocation.
	maxRecords := uint64((len(data) - headerLen) / pathLenSize)
	if count > maxRecords {
		return nil, false, corruptedf("declares %d paths but the file can contain at most %d", count, maxRecords)
	}

	paths = make([]string, 0, count)
	pos := headerLen
	for i := uint64(0); i < count; i++ {
		if pos+pathLenSize > len(data) {
			return nil, false, corruptedf("path %d has a truncated length", i)
		}
		n := int(binary.LittleEndian.Uint32(data[pos : pos+pathLenSize]))
		pos += pathLenSize
		if n < 0 || pos+n > len(data) {
			return nil, false, corruptedf("path %d declares %d bytes but only %d remain", i, n, len(data)-pos)
		}
		paths = append(paths, string(data[pos:pos+n]))
		pos += n
	}
	return paths, true, nil
}
