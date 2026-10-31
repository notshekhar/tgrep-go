package live

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/notshekhar/tgrep-go/encoding"
	"github.com/notshekhar/tgrep-go/filetypes"
	"github.com/notshekhar/tgrep-go/gitignore"
	"github.com/notshekhar/tgrep-go/trigram"
)

// Watcher keeps an overlay in step with the filesystem.
//
// It exists so a server can answer correctly about a file saved a moment ago.
// Three things make it more than a loop over events:
//
//   - Events are DEBOUNCED. An editor writing a file commonly produces several
//     events in a few milliseconds — write, chmod, rename-into-place — and
//     re-indexing on each one costs several reads of the same bytes.
//   - New directories are watched as they appear. fsnotify is not recursive,
//     so a directory created after startup would otherwise be invisible along
//     with everything ever put in it.
//   - The same admission rules the walk used are applied again. A file the
//     walk excluded must not arrive through the watcher, or the index and the
//     walk stop agreeing about what the index contains.
type Watcher struct {
	root  string
	index *Index
	opts  WatchOptions
	fsw   *fsnotify.Watcher

	// applyIgnores is whether ignore rules are in force at all. Set once
	// before the event loop starts and never written afterwards.
	applyIgnores bool

	// ignoreMu guards the matcher and the files it was built from.
	//
	// They are written by the event loop when a new directory appears and read
	// by the debounce timer when a batch is applied — two goroutines, so the
	// lock is not optional. It is separate from mu because the two protect
	// unrelated state, and sharing one would make every incoming event wait on
	// the length of a flush.
	ignoreMu    sync.RWMutex
	matcher     *gitignore.Matcher
	ignoreFiles []*gitignore.File

	mu      sync.Mutex
	pending map[string]struct{}
	timer   *time.Timer

	stop     chan struct{}
	stopOnce sync.Once
	// OnChange, if set, is called after each debounced batch is applied, with
	// how many paths were upserted and deleted.
	OnChange func(upserted, deleted int)
}

// WatchOptions mirror the walk's admission rules.
type WatchOptions struct {
	IncludeHidden bool
	NoIgnore      bool
	NoRequireGit  bool
	SearchBinary  bool
	MaxFileSize   int64
	// Debounce is how long to wait for a burst to settle. Zero means 100ms,
	// which is long enough to coalesce an editor's save and short enough that
	// a search issued right after one feels immediate.
	Debounce time.Duration
	// ExcludeDirs are directory names never watched.
	ExcludeDirs []string
}

func (o WatchOptions) debounce() time.Duration {
	if o.Debounce <= 0 {
		return 100 * time.Millisecond
	}
	return o.Debounce
}

func (o WatchOptions) maxFileSize() int64 {
	if o.MaxFileSize == 0 {
		return 64 << 20
	}
	return o.MaxFileSize
}

// NewWatcher starts watching root and feeding index.
func NewWatcher(root string, index *Index, opts WatchOptions) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	w := &Watcher{
		root:    root,
		index:   index,
		opts:    opts,
		fsw:     fsw,
		pending: map[string]struct{}{},
		stop:    make(chan struct{}),
	}

	w.applyIgnores = !opts.NoIgnore && (opts.NoRequireGit || gitignore.InGitRepo(root))

	// Rules from above the root still govern it, and are the starting point
	// for the ones found inside.
	var ancestors []*gitignore.File
	if w.applyIgnores {
		ancestors = gitignore.AncestorFiles(root, opts.NoRequireGit)
		w.ignoreFiles = append(w.ignoreFiles, ancestors...)
	}
	// Nothing else runs yet, so this initial build needs no lock.
	w.matcher = gitignore.NewMatcher(ancestors)

	if err := w.addTree(root, "", ancestors); err != nil {
		fsw.Close()
		return nil, err
	}
	// The final matcher carries every ignore file in the tree, so an event
	// arriving for a deeply nested path is judged by the same rules the walk
	// would have applied to it.
	w.rebuildMatcher()

	go w.loop()
	return w, nil
}

// Close stops watching. Safe to call more than once.
func (w *Watcher) Close() error {
	w.stopOnce.Do(func() { close(w.stop) })
	return w.fsw.Close()
}

// addTree registers a directory and everything under it, collecting the ignore
// files it finds on the way down.
//
// Written as an explicit descent rather than filepath.WalkDir because ignore
// rules are hierarchical: a directory's own .gitignore governs its subtree and
// has to be in hand before deciding whether to descend into its children. That
// is the same shape the walk uses, and using a different one here is exactly
// how the watcher and the walk come to disagree.
func (w *Watcher) addTree(absDir, relDir string, inherited []*gitignore.File) error {
	entries, err := os.ReadDir(absDir)
	if err != nil {
		// An unreadable directory is skipped, not fatal: one bad permission
		// must not stop the whole tree being watched.
		return nil //nolint:nilerr // deliberate: keep going
	}

	active := inherited
	if w.applyIgnores {
		if found := gitignore.FilesIn(absDir, relDir); len(found) > 0 {
			// Copied rather than appended in place: sibling directories share
			// the inherited slice, and appending would leak one directory's
			// rules into the next.
			active = make([]*gitignore.File, 0, len(inherited)+len(found))
			active = append(active, inherited...)
			active = append(active, found...)
			w.ignoreMu.Lock()
			w.ignoreFiles = append(w.ignoreFiles, found...)
			w.ignoreMu.Unlock()
		}
	}
	matcher := gitignore.NewMatcher(active)

	w.fsw.Add(absDir)

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		// .git is never watched even with hidden files included: it changes on
		// every git command and none of that churn is searchable content.
		if name == ".git" {
			continue
		}
		if !w.opts.IncludeHidden && strings.HasPrefix(name, ".") {
			continue
		}
		if slices.Contains(w.opts.ExcludeDirs, name) {
			continue
		}
		rel := name
		if relDir != "" {
			rel = relDir + "/" + name
		}
		if w.applyIgnores && matcher.IsIgnored(rel, true) {
			continue
		}
		w.addTree(filepath.Join(absDir, name), rel, active)
	}
	return nil
}

// relative expresses an absolute path against the root, using forward slashes.
func (w *Watcher) relative(abs string) (string, bool) {
	rel, err := filepath.Rel(w.root, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// ignored applies the walk's ignore rules to a path.
func (w *Watcher) ignored(rel string, isDir bool) bool {
	w.ignoreMu.RLock()
	matcher := w.matcher
	w.ignoreMu.RUnlock()
	if matcher == nil {
		return false
	}
	return matcher.IsIgnored(rel, isDir)
}

// rebuildMatcher recompiles the matcher from every ignore file seen so far.
//
// Called after a new directory is registered, since that directory may carry
// rules of its own that have to apply to the files about to arrive from it.
func (w *Watcher) rebuildMatcher() {
	w.ignoreMu.Lock()
	defer w.ignoreMu.Unlock()
	w.matcher = gitignore.NewMatcher(w.ignoreFiles)
}

// snapshotIgnoreFiles copies the collected ignore files, for a descent that
// must not hold the lock while it walks the filesystem.
func (w *Watcher) snapshotIgnoreFiles() []*gitignore.File {
	w.ignoreMu.RLock()
	defer w.ignoreMu.RUnlock()
	return append([]*gitignore.File(nil), w.ignoreFiles...)
}

// loop consumes events until Close.
func (w *Watcher) loop() {
	for {
		select {
		case <-w.stop:
			return
		case event, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			w.note(event)
		case _, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
			// A dropped event is not worth stopping over; the next flush or
			// reload picks the file up.
		}
	}
}

// note records a path and (re)arms the debounce timer.
func (w *Watcher) note(event fsnotify.Event) {
	// An attribute-only change — Spotlight, a backup tool, `touch -a`, an
	// xattr — leaves the bytes alone, so there is nothing to reindex. Queuing
	// it anyway re-arms the debounce on every such event, and a tool that
	// keeps touching attributes would keep the index flushing forever.
	if event.Op == fsnotify.Chmod {
		return
	}
	// A newly created directory has to be watched immediately, and its
	// contents swept: files can be written into it before the Add lands.
	if event.Has(fsnotify.Create) {
		if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
			if rel, ok := w.relative(event.Name); ok {
				w.addTree(event.Name, rel, w.snapshotIgnoreFiles())
				w.rebuildMatcher()
			}
			w.sweep(event.Name)
			return
		}
	}

	w.mu.Lock()
	w.pending[event.Name] = struct{}{}
	if w.timer == nil {
		w.timer = time.AfterFunc(w.opts.debounce(), w.flush)
	} else {
		w.timer.Reset(w.opts.debounce())
	}
	w.mu.Unlock()
}

// sweep queues every file under a directory, for a directory that appeared
// whole (a checkout, a move, an extraction).
func (w *Watcher) sweep(dir string) {
	filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		w.mu.Lock()
		w.pending[path] = struct{}{}
		if w.timer == nil {
			w.timer = time.AfterFunc(w.opts.debounce(), w.flush)
		} else {
			w.timer.Reset(w.opts.debounce())
		}
		w.mu.Unlock()
		return nil
	})
}

// flush applies one debounced batch.
func (w *Watcher) flush() {
	w.mu.Lock()
	batch := w.pending
	w.pending = map[string]struct{}{}
	w.timer = nil
	w.mu.Unlock()

	var upserted, deleted int
	for abs := range batch {
		switch w.apply(abs) {
		case appliedUpsert:
			upserted++
		case appliedDelete:
			deleted++
		}
	}
	if w.OnChange != nil && (upserted > 0 || deleted > 0) {
		w.OnChange(upserted, deleted)
	}
}

type applied int

const (
	appliedNothing applied = iota
	appliedUpsert
	appliedDelete
)

// apply brings the overlay in line with one path's current state on disk.
func (w *Watcher) apply(abs string) applied {
	rel, ok := w.relative(abs)
	if !ok {
		return appliedNothing
	}

	info, err := os.Stat(abs)
	if err != nil {
		// Gone, or unreadable. Either way it must stop being searchable — but
		// only if the index ever knew about it, or every stray event on an
		// ignored file would record a pointless deletion.
		if w.index.HasPath(rel) || !w.index.IsDeleted(rel) {
			w.index.Delete(rel)
			return appliedDelete
		}
		return appliedNothing
	}
	if info.IsDir() {
		return appliedNothing
	}

	// The admission rules the walk applied have to hold here too, or the
	// watcher inserts files the index would never have contained.
	if !w.opts.IncludeHidden && hasHiddenComponent(rel) {
		return appliedNothing
	}
	if w.ignored(rel, false) {
		return appliedNothing
	}
	if limit := w.opts.maxFileSize(); limit > 0 && info.Size() > limit {
		return appliedNothing
	}
	if !info.Mode().IsRegular() {
		return appliedNothing
	}
	if !w.opts.SearchBinary && filetypes.IsBinaryExtension(rel) {
		return appliedNothing
	}

	raw, err := os.ReadFile(abs)
	if err != nil {
		return appliedNothing
	}
	// Decoded exactly as the builder decodes it, or an edited UTF-16 file
	// would enter the overlay as binary and stop matching.
	content, _ := encoding.Decode(raw, encoding.Auto)
	if !w.opts.SearchBinary && trigram.IsBinary(content) {
		return appliedNothing
	}

	// Extraction happens before the lock is taken, which is what keeps a burst
	// of saves from serialising on the overlay.
	w.index.Commit(rel, trigram.ExtractMergedMasks(content))
	return appliedUpsert
}

// hasHiddenComponent reports whether any component of a relative path is
// dot-prefixed, which is how a walk decides a file is hidden.
func hasHiddenComponent(rel string) bool {
	for _, part := range strings.Split(rel, "/") {
		if strings.HasPrefix(part, ".") {
			return true
		}
	}
	return false
}
