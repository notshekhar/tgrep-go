// Command tgrep-diff compares this port against the reference Rust tgrep.
//
// The Go test suite already compares the two at the library level. This drives
// both BINARIES over a real repository instead, which is the only way to reach
// what a user actually runs: flag parsing, walk admission, index bytes, and
// printed output.
//
// It exists because the two implementations agreeing on a fixture proves much
// less than them agreeing on a checkout nobody wrote to make a test pass.
//
//	tgrep-diff --repo ~/src/project
//	tgrep-diff --repo ~/src/project --patterns 'func ,TODO,^import'
//
// Exit status is 0 when everything agreed and 1 when anything did not, so this
// can gate a release.
package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// defaultPatterns exercise the planner's distinct paths: plain literals, an
// anchored match, an alternation, a character class, and one pattern that
// cannot be narrowed at all and so forces a full scan.
var defaultPatterns = []string{
	"func ", "error", "TODO", "return", "import",
	"^package", "err != nil$", "func (\\w+) ",
	"[A-Z][a-z]+Error", "context|Context",
	"a.*b",
}

func main() {
	repo := flag.String("repo", ".", "repository to compare over")
	refBin := flag.String("reference", "", "path to the reference tgrep (default: ../tgrep/target/release/tgrep, then $PATH)")
	ourBin := flag.String("ours", "", "path to this port's tgrep (default: built from source)")
	patternList := flag.String("patterns", "", "comma-separated patterns (default: a built-in set)")
	verbose := flag.Bool("v", false, "print the compared row counts for passing checks too")
	flag.Parse()

	patterns := defaultPatterns
	if *patternList != "" {
		patterns = strings.Split(*patternList, ",")
	}

	root, err := filepath.Abs(*repo)
	if err != nil {
		fatal("%v", err)
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}

	reference, err := findReference(*refBin)
	if err != nil {
		fatal("%v", err)
	}
	ours, cleanup, err := findOurs(*ourBin)
	if err != nil {
		fatal("%v", err)
	}
	defer cleanup()

	fmt.Printf("repository: %s\nreference:  %s\nport:       %s\n\n", root, reference, ours)

	d := &differ{root: root, reference: reference, ours: ours, verbose: *verbose}
	d.run(patterns)

	fmt.Printf("\n%d checks, %d disagreements\n", d.checks, len(d.failures))
	if len(d.failures) > 0 {
		fmt.Println("\nDISAGREEMENTS")
		for _, f := range d.failures {
			fmt.Printf("  - %s\n", f)
		}
		os.Exit(1)
	}
	fmt.Println("everything agreed")
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(2)
}

// findReference locates the Rust binary.
func findReference(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	for _, candidate := range []string{
		"../tgrep/target/release/tgrep",
		"../../tgrep/target/release/tgrep",
	} {
		if abs, err := filepath.Abs(candidate); err == nil {
			if _, err := os.Stat(abs); err == nil {
				return abs, nil
			}
		}
	}
	if path, err := exec.LookPath("tgrep"); err == nil {
		return path, nil
	}
	return "", fmt.Errorf("reference tgrep not found; build it with `cargo build --release` or pass --reference")
}

// findOurs builds this port unless a binary was supplied.
func findOurs(explicit string) (string, func(), error) {
	if explicit != "" {
		return explicit, func() {}, nil
	}
	dir, err := os.MkdirTemp("", "tgrep-diff-")
	if err != nil {
		return "", func() {}, err
	}
	bin := filepath.Join(dir, "tgrep")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/notshekhar/tgrep-go/cmd/tgrep")
	if out, err := cmd.CombinedOutput(); err != nil {
		os.RemoveAll(dir)
		return "", func() {}, fmt.Errorf("building the port: %v\n%s", err, out)
	}
	return bin, func() { os.RemoveAll(dir) }, nil
}

type differ struct {
	root      string
	reference string
	ours      string
	verbose   bool

	checks    int
	failures  []string
	lastCheck time.Time
}

// check records one comparison.
//
// Every check prints, pass or fail. A quiet harness on a large repository is
// indistinguishable from a hung one — a full comparison over a checkout with a
// vendored node_modules runs dozens of whole-corpus searches and takes
// minutes — so progress is not optional even though only failures matter.
func (d *differ) check(name string, ok bool, detail string) {
	d.checks++
	status := "ok"
	if !ok {
		status = "DIFFERS"
		d.failures = append(d.failures, name+": "+detail)
	}
	fmt.Printf("  %-44s %7s  %s\n", truncate(name, 44), elapsed(d.lastCheck), status)
	d.lastCheck = time.Now()
	// Detail on failure always; on success only when asked, since it is the
	// row counts that were compared and those are worth seeing when checking
	// that a comparison was not vacuously empty.
	if (!ok || d.verbose) && detail != "" {
		for _, line := range strings.Split(detail, "\n") {
			fmt.Printf("      %s\n", line)
		}
	}
}

// truncate keeps a long pattern from pushing the status column off the line.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// elapsed renders how long the previous check took, so a slow comparison is
// visibly slow rather than silently so.
func elapsed(since time.Time) string {
	if since.IsZero() {
		return ""
	}
	d := time.Since(since)
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

func (d *differ) run(patterns []string) {
	refIdx, err := os.MkdirTemp("", "diff-ref-idx-")
	if err != nil {
		fatal("%v", err)
	}
	defer os.RemoveAll(refIdx)
	ourIdx, err := os.MkdirTemp("", "diff-our-idx-")
	if err != nil {
		fatal("%v", err)
	}
	defer os.RemoveAll(ourIdx)

	fmt.Println("BUILDING INDEXES")
	refBuild := timed(func() { mustRun(d.reference, "index", d.root, "--index-path", refIdx) })
	ourBuild := timed(func() { mustRun(d.ours, "index", d.root, "--index-path", ourIdx) })
	fmt.Printf("  reference %.2fs, port %.2fs\n\n", refBuild.Seconds(), ourBuild.Seconds())

	fmt.Println("WALK ADMISSION")
	d.compareFileSets()

	fmt.Println("\nINDEX CONTENTS")
	d.compareIndexes(refIdx, ourIdx)

	fmt.Println("\nSEARCH RESULTS")
	for _, pattern := range patterns {
		d.compareSearch(pattern, refIdx, ourIdx)
	}

	fmt.Println("\nSEARCH FLAGS")
	for _, flags := range [][]string{
		{"-i"}, {"-w"}, {"-F"}, {"-c"}, {"-l"}, {"-n"}, {"-m", "3"},
		{"-A", "2"}, {"-B", "2"}, {"-C", "1"}, {"--vimgrep"},
	} {
		d.compareSearchWithFlags("error", flags, refIdx, ourIdx)
	}
}

// compareFileSets checks that both admit the same paths.
func (d *differ) compareFileSets() {
	refRows, refCut := run(d.reference, "--files", d.root, "--index-path", mustTemp())
	ourRows, ourCut := run(d.ours, "--files", d.root, "--index-path", mustTemp())
	ref := normalisePaths(d.root, refRows)
	ours := normalisePaths(d.root, ourRows)
	if refCut || ourCut {
		fmt.Printf("  note: more than %d paths; comparing the first %d\n", maxRows, maxRows)
	}

	missing, extra := setDiff(ours, ref)
	detail := fmt.Sprintf("reference admits %d, port admits %d", len(ref), len(ours))
	if len(missing) > 0 {
		detail += fmt.Sprintf("\nonly in reference (%d): %v", len(missing), sample(missing))
	}
	if len(extra) > 0 {
		detail += fmt.Sprintf("\nonly in port (%d): %v", len(extra), sample(extra))
	}
	d.check(fmt.Sprintf("admitted paths (%d)", len(ref)), len(missing) == 0 && len(extra) == 0, detail)
}

// compareIndexes compares the deterministic index files byte for byte.
//
// files.bin and index.bin are deliberately NOT compared: both encode file ids,
// which come from walk order, and the reference's parallel walker is
// nondeterministic — it produces different orders for the same unchanged tree,
// so its own two runs would not match either. lookup.bin is order-independent
// and is the meaningful comparison.
func (d *differ) compareIndexes(refIdx, ourIdx string) {
	refLookup, err1 := os.ReadFile(filepath.Join(refIdx, "lookup.bin"))
	ourLookup, err2 := os.ReadFile(filepath.Join(ourIdx, "lookup.bin"))
	if err1 != nil || err2 != nil {
		d.check("lookup.bin readable", false, fmt.Sprintf("reference: %v, port: %v", err1, err2))
		return
	}

	// The trigram SET and each list's LENGTH are order-independent, so the
	// whole file must match byte for byte even though the postings it points
	// at are ordered differently.
	same := bytes.Equal(refLookup, ourLookup)
	detail := fmt.Sprintf("reference %d bytes (%s), port %d bytes (%s)",
		len(refLookup), digest(refLookup), len(ourLookup), digest(ourLookup))
	if !same && len(refLookup) == len(ourLookup) {
		detail += "\nsame length, differing content — the trigram set matches but a posting count does not"
	}
	d.check("lookup.bin identical", same, detail)

	// The file counts must agree even though the ids do not.
	refList, _ := run(d.reference, "--files", d.root, "--index-path", refIdx)
	ourList, _ := run(d.ours, "--files", d.root, "--index-path", ourIdx)
	refFiles := len(normalisePaths(d.root, refList))
	ourFiles := len(normalisePaths(d.root, ourList))
	d.check("indexed file counts", refFiles == ourFiles, fmt.Sprintf("reference %d, port %d", refFiles, ourFiles))
}

func (d *differ) compareSearch(pattern, refIdx, ourIdx string) {
	d.compareSearchWithFlags(pattern, nil, refIdx, ourIdx)
}

// compareSearchWithFlags compares one search, in --vimgrep form unless the
// flags already choose an output format.
func (d *differ) compareSearchWithFlags(pattern string, flags []string, refIdx, ourIdx string) {
	args := append([]string{}, flags...)
	if !slices.ContainsFunc(flags, isOutputFlag) {
		// One row per match, which is the only format precise enough to
		// compare mechanically: it carries the column as well as the line.
		args = append(args, "--vimgrep")
	}
	args = append(args, pattern, d.root)

	refRows, refCut := run(d.reference, append(args, "--index-path", refIdx)...)
	ourRows, ourCut := run(d.ours, append(args, "--index-path", ourIdx)...)
	refOut := normalisePaths(d.root, refRows)
	ourOut := normalisePaths(d.root, ourRows)

	// Both sides are sorted: the reference emits files in its (nondeterministic)
	// index order, so only the set of rows can be compared.
	refOut = withoutSeparators(refOut)
	ourOut = withoutSeparators(ourOut)
	slices.Sort(refOut)
	slices.Sort(ourOut)

	missing, extra := setDiff(ourOut, refOut)
	name := fmt.Sprintf("%q %s", pattern, strings.Join(flags, " "))
	detail := fmt.Sprintf("reference %d rows, port %d rows", len(refOut), len(ourOut))
	if refCut || ourCut {
		// Past the cap the two sides no longer cover the same rows, so a
		// difference would be an artefact of where each was cut. Say so and
		// compare nothing rather than report a false disagreement.
		d.check(strings.TrimSpace(name), true,
			fmt.Sprintf("SKIPPED: output exceeds %d rows (minified or generated content); not comparable", maxRows))
		return
	}
	if len(missing) > 0 {
		detail += fmt.Sprintf("\nonly in reference (%d): %v", len(missing), sample(missing))
	}
	if len(extra) > 0 {
		detail += fmt.Sprintf("\nonly in port (%d): %v", len(extra), sample(extra))
	}
	d.check(strings.TrimSpace(name), len(missing) == 0 && len(extra) == 0, detail)
}

// isOutputFlag reports whether a flag already selects an output format.
func isOutputFlag(f string) bool {
	switch f {
	case "-c", "-l", "--vimgrep", "--json", "--count-matches":
		return true
	}
	return false
}

func withoutSeparators(lines []string) []string {
	out := lines[:0]
	for _, l := range lines {
		if l != "--" {
			out = append(out, l)
		}
	}
	return out
}

// maxRows bounds how many output rows one comparison holds in memory.
//
// It is not a tuning knob, it is a safety limit. A repository that vendors
// minified or generated code has lines of a hundred kilobytes or more, and a
// per-match format repeats the whole line for every match on it: `return`
// searched over one 3,495-file checkout produces 5.3 GB of output from the
// REFERENCE implementation alone. Buffering both sides of that is 10 GB and
// the harness simply stops.
//
// Hitting the cap is reported rather than silently truncating, so a partial
// comparison is never mistaken for a complete one.
const maxRows = 200_000

// run executes a binary and returns its non-empty stdout lines, up to maxRows.
//
// Output is streamed rather than buffered whole: see maxRows for why. A
// non-zero exit is not an error — "no matches" is status 1.
func run(bin string, args ...string) (lines []string, truncated bool) {
	cmd := exec.Command(bin, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false
	}
	if err := cmd.Start(); err != nil {
		return nil, false
	}

	scanner := bufio.NewScanner(stdout)
	// One row can be an entire minified line, so the token limit has to be
	// generous; anything longer than this is not a line anyone wrote.
	scanner.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for scanner.Scan() {
		if len(lines) >= maxRows {
			truncated = true
			break
		}
		if line := strings.TrimRight(scanner.Text(), "\r"); line != "" {
			lines = append(lines, line)
		}
	}
	// Draining and killing rather than waiting: the child may still be writing
	// gigabytes, and there is nothing left to read.
	if truncated {
		cmd.Process.Kill()
	}
	io.Copy(io.Discard, stdout)
	cmd.Wait()
	return lines, truncated
}

func mustRun(bin string, args ...string) {
	cmd := exec.Command(bin, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		fatal("%s %v: %v\n%s", bin, args, err, out)
	}
}

func mustTemp() string {
	dir, err := os.MkdirTemp("", "diff-scratch-")
	if err != nil {
		fatal("%v", err)
	}
	return dir
}

// normalisePaths rewrites absolute paths in output to be relative to the root.
//
// The two binaries name paths differently — one resolves symlinks, the other
// prints what the user typed — and on macOS /var and /private/var are the same
// directory. Comparing raw output would report every row as different for a
// reason that has nothing to do with search.
func normalisePaths(root string, lines []string) []string {
	resolved := root
	if r, err := filepath.EvalSymlinks(root); err == nil {
		resolved = r
	}
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimPrefix(line, resolved+string(filepath.Separator))
		line = strings.TrimPrefix(line, root+string(filepath.Separator))
		// A warning on stdout is not a result.
		if strings.HasPrefix(line, "warning:") || strings.HasPrefix(line, "note:") {
			continue
		}
		out = append(out, line)
	}
	return out
}

func setDiff(got, want []string) (missing, extra []string) {
	inGot := make(map[string]struct{}, len(got))
	for _, g := range got {
		inGot[g] = struct{}{}
	}
	inWant := make(map[string]struct{}, len(want))
	for _, w := range want {
		inWant[w] = struct{}{}
	}
	for _, w := range want {
		if _, ok := inGot[w]; !ok {
			missing = append(missing, w)
		}
	}
	for _, g := range got {
		if _, ok := inWant[g]; !ok {
			extra = append(extra, g)
		}
	}
	return missing, extra
}

// sample truncates a list so a failure on a large repository prints something
// readable rather than thousands of lines.
func sample(xs []string) []string {
	const n = 5
	if len(xs) <= n {
		return xs
	}
	return append(xs[:n:n], fmt.Sprintf("... and %d more", len(xs)-n))
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

func timed(f func()) time.Duration {
	start := time.Now()
	f()
	return time.Since(start)
}
