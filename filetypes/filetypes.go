// Package filetypes decides which files are worth indexing by name alone.
//
// The extension check is the cheap half of binary rejection: it costs no I/O
// at all, where the content sniff costs an open and an 8KB read. On one
// measured checkout 96% of the bytes a naive search touched belonged to
// binaries it then discarded, so rejecting the obvious ones by name first is
// most of the win.
//
// The list is deliberately conservative. A binary type wrongly omitted costs
// one 8KB read before the content sniff catches it; a text type wrongly listed
// makes its files unsearchable, with no symptom beyond results that are never
// returned.
package filetypes

import (
	"path/filepath"
	"strings"
)

// binaryExtensions are formats that can be rejected without reading content.
var binaryExtensions = map[string]struct{}{}

func init() {
	for _, ext := range []string{
		// images
		"png", "jpg", "jpeg", "gif", "bmp", "ico", "webp", "tiff", "tif", "psd",
		// audio and video
		"mp3", "mp4", "avi", "mkv", "mov", "wav", "flac", "ogg", "wma", "aac", "m4a", "webm",
		// archives
		"zip", "tar", "gz", "bz2", "xz", "zst", "7z", "rar", "lz4", "lzma", "cab",
		// compiled objects and libraries
		"exe", "dll", "so", "dylib", "obj", "o", "a", "lib", "pdb", "wasm",
		"class", "jar", "pyc", "pyo", "beam",
		// documents
		"pdf", "doc", "docx", "xls", "xlsx", "ppt", "pptx",
		// fonts
		"ttf", "otf", "woff", "woff2", "eot",
		// opaque data
		"bin", "dat", "db", "sqlite", "sqlite3",
	} {
		binaryExtensions[ext] = struct{}{}
	}
}

// IsBinaryExtension reports whether a path's extension names a binary format.
//
// Exported because the watcher has to apply exactly the rule the walk applied:
// a file the walk rejected here must not be inserted by an incremental update,
// or the index and the walk stop agreeing about what the index contains.
func IsBinaryExtension(path string) bool {
	ext := filepath.Ext(path)
	if ext == "" {
		return false
	}
	_, ok := binaryExtensions[strings.ToLower(ext[1:])]
	return ok
}
