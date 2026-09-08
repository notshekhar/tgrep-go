package compat

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// goBinary builds the port's CLI once per test run and returns its path.
//
// Built rather than invoked through the library, because the whole point of
// these tests is the part the library does not cover: flag parsing, output
// formatting, and exit codes.
var goBinary = sync.OnceValues(func() (string, error) {
	dir, err := os.MkdirTemp("", "tgrep-go-bin-")
	if err != nil {
		return "", err
	}
	bin := filepath.Join(dir, "tgrep-go")
	cmd := exec.Command("go", "build", "-o", bin, "../cmd/tgrep")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", &buildError{output: string(out), err: err}
	}
	return bin, nil
})

type buildError struct {
	output string
	err    error
}

func (e *buildError) Error() string { return e.err.Error() + "\n" + e.output }

func ourBinary(t *testing.T) string {
	t.Helper()
	bin, err := goBinary()
	if err != nil {
		t.Fatalf("building the CLI: %v", err)
	}
	return bin
}

// runCLI executes a binary and returns its stdout lines and exit code.
func runCLI(t *testing.T, bin string, args ...string) (lines []string, code int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	out, err := cmd.Output()
	if exit, ok := err.(*exec.ExitError); ok {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatalf("%s %v: %v", bin, args, err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimRight(line, "\r"); line != "" {
			lines = append(lines, line)
		}
	}
	return lines, code
}

// TestCLIOutputMatchesTheReference compares the two binaries' stdout for the
// formats scripts and editors actually consume.
//
// Lines are compared as a SET rather than in order: the reference emits files
// in its index order, which its nondeterministic walk makes unstable, while
// this port emits them sorted. Sorting both is the only comparison that can
// hold, and the ordering difference is itself deliberate — see the walker.
func TestCLIOutputMatchesTheReference(t *testing.T) {
	ref := referenceBinary(t)
	ours := ourBinary(t)

	root := searchCorpus(t)
	refIdx, ourIdx := t.TempDir(), t.TempDir()

	if _, code := runCLI(t, ref, "index", root, "--index-path", refIdx); code != 0 {
		t.Fatalf("reference index failed with code %d", code)
	}
	if _, code := runCLI(t, ours, "index", root, "--index-path", ourIdx); code != 0 {
		t.Fatalf("our index failed with code %d", code)
	}

	cases := []struct {
		name string
		args []string
	}{
		{"line numbers", []string{"-n", "handler"}},
		{"vimgrep", []string{"--vimgrep", "handler"}},
		{"count", []string{"-c", "handler"}},
		{"files with matches", []string{"-l", "handler"}},
		{"case insensitive", []string{"-n", "-i", "handler"}},
		{"word boundary", []string{"-n", "-w", "handler"}},
		{"fixed string", []string{"-n", "-F", "handler("}},
		{"whole line", []string{"-n", "-x", "handler with no trailing newline"}},
		{"context", []string{"-n", "-C", "1", "handler"}},
		{"after context", []string{"-n", "-A", "2", "handler"}},
		{"before context", []string{"-n", "-B", "2", "handler"}},
		{"max count", []string{"-n", "-m", "1", "handler"}},
		{"regex", []string{"-n", "handler[0-9]"}},
		{"no matches", []string{"-n", "zzznotpresent"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			refArgs := append(append([]string{}, tc.args...), root, "--index-path", refIdx)
			ourArgs := append(append([]string{}, tc.args...), root, "--index-path", ourIdx)

			refLines, refCode := runCLI(t, ref, refArgs...)
			ourLines, ourCode := runCLI(t, ours, ourArgs...)

			// The separator between context blocks depends on file ordering,
			// which the two do not share, so it is dropped from both sides.
			refLines = withoutSeparators(refLines)
			ourLines = withoutSeparators(ourLines)
			slices.Sort(refLines)
			slices.Sort(ourLines)

			if refCode != ourCode {
				t.Errorf("exit code: ours %d, reference %d", ourCode, refCode)
			}
			missing, extra := diff(ourLines, refLines)
			if len(missing) > 0 {
				t.Errorf("the reference printed these and we did not:\n  %s", strings.Join(first(missing, 10), "\n  "))
			}
			if len(extra) > 0 {
				t.Errorf("we printed these and the reference did not:\n  %s", strings.Join(first(extra, 10), "\n  "))
			}
		})
	}
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

// TestExitCodesFollowGrep pins the convention scripts depend on.
func TestExitCodesFollowGrep(t *testing.T) {
	ours := ourBinary(t)
	root := searchCorpus(t)
	idx := t.TempDir()
	runCLI(t, ours, "index", root, "--index-path", idx)

	if _, code := runCLI(t, ours, "handler", root, "--index-path", idx); code != 0 {
		t.Errorf("a successful match exited %d, want 0", code)
	}
	if _, code := runCLI(t, ours, "zzznotpresent", root, "--index-path", idx); code != 1 {
		t.Errorf("no match exited %d, want 1", code)
	}
	if _, code := runCLI(t, ours, `(a)\1`, root, "--index-path", idx); code != 2 {
		t.Errorf("an invalid pattern exited %d, want 2", code)
	}
}

// TestFlagsAfterPositionalsAreParsed guards a defect that made an index appear
// in the wrong place.
//
// Go's flag package stops at the first non-flag argument, so `tgrep pat . -n`
// silently dropped every flag after the pattern — including --index-path,
// which meant the index was written to the default location while the command
// claimed otherwise. Nothing errored; the flag was simply ignored.
func TestFlagsAfterPositionalsAreParsed(t *testing.T) {
	ours := ourBinary(t)
	root := searchCorpus(t)
	idx := t.TempDir()

	// --index-path AFTER the positional must be honoured: if it were dropped,
	// the index would land in root/.tgrep instead.
	if _, code := runCLI(t, ours, "index", root, "--index-path", idx); code != 0 {
		t.Fatalf("index failed with code %d", code)
	}
	if _, err := os.Stat(filepath.Join(idx, "lookup.bin")); err != nil {
		t.Errorf("--index-path after the positional was ignored: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".tgrep")); err == nil {
		t.Error("an index was created at the default path despite --index-path")
	}

	// The same for a search flag written last.
	lines, _ := runCLI(t, ours, "handler", root, "--index-path", idx, "-n")
	for _, line := range lines {
		// With -n honoured every row carries path:line:text, so the field
		// after the first colon is a number.
		parts := strings.SplitN(line, ":", 3)
		if len(parts) < 3 {
			t.Errorf("-n after the positional was ignored: %q", line)
			break
		}
	}
}

// TestVersionAndHelp covers the two commands that must never need an index.
func TestVersionAndHelp(t *testing.T) {
	ours := ourBinary(t)
	if lines, code := runCLI(t, ours, "--version"); code != 0 || len(lines) == 0 {
		t.Errorf("--version exited %d with %d lines", code, len(lines))
	}
	if lines, code := runCLI(t, ours, "--help"); code != 0 || len(lines) == 0 {
		t.Errorf("--help exited %d with %d lines", code, len(lines))
	}
}

// TestSearchWithoutAnIndexStillWorks covers the fallback path: a search with
// no index must still return the right answers, slowly, rather than fail.
func TestSearchWithoutAnIndexStillWorks(t *testing.T) {
	ours := ourBinary(t)
	root := searchCorpus(t)
	missing := filepath.Join(t.TempDir(), "does-not-exist")

	lines, code := runCLI(t, ours, "-n", "handler", root, "--index-path", missing)
	if code != 0 {
		t.Fatalf("exited %d, want 0", code)
	}
	if len(lines) == 0 {
		t.Fatal("no matches without an index")
	}

	// It must agree with the indexed search.
	idx := t.TempDir()
	runCLI(t, ours, "index", root, "--index-path", idx)
	indexed, _ := runCLI(t, ours, "-n", "handler", root, "--index-path", idx)

	slices.Sort(lines)
	slices.Sort(indexed)
	if !slices.Equal(lines, indexed) {
		t.Errorf("unindexed search differs from indexed:\n  unindexed: %v\n  indexed:   %v", lines, indexed)
	}
}
