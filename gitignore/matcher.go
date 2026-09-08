package gitignore

import (
	"bufio"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Kind distinguishes the ignore files, because they do not rank equally.
type Kind int

const (
	// KindGitignore is a .gitignore file.
	KindGitignore Kind = iota
	// KindIgnore is a .ignore file, which outranks a .gitignore beside it —
	// it is the file a user writes to override the repository's own rules.
	KindIgnore
	// KindExclude is .git/info/exclude, which behaves like a repo-root
	// .gitignore that is not committed.
	KindExclude
)

// File is one parsed ignore file, anchored at the directory containing it.
type File struct {
	// Dir is the file's directory, relative to the walk root, using forward
	// slashes. The empty string is the root itself.
	Dir string
	// Kind decides precedence against another file at the same depth.
	Kind Kind
	// Patterns are in file order; within one file the last match wins.
	Patterns []Pattern
}

// ParseFile reads and compiles an ignore file. dir is its location relative to
// the walk root.
func ParseFile(absPath, dir string, kind Kind) (*File, error) {
	f, err := os.Open(absPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	file := &File{Dir: dir, Kind: kind}
	scanner := bufio.NewScanner(f)
	// Ignore files are usually tiny, but a generated one can carry very long
	// lines; a pattern longer than this is not a pattern anyone wrote.
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if p, ok := Parse(line); ok {
			file.Patterns = append(file.Patterns, p)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return file, nil
}

// ParseLines compiles patterns from memory, for callers that already hold the
// text (the p4ignore reader, and tests).
func ParseLines(dir string, kind Kind, lines []string) *File {
	file := &File{Dir: dir, Kind: kind}
	for _, line := range lines {
		if p, ok := Parse(line); ok {
			file.Patterns = append(file.Patterns, p)
		}
	}
	return file
}

// depth is how many directories deep this file sits, used for precedence.
func (f *File) depth() int {
	if f.Dir == "" {
		return 0
	}
	return strings.Count(f.Dir, "/") + 1
}

// Matcher answers whether a path is ignored, given a stack of ignore files.
type Matcher struct {
	// files are ordered from weakest to strongest, so a later match overrides
	// an earlier one.
	files []*File
}

// NewMatcher builds a matcher over the given files, ordering them by
// precedence: shallower first, and .gitignore before .ignore at equal depth.
//
// Ordering here rather than at match time means the match itself is a single
// pass in which the last decision simply wins, which is the rule gitignore(5)
// states and the one people expect.
func NewMatcher(files []*File) *Matcher {
	ordered := make([]*File, len(files))
	copy(ordered, files)
	// A stable insertion by (depth, kind) keeps files of equal rank in the
	// order the caller supplied, which is the order they were discovered.
	stableSortFiles(ordered)
	return &Matcher{files: ordered}
}

func stableSortFiles(files []*File) {
	for i := 1; i < len(files); i++ {
		for j := i; j > 0; j-- {
			a, b := files[j-1], files[j]
			if a.depth() < b.depth() || (a.depth() == b.depth() && rank(a.Kind) <= rank(b.Kind)) {
				break
			}
			files[j-1], files[j] = b, a
		}
	}
}

// rank orders the kinds at equal depth, weakest first.
func rank(k Kind) int {
	switch k {
	case KindExclude:
		return 0
	case KindGitignore:
		return 1
	default: // KindIgnore
		return 2
	}
}

// Match reports whether rel is ignored. rel is relative to the walk root and
// uses forward slashes.
//
// Both answers matter to the caller: ignored says what the rules decided, and
// matched says whether any rule spoke at all. A directory that no rule mentions
// is descended into; one that a negation re-included is descended into too,
// and the two must not be confused.
func (m *Matcher) Match(rel string, isDir bool) (ignored, matched bool) {
	for _, file := range m.files {
		// A file's rules only govern its own subtree.
		sub, ok := relativeTo(rel, file.Dir)
		if !ok {
			continue
		}
		for _, p := range file.Patterns {
			if p.Match(sub, isDir) {
				ignored, matched = !p.Negated, true
			}
		}
	}
	return ignored, matched
}

// IsIgnored is Match without the second answer, for callers that only decide.
func (m *Matcher) IsIgnored(rel string, isDir bool) bool {
	ignored, _ := m.Match(rel, isDir)
	return ignored
}

// relativeTo re-expresses rel against dir, reporting false when rel is not
// inside dir at all.
func relativeTo(rel, dir string) (string, bool) {
	if dir == "" {
		return rel, true
	}
	if !strings.HasPrefix(rel, dir+"/") {
		return "", false
	}
	return rel[len(dir)+1:], true
}

// InGitRepo reports whether root is inside a git repository.
//
// This gate exists because gitignore rules only apply inside one — ripgrep
// behaves the same way, and tgrep matches it. The consequence surprises people
// with a Perforce or Source Depot enlistment, or a plain exported directory: a
// repo-root .gitignore is read by nothing, and the only symptom is an index far
// larger than expected. Callers that want the rules anyway pass no-require-git.
func InGitRepo(root string) bool {
	return GitRepoRoot(root) != ""
}

// GitRepoRoot returns the nearest ancestor of root holding a .git entry, or ""
// if there is none.
func GitRepoRoot(root string) string {
	dir, err := filepath.Abs(root)
	if err != nil {
		return ""
	}
	for {
		// .git is a directory in a normal checkout and a file in a worktree
		// or submodule, so existence is the test, not directory-ness.
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// ExcludePath returns the repository's .git/info/exclude, if it has one.
func ExcludePath(root string) string {
	repo := GitRepoRoot(root)
	if repo == "" {
		return ""
	}
	p := filepath.Join(repo, ".git", "info", "exclude")
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

// Names of the two ignore files a walk looks for in every directory.
const (
	GitignoreName = ".gitignore"
	IgnoreName    = ".ignore"
)

// FilesIn returns the ignore files present in one directory, in the order they
// should be added to a matcher.
func FilesIn(absDir, relDir string) []*File {
	var out []*File
	for _, spec := range []struct {
		name string
		kind Kind
	}{
		{GitignoreName, KindGitignore},
		{IgnoreName, KindIgnore},
	} {
		p := filepath.Join(absDir, spec.name)
		if _, err := os.Stat(p); err != nil {
			continue
		}
		// An unreadable or malformed ignore file is skipped rather than
		// fatal: refusing to index a repository because one .gitignore has
		// bad permissions helps nobody.
		if file, err := ParseFile(p, relDir, spec.kind); err == nil {
			out = append(out, file)
		}
	}
	return out
}

// AncestorFiles collects ignore files above root, which still govern it.
//
// A search inside a subdirectory of a repository is still subject to the rules
// written at the repository root, so those files are read even though the walk
// never visits them. Their Dir is expressed relative to root, which for an
// ancestor means an empty string: everything the walk can reach is inside them.
func AncestorFiles(root string, noRequireGit bool) []*File {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil
	}
	repo := GitRepoRoot(abs)
	if repo == "" && !noRequireGit {
		return nil
	}

	// Walk up to the repository root, or to the filesystem root when the git
	// gate has been lifted.
	var dirs []string
	for dir := filepath.Dir(abs); ; dir = filepath.Dir(dir) {
		if repo != "" && !strings.HasPrefix(dir, repo) {
			break
		}
		dirs = append(dirs, dir)
		if dir == repo || filepath.Dir(dir) == dir {
			break
		}
	}

	// Nearest ancestor last, so it outranks the ones above it.
	var out []*File
	for i := len(dirs) - 1; i >= 0; i-- {
		out = append(out, FilesIn(dirs[i], "")...)
	}
	if p := ExcludePath(abs); p != "" {
		if file, err := ParseFile(p, "", KindExclude); err == nil {
			// The exclude file ranks below .gitignore, and NewMatcher orders
			// it there; it is appended rather than prepended so discovery
			// order stays the tiebreak among equals.
			out = append(out, file)
		}
	}
	return out
}

// CleanRel normalises a path for matching: forward slashes, no leading "./".
func CleanRel(rel string) string {
	rel = filepath.ToSlash(rel)
	rel = strings.TrimPrefix(rel, "./")
	return path.Clean(rel)
}
