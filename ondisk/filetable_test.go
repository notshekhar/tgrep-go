package ondisk

import (
	"bytes"
	"encoding/binary"
	"errors"
	"runtime"
	"testing"
)

func fileTable(t *testing.T, versioned bool, paths ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	if versioned {
		if err := WriteFileTableHeader(&buf); err != nil {
			t.Fatal(err)
		}
	}
	for id, p := range paths {
		if err := WriteFileEntry(&buf, uint32(id), p); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

func decodeTable(data []byte) ([]FileEntry, error) {
	body, err := FileTableBody(data)
	if err != nil {
		return nil, err
	}
	return DecodeFileEntries(body)
}

func TestFileTablesRejectUnsafePathsInEveryFormat(t *testing.T) {
	unsafe := []string{
		"", ".", "./file", "..", "../file", "dir/../../file", "/file",
		"dir/./file", "dir//file", "dir/", `\file`, `\\server\share\file`,
		"//server/share/file", `\\?\C:\file`, `dir\file`, "file\x00suffix",
		"bad\xffutf8",
	}
	if runtime.GOOS == "windows" {
		unsafe = append(unsafe, "C:/file", "C:file")
	}
	for _, path := range unsafe {
		for _, versioned := range []bool{false, true} {
			if _, err := decodeTable(fileTable(t, versioned, path)); !errors.Is(err, ErrCorrupted) {
				t.Errorf("accepted %q (versioned=%v): err %v", path, versioned, err)
			}
		}
	}
	for _, path := range []string{"file", "dir/file", ".hidden/x", "a:b", "é/ü.txt"} {
		if runtime.GOOS == "windows" && path == "a:b" {
			continue
		}
		if _, err := decodeTable(fileTable(t, true, path)); err != nil {
			t.Errorf("rejected safe path %q: %v", path, err)
		}
	}
}

func TestFileTableReadsLegacyAndRefusesUnknownVersions(t *testing.T) {
	for _, versioned := range []bool{false, true} {
		entries, err := decodeTable(fileTable(t, versioned, "src/main.go", ".secret"))
		if err != nil || len(entries) != 2 || entries[1].Path != ".secret" {
			t.Errorf("versioned=%v: %v, %v", versioned, entries, err)
		}
	}

	data := fileTable(t, true, "a")
	binary.LittleEndian.PutUint32(data[len(fileTableMagic):], 99)
	if _, err := FileTableBody(data); !errors.Is(err, ErrCorrupted) {
		t.Errorf("version 99 accepted: %v", err)
	}
	if _, err := FileTableBody(data[:len(fileTableMagic)+2]); !errors.Is(err, ErrCorrupted) {
		t.Errorf("truncated header accepted: %v", err)
	}

	// The magic is two records with a duplicate reserved id, so a reader
	// that predates the header cannot mistake a v3 table for a valid legacy
	// one: either it fails to decode, or its dense-id check trips.
	legacy, err := DecodeFileEntries(fileTable(t, true, "a")[:len(fileTableMagic)])
	if err == nil && (len(legacy) < 2 || legacy[0].FileID != legacy[1].FileID) {
		t.Errorf("magic reads as a valid legacy table: %v", legacy)
	}
}
