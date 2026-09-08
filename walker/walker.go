// Package walker enumerates the files an index or a search should cover.
//
// It answers one question — which paths are admitted — and it must answer it
// the same way every time it is asked. Two properties matter more than speed:
//
//   - The path SET must match what the reference implementation admits, or the
//     two tools disagree about what is searchable and a query silently returns
//     fewer results.
//   - The path ORDER must be deterministic. File ids are assigned by position,
//     so a varying order makes the index files vary run to run. The reference's
//     parallel walker does vary — the same unchanged tree produced three
//     different orders across five runs — which makes its indexes
//     unreproducible and undiffable. This walker sorts every directory's
//     entries instead, so the same tree always yields the same index bytes.
//
// The cost of sorting is one sort per directory, paid once per walk, against a
// property that makes every downstream comparison possible.
package walker

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/notshekhar/tgrep-go/encoding"
	"github.com/notshekhar/tgrep-go/filetypes"
	"github.com/notshekhar/tgrep-go/gitignore"
	"github.com/notshekhar/tgrep-go/trigram"
)

// DefaultMaxFileSize is the size above which a file is neither indexed nor
// searched. It is a deliberate departure from ripgrep, which has no such
// limit: a 2 GB file costs more to trigram than it can ever repay.
const DefaultMaxFileSize int64 = 64 << 20

// binarySniff is how much of a file is read to classify it. A NUL byte in the
// first 8KB is what "binary" means here.
const binarySniff = 8192

// Options tune a walk. The zero value is the default walk: no hidden files,
// ignore rules respected, binaries rejected, 64 MiB cap.
type Options struct {
	// IncludeHidden admits dot-prefixed files and directories.
	IncludeHidden bool
	// NoIgnore disables .gitignore and .ignore entirely.
	NoIgnore bool
	// NoRequireGit applies gitignore rules outside a git repository.
	//
	// Rules are normally gated on finding a .git, which is what ripgrep does
	// and what tgrep matches. On a Perforce or Source Depot enlistment, or a
	// plain exported directory, that makes a repo-root .gitignore inert and
	// the only symptom is an index far larger than expected.
	NoRequireGit bool
	// SearchBinary admits binary files instead of classifying them out.
	SearchBinary bool
	// FollowLinks follows symbolic links while walking.
	FollowLinks bool
	// MaxFileSize rejects larger files. Zero means DefaultMaxFileSize; a
	// negative value means no limit.
	MaxFileSize int64
	// ExcludeDirs are directory names never descended into.
	ExcludeDirs []string
	// MaxDepth limits how far below the root the walk descends. Zero means
	// no limit.
	MaxDepth int
	// CollectIgnoreFiles records the ignore files the walk encountered, so a
	// caller that needs a matcher can build one without repeating the walk.
	CollectIgnoreFiles bool
}

func (o Options) maxFileSize() int64 {
	switch {
	case o.MaxFileSize == 0:
		return DefaultMaxFileSize
	case o.MaxFileSize < 0:
		return 0
	default:
		return o.MaxFileSize
	}
}

// Result is what a walk found. Every path is relative to the root and uses
// forward slashes; each list is sorted.
type Result struct {
	// Files are the text files to index, in the order their ids are assigned.
	Files []string
	// BinaryFiles were admitted by the ignore rules but hold binary content.
	// They are listable but not searchable, and go to the index's sidecar.
	BinaryFiles []string
	// TooLarge exceeded the size cap, and are listable for the same reason.
	TooLarge []string
	// Unreadable could not be opened or stated.
	Unreadable []string
	// IgnoreFiles are the ignore files seen, when CollectIgnoreFiles was set.
	IgnoreFiles []string
	// GitignoreGateHint is set when a .gitignore exists but the git gate
	// silently disabled it — the case whose only other symptom is an index
	// far larger than expected.
	GitignoreGateHint string
}

// Walk enumerates the files under root.
//
// Errors on individual entries are collected into Result.Unreadable rather
// than aborting: one unreadable directory in a large tree must not cost the
// whole index.
func Walk(root string, opts Options) (*Result, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if resolved, err := filepath.EvalSymlinks(absRoot); err == nil {
		absRoot = resolved
	}

	res := &Result{}
	maxSize := opts.maxFileSize()
	excluded := make(map[string]struct{}, len(opts.ExcludeDirs))
	for _, d := range opts.ExcludeDirs {
		excluded[d] = struct{}{}
	}

	// Whether ignore rules apply at all.
	applyIgnores := !opts.NoIgnore && (opts.NoRequireGit || gitignore.InGitRepo(absRoot))
	if !opts.NoIgnore && !applyIgnores {
		if _, err := os.Stat(filepath.Join(absRoot, gitignore.GitignoreName)); err == nil {
			res.GitignoreGateHint = absRoot + " has a .gitignore but is not a git repository, so it is not applied " +
				"(this matches ripgrep). Use NoRequireGit to apply it."
		}
	}

	// Rules written above the root still govern it.
	var ignoreFiles []*gitignore.File
	if applyIgnores {
		ignoreFiles = gitignore.AncestorFiles(absRoot, opts.NoRequireGit)
	}

	// walk descends one directory. inherited carries the ignore files that
	// govern it, so a nested .gitignore adds to its parents' rules rather than
	// replacing them.
	var walk func(absDir, relDir string, depth int, inherited []*gitignore.File)
	walk = func(absDir, relDir string, depth int, inherited []*gitignore.File) {
		if opts.MaxDepth > 0 && depth > opts.MaxDepth {
			return
		}

		entries, err := os.ReadDir(absDir)
		if err != nil {
			res.Unreadable = append(res.Unreadable, relDir)
			return
		}
		// The one thing that makes a walk reproducible.
		slices.SortFunc(entries, func(a, b os.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })

		active := inherited
		if applyIgnores {
			if found := gitignore.FilesIn(absDir, relDir); len(found) > 0 {
				// Copied rather than appended in place: sibling directories
				// share the inherited slice, and appending to it would leak
				// one directory's rules into the next.
				active = make([]*gitignore.File, 0, len(inherited)+len(found))
				active = append(active, inherited...)
				active = append(active, found...)
				if opts.CollectIgnoreFiles {
					for _, f := range found {
						name := gitignore.GitignoreName
						if f.Kind == gitignore.KindIgnore {
							name = gitignore.IgnoreName
						}
						res.IgnoreFiles = append(res.IgnoreFiles, path.Join(relDir, name))
					}
				}
			}
		}
		matcher := gitignore.NewMatcher(active)

		for _, entry := range entries {
			name := entry.Name()
			rel := name
			if relDir != "" {
				rel = relDir + "/" + name
			}

			// .git is skipped only because it is hidden, not by a rule of its
			// own. That means IncludeHidden walks straight into the object
			// store — which is surprising, and is what ripgrep and the
			// reference implementation both do, so the port matches them
			// rather than quietly diverging. A caller that wants a repository
			// indexed with hidden files but without its git internals passes
			// ".git" in ExcludeDirs.
			if !opts.IncludeHidden && strings.HasPrefix(name, ".") {
				continue
			}

			isDir := entry.IsDir()
			// A symlink's target decides what it is, but only when links are
			// followed; otherwise it is skipped entirely rather than indexed
			// as its own bytes.
			if entry.Type()&fs.ModeSymlink != 0 {
				if !opts.FollowLinks {
					continue
				}
				info, err := os.Stat(filepath.Join(absDir, name))
				if err != nil {
					res.Unreadable = append(res.Unreadable, rel)
					continue
				}
				isDir = info.IsDir()
			}

			if isDir {
				if _, skip := excluded[name]; skip {
					continue
				}
				if applyIgnores && matcher.IsIgnored(rel, true) {
					continue
				}
				walk(filepath.Join(absDir, name), rel, depth+1, active)
				continue
			}

			if applyIgnores && matcher.IsIgnored(rel, false) {
				continue
			}

			info, err := entry.Info()
			if err != nil {
				res.Unreadable = append(res.Unreadable, rel)
				continue
			}
			if !info.Mode().IsRegular() {
				// Sockets, devices and fifos have no content to index and
				// would block a reader forever.
				continue
			}
			if maxSize > 0 && info.Size() > maxSize {
				res.TooLarge = append(res.TooLarge, rel)
				continue
			}
			if !opts.SearchBinary {
				if filetypes.IsBinaryExtension(name) {
					res.BinaryFiles = append(res.BinaryFiles, rel)
					continue
				}
				binary, err := sniffBinary(filepath.Join(absDir, name))
				if err != nil {
					res.Unreadable = append(res.Unreadable, rel)
					continue
				}
				if binary {
					res.BinaryFiles = append(res.BinaryFiles, rel)
					continue
				}
			}
			res.Files = append(res.Files, rel)
		}
	}

	info, err := os.Stat(absRoot)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		// A file given directly is admitted without consulting ignore rules:
		// naming a path explicitly is a stronger signal than any rule.
		res.Files = append(res.Files, filepath.Base(absRoot))
		return res, nil
	}
	walk(absRoot, "", 1, ignoreFiles)

	// The recursion already visits in sorted order within each directory, but
	// a parent's files are appended before its subdirectories' — sorting the
	// result makes the whole list ordered, not just each level.
	slices.Sort(res.Files)
	slices.Sort(res.BinaryFiles)
	slices.Sort(res.TooLarge)
	slices.Sort(res.Unreadable)
	slices.Sort(res.IgnoreFiles)
	return res, nil
}

// sniffBinary reports whether a file's first 8KB contain a NUL byte.
//
// A UTF-16 file is exempt. Its ASCII text is interleaved with NULs, so the
// plain test calls it binary and the file becomes unsearchable — silently,
// which is the whole reason this exception exists. Its byte-order mark says
// what it is, and package encoding transcodes it before anything reads it.
func sniffBinary(abs string) (bool, error) {
	f, err := os.Open(abs)
	if err != nil {
		return false, err
	}
	defer f.Close()

	buf := make([]byte, binarySniff)
	n, err := f.Read(buf)
	if err != nil && n == 0 {
		// A genuinely empty file reads zero bytes at EOF and is text.
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		return false, err
	}
	if encoding.IsUTF16(buf[:n]) {
		return false, nil
	}
	return trigram.IsBinary(buf[:n]), nil
}
