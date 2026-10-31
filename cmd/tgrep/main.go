// Command tgrep is a trigram-indexed grep.
//
//	tgrep index .            build the index
//	tgrep serve .            serve searches over TCP
//	tgrep "fn main" .        search
//	tgrep status .           report on a running server
//
// The flag surface mirrors ripgrep's where they overlap, because that is what
// muscle memory and existing scripts expect.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/notshekhar/tgrep-go/index"
	"github.com/notshekhar/tgrep-go/live"
	"github.com/notshekhar/tgrep-go/matching"
	"github.com/notshekhar/tgrep-go/meta"
	"github.com/notshekhar/tgrep-go/output"
	"github.com/notshekhar/tgrep-go/reader"
	"github.com/notshekhar/tgrep-go/search"
	"github.com/notshekhar/tgrep-go/server"
	"github.com/notshekhar/tgrep-go/walker"
)

// Exit codes follow grep's convention, which scripts depend on: 0 found,
// 1 not found, 2 an actual error.
const (
	exitMatch     = 0
	exitNoMatch   = 1
	exitError     = 2
	versionString = "tgrep-go 0.1.0"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage(os.Stderr)
		return exitError
	}

	// A subcommand is only a subcommand in first position; `tgrep index` means
	// build an index, but `tgrep foo index` searches for "foo" under ./index.
	switch args[0] {
	case "search":
		// The reference's explicit form of the default command. Without it,
		// `tgrep search -c foo dir` searched for the word "search".
		return cmdSearch(args[1:])
	case "index":
		return cmdIndex(args[1:])
	case "serve":
		return cmdServe(args[1:])
	case "status":
		return cmdStatus(args[1:])
	case "help", "--help", "-h":
		usage(os.Stdout)
		return exitMatch
	case "version", "--version", "-V":
		fmt.Println(versionString)
		return exitMatch
	}
	return cmdSearch(args)
}

func usage(w *os.File) {
	fmt.Fprint(w, `tgrep — trigram-indexed grep

USAGE:
    tgrep [OPTIONS] <PATTERN> [PATH]
    tgrep index [OPTIONS] [PATH]
    tgrep serve [OPTIONS] [PATH]
    tgrep status [PATH]

SEARCH OPTIONS:
    -i                  case insensitive
    -S                  smart case: insensitive unless the pattern has uppercase
    -F                  treat the pattern as a literal string
    -w                  match whole words only
    -x                  match whole lines only
    -v                  show lines that do NOT match
    -n                  show line numbers
    -c                  show a count of matching lines per file
    -l                  show only the paths of matching files
    -U                  allow matches to span lines
    -A <N> -B <N> -C <N>  lines of context after, before, or both
    -m <N>              stop after N matches per file
    -e <PATTERN>        add a pattern (repeatable)
    --files             list the files that would be searched
    --vimgrep           one row per match: path:line:col:text
    --json              ripgrep-compatible JSON Lines
    --hidden            include hidden files
    --no-ignore         ignore .gitignore and .ignore
    --no-require-git    apply gitignore rules outside a git repository
    --text              search binary files too
    --max-filesize <N>  skip files larger than N bytes
    --index-path <DIR>  where the index lives (default <PATH>/.tgrep)

INDEX OPTIONS:
    --buffer-bytes <N>  memory budget before spilling to disk

SERVE OPTIONS:
    --addr <ADDR>       listen address (default 127.0.0.1:0)
    --no-watch          do not watch for file changes
`)
}

// commonFlags are shared by every subcommand.
type commonFlags struct {
	indexPath    string
	hidden       bool
	noIgnore     bool
	noRequireGit bool
	maxFileSize  int64
	text         bool
}

func (c *commonFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&c.indexPath, "index-path", "", "where the index lives")
	fs.BoolVar(&c.hidden, "hidden", false, "include hidden files")
	fs.BoolVar(&c.noIgnore, "no-ignore", false, "ignore .gitignore and .ignore")
	fs.BoolVar(&c.noRequireGit, "no-require-git", false, "apply gitignore outside a git repository")
	fs.Int64Var(&c.maxFileSize, "max-filesize", 0, "skip files larger than this")
	fs.BoolVar(&c.text, "text", false, "search binary files too")
}

func (c *commonFlags) walkOptions() walker.Options {
	return walker.Options{
		IncludeHidden: c.hidden,
		NoIgnore:      c.noIgnore,
		NoRequireGit:  c.noRequireGit,
		MaxFileSize:   c.maxFileSize,
		SearchBinary:  c.text,
	}
}

// indexDir resolves where the index for a root lives.
func (c *commonFlags) indexDir(root string) string {
	if c.indexPath != "" {
		return c.indexPath
	}
	return index.DefaultIndexDir(root)
}

func fail(format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	return exitError
}

// parseArgs parses flags that may appear anywhere on the command line.
//
// Go's flag package stops at the first non-flag argument, so `tgrep foo . -n`
// would treat "-n" as a path and silently ignore it. Every documented usage
// here puts a positional before the flags people actually type after it, and a
// flag that is quietly dropped is far worse than one that errors — so the
// arguments are reordered, flags first, before flag.Parse sees them.
func parseArgs(fs *flag.FlagSet, args []string) error {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]

		// Everything after "--" is positional by definition, which is how a
		// pattern beginning with a dash is passed.
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if len(arg) < 2 || arg[0] != '-' {
			positional = append(positional, arg)
			continue
		}

		flags = append(flags, arg)
		name := strings.TrimLeft(arg, "-")
		// "-flag=value" carries its own value; anything else needs the next
		// argument, unless the flag is boolean.
		if strings.Contains(name, "=") {
			continue
		}
		if f := fs.Lookup(name); f != nil && !isBoolFlag(f) && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return fs.Parse(append(flags, positional...))
}

// isBoolFlag reports whether a flag stands alone rather than taking a value.
func isBoolFlag(f *flag.Flag) bool {
	bf, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && bf.IsBoolFlag()
}

// resolveRoot turns an optional path argument into a directory to walk and a
// prefix to print.
//
// The two differ, and the difference matters. Filesystem work needs the fully
// resolved path so that an index built through a symlink still names one tree
// — on macOS /var is a link to /private/var, so the same directory has two
// absolute spellings. Output needs the spelling the USER typed, because a
// printed path is something people paste back into a command; rewriting
// /var/... to /private/var/... is technically accurate and practically wrong.
func resolveRoot(args []string) (walkRoot, displayRoot string, err error) {
	displayRoot = "."
	if len(args) > 0 {
		displayRoot = args[0]
	}
	abs, err := filepath.Abs(displayRoot)
	if err != nil {
		return "", "", err
	}
	walkRoot = abs
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		walkRoot = resolved
	}
	return walkRoot, displayRoot, nil
}

func cmdIndex(args []string) int {
	fs := flag.NewFlagSet("index", flag.ContinueOnError)
	var common commonFlags
	common.register(fs)
	bufferBytes := fs.Int("buffer-bytes", 0, "memory budget before spilling")
	if err := parseArgs(fs, args); err != nil {
		return exitError
	}

	root, _, err := resolveRoot(fs.Args())
	if err != nil {
		return fail("%v", err)
	}
	dir := common.indexDir(root)

	fmt.Printf("Walking %s...\n", root)
	started := time.Now()
	walked, err := walker.Walk(root, common.walkOptions())
	if err != nil {
		return fail("walk: %v", err)
	}
	if walked.GitignoreGateHint != "" {
		fmt.Fprintf(os.Stderr, "warning: %s\n", walked.GitignoreGateHint)
	}
	fmt.Printf("Found %d text files (%d binary skipped, %d too large, %d errors)\n",
		len(walked.Files), len(walked.BinaryFiles), len(walked.TooLarge), len(walked.Unreadable))

	outcome, err := index.BuildFromPaths(dir, root, walked.Files, index.Options{
		BufferBytes: *bufferBytes,
		MaxFileSize: common.maxFileSize,
		Complete:    true,
	})
	if err != nil {
		return fail("build: %v", err)
	}

	fmt.Printf("Index built successfully at %s\n", dir)
	fmt.Printf("Indexed in %.1fs (%d trigrams, %d files, %d spill segment(s))\n",
		time.Since(started).Seconds(), outcome.NumTrigrams, outcome.NumFiles, outcome.Segments)
	return exitMatch
}

func cmdServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	var common commonFlags
	common.register(fs)
	addr := fs.String("addr", "127.0.0.1:0", "listen address")
	noWatch := fs.Bool("no-watch", false, "do not watch for file changes")
	if err := parseArgs(fs, args); err != nil {
		return exitError
	}

	root, _, err := resolveRoot(fs.Args())
	if err != nil {
		return fail("%v", err)
	}
	dir := common.indexDir(root)

	idx, err := reader.Open(dir)
	if err != nil {
		return fail("no index at %s — build one with `tgrep index`: %v", dir, err)
	}
	defer idx.Close()

	srv := server.New(root, dir, idx)
	if !*noWatch {
		// Without a watcher the server answers from the index as built, which
		// goes stale the moment anything is edited. Failing to start one is
		// reported but not fatal: a stale-but-serving index beats no server.
		if err := srv.Watch(live.WatchOptions{
			IncludeHidden: common.hidden,
			NoIgnore:      common.noIgnore,
			NoRequireGit:  common.noRequireGit,
			SearchBinary:  common.text,
			MaxFileSize:   common.maxFileSize,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "warning: file watching is off (%v); edits will not be picked up until reload\n", err)
		}
	}
	if err := srv.Listen(*addr); err != nil {
		return fail("listen: %v", err)
	}
	watching := "watching for changes"
	if *noWatch {
		watching = "not watching"
	}
	fmt.Printf("tgrep serving %s on %s (%d files, %d trigrams, %s)\n",
		root, srv.Addr(), idx.NumFiles(), idx.NumTrigrams(), watching)

	// A server that leaves its port file behind sends the next client to a
	// socket nobody is listening on, so the signal handler is not optional.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		fmt.Println("\nshutting down")
		srv.Shutdown()
	}()

	if err := srv.Serve(); err != nil {
		return fail("serve: %v", err)
	}
	return exitMatch
}

func cmdStatus(args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	var common commonFlags
	common.register(fs)
	if err := parseArgs(fs, args); err != nil {
		return exitError
	}

	root, _, err := resolveRoot(fs.Args())
	if err != nil {
		return fail("%v", err)
	}
	dir := common.indexDir(root)

	m, err := meta.Load(dir)
	if err != nil {
		fmt.Printf("no index at %s\n", dir)
		return exitNoMatch
	}
	fmt.Printf("index:     %s\n", dir)
	fmt.Printf("root:      %s\n", m.RootPath)
	fmt.Printf("files:     %d\n", m.NumFiles)
	fmt.Printf("trigrams:  %d\n", m.NumTrigrams)
	fmt.Printf("complete:  %v\n", m.Complete)
	fmt.Printf("built:     %s\n", time.Unix(int64(m.CreatedAt), 0).Format(time.RFC3339))

	if port, err := server.ReadPort(dir); err == nil {
		fmt.Printf("server:    listening on port %s\n", strings.TrimSpace(port))
	} else {
		fmt.Printf("server:    not running\n")
	}
	return exitMatch
}

// searchFlags are the search-specific options.
type searchFlags struct {
	common          commonFlags
	caseInsensitive bool
	smartCase       bool
	fixedString     bool
	wordBoundary    bool
	lineRegexp      bool
	invert          bool
	lineNumbers     bool
	count           bool
	countMatches    bool
	filesWithMatch  bool
	filesOnly       bool
	multiline       bool
	multilineDotAll bool
	vimgrep         bool
	jsonOut         bool
	quiet           bool
	noHeading       bool
	after           int
	before          int
	context         int
	maxCount        int
	patterns        patternList
}

// patternList collects repeated -e flags.
type patternList []string

func (p *patternList) String() string     { return strings.Join(*p, ",") }
func (p *patternList) Set(v string) error { *p = append(*p, v); return nil }

func cmdSearch(args []string) int {
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	var f searchFlags
	f.common.register(fs)
	fs.BoolVar(&f.caseInsensitive, "i", false, "case insensitive")
	fs.BoolVar(&f.smartCase, "S", false, "smart case")
	fs.BoolVar(&f.fixedString, "F", false, "literal string")
	fs.BoolVar(&f.wordBoundary, "w", false, "whole words")
	fs.BoolVar(&f.lineRegexp, "x", false, "whole lines")
	fs.BoolVar(&f.invert, "v", false, "invert")
	fs.BoolVar(&f.lineNumbers, "n", false, "line numbers")
	fs.BoolVar(&f.count, "c", false, "count matching lines")
	fs.BoolVar(&f.countMatches, "count-matches", false, "count matches")
	fs.BoolVar(&f.filesWithMatch, "l", false, "matching file paths only")
	fs.BoolVar(&f.filesOnly, "files", false, "list files that would be searched")
	fs.BoolVar(&f.multiline, "U", false, "matches may span lines")
	fs.BoolVar(&f.multilineDotAll, "multiline-dotall", false, "'.' matches newline")
	fs.BoolVar(&f.vimgrep, "vimgrep", false, "path:line:col:text")
	fs.BoolVar(&f.jsonOut, "json", false, "JSON Lines")
	fs.BoolVar(&f.quiet, "q", false, "print nothing; exit status only")
	fs.BoolVar(&f.noHeading, "no-heading", false, "path on every line")
	fs.IntVar(&f.after, "A", 0, "lines of context after")
	fs.IntVar(&f.before, "B", 0, "lines of context before")
	fs.IntVar(&f.context, "C", 0, "lines of context around")
	fs.IntVar(&f.maxCount, "m", 0, "stop after N matches per file")
	fs.Var(&f.patterns, "e", "add a pattern (repeatable)")
	if err := parseArgs(fs, args); err != nil {
		return exitError
	}

	rest := fs.Args()
	patterns := []string(f.patterns)
	// Which positional is the pattern depends on two things.
	//
	// With -e the pattern came from a flag, so every positional is a path.
	// With --files there is no pattern at all — it lists what would be
	// searched — so again every positional is a path. Getting this wrong is
	// not a parse error: `tgrep --files /some/repo` silently treats the
	// repository as the pattern and lists the current directory instead.
	if len(patterns) == 0 && !f.filesOnly {
		if len(rest) == 0 {
			usage(os.Stderr)
			return exitError
		}
		patterns = append(patterns, rest[0])
		rest = rest[1:]
	}

	root, display, err := resolveRoot(rest)
	if err != nil {
		return fail("%v", err)
	}
	if info, err := os.Stat(root); err == nil && !info.IsDir() && !f.filesOnly {
		return searchOneFile(root, display, patterns, f)
	}
	dir := f.common.indexDir(root)

	if f.filesOnly {
		return listFiles(root, display, f)
	}

	// A flag that changes WHICH FILES are searched cannot be honoured by an
	// index that was built without it: the index knows only the file set it
	// was given. Searching it anyway would silently return fewer results —
	// `--hidden` over an index built without hidden files would find none of
	// them and say nothing. So the walk is redone with the flags in force,
	// which is what the reference implementation does too. It is slow, and
	// that is the honest cost of asking for a different corpus.
	if f.common.hidden || f.common.noIgnore || f.common.noRequireGit || f.common.text {
		return searchWithoutIndex(root, display, patterns, f)
	}

	idx, err := reader.Open(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: no index at %s — scanning every file (slow on large trees)\n", dir)
		fmt.Fprintf(os.Stderr, "note: build one with `tgrep index --index-path %s`\n", dir)
		return searchWithoutIndex(root, display, patterns, f)
	}
	defer idx.Close()

	if f.context > 0 {
		f.before, f.after = f.context, f.context
	}

	opts := searchOptions(patterns, f)
	// An index built by the reference may hold hidden files, recorded in its
	// visibility. --hidden never reaches here (it rewalks above), so they are
	// always filtered out of an indexed search.
	if m, err := meta.Load(dir); err == nil && len(m.Visibility.Hidden) > 0 {
		opts.PathFilter = func(p string) bool { return !m.Visibility.Hides(p) }
	}
	res, err := search.New(root, idx).Run(opts)
	if err != nil {
		return fail("%v", err)
	}
	return emit(res, display, f)
}

func searchOptions(patterns []string, f searchFlags) search.Options {
	return search.Options{
		Patterns: patterns,
		Matcher: matching.Config{
			FixedString:     f.fixedString,
			CaseInsensitive: f.caseInsensitive,
			SmartCase:       f.smartCase,
			WordBoundary:    f.wordBoundary,
			LineRegexp:      f.lineRegexp,
			Multiline:       f.multiline,
			MultilineDotAll: f.multilineDotAll,
		},
		Match: matching.Options{
			InvertMatch:   f.invert,
			BeforeContext: f.before,
			AfterContext:  f.after,
			MaxCount:      f.maxCount,
			// Only the formats that report positions need every span located;
			// a plain search stops at the first match on each line.
			AllSpans: f.vimgrep || f.jsonOut || f.countMatches,
		},
		MaxFileSize:  f.common.maxFileSize,
		SearchBinary: f.common.text,
	}
}

// searchWithoutIndex walks and searches directly, which is what happens when
// no index exists. It is the slow path by definition, and says so.
func searchWithoutIndex(root, display string, patterns []string, f searchFlags) int {
	walked, err := walker.Walk(root, f.common.walkOptions())
	if err != nil {
		return fail("walk: %v", err)
	}
	return scanPaths(root, walked.Files, display, patterns, f, false)
}

// searchOneFile searches a file named on the command line. It is searched
// from its directory — searching from the file itself would look for
// <file>/<file> and find nothing — and, like ripgrep, without an index and
// without repeating its name on every line.
func searchOneFile(file, display string, patterns []string, f searchFlags) int {
	return scanPaths(filepath.Dir(file), []string{filepath.Base(file)}, filepath.Dir(display), patterns, f, true)
}

// scanPaths searches exactly these paths under root through a throwaway index.
func scanPaths(root string, paths []string, display string, patterns []string, f searchFlags, single bool) int {
	dir, err := os.MkdirTemp("", "tgrep-scan-")
	if err != nil {
		return fail("%v", err)
	}
	defer os.RemoveAll(dir)

	if _, err := index.BuildFromPaths(dir, root, paths, index.Options{Complete: true}); err != nil {
		return fail("build: %v", err)
	}
	idx, err := reader.Open(dir)
	if err != nil {
		return fail("%v", err)
	}
	defer idx.Close()

	if f.context > 0 {
		f.before, f.after = f.context, f.context
	}
	res, err := search.New(root, idx).Run(searchOptions(patterns, f))
	if err != nil {
		return fail("%v", err)
	}
	return emitResult(res, display, f, single)
}

func listFiles(root, display string, f searchFlags) int {
	walked, err := walker.Walk(root, f.common.walkOptions())
	if err != nil {
		return fail("walk: %v", err)
	}
	w := output.NewWriter(os.Stdout, output.Options{Format: output.FormatFilesWithMatches})
	defer w.Close()
	for _, group := range [][]string{walked.Files, walked.BinaryFiles, walked.TooLarge} {
		for _, p := range group {
			if err := w.WriteFile(search.FileResult{Path: p, MatchedLines: 1}); err != nil {
				return fail("%v", err)
			}
		}
	}
	return exitMatch
}

func emit(res *search.Result, display string, f searchFlags) int {
	return emitResult(res, display, f, false)
}

func emitResult(res *search.Result, display string, f searchFlags, single bool) int {
	opts := output.Options{
		NoFilename:  single,
		Format:      output.FormatDefault,
		LineNumbers: f.lineNumbers,
		WithContext: f.before > 0 || f.after > 0,
		// Paths are printed the way the user would have to type them, which
		// means relative to where they are, not to the index root.
		PathPrefix: prefixFor(display),
	}
	switch {
	case f.quiet:
		opts.Format = output.FormatQuiet
	case f.jsonOut:
		opts.Format = output.FormatJSON
	case f.vimgrep:
		opts.Format = output.FormatVimgrep
	case f.count:
		opts.Format = output.FormatCount
	case f.countMatches:
		opts.Format = output.FormatCountMatches
	case f.filesWithMatch:
		opts.Format = output.FormatFilesWithMatches
	}

	w := output.NewWriter(os.Stdout, opts)
	if err := w.WriteResult(res); err != nil {
		w.Close()
		return fail("%v", err)
	}
	if err := w.Close(); err != nil {
		return fail("%v", err)
	}

	if res.Stats.MatchedFiles == 0 {
		return exitNoMatch
	}
	return exitMatch
}

// prefixFor renders the root the way the user named it, so output paths can be
// pasted straight back into a command.
//
// "." is the one spelling nobody wants echoed: `tgrep foo .` should print
// src/main.go, not ./src/main.go.
func prefixFor(display string) string {
	if display == "." || display == "" {
		return ""
	}
	return strings.TrimSuffix(display, string(filepath.Separator)) + string(filepath.Separator)
}
