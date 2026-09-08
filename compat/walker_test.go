package compat

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/notshekhar/tgrep-go/walker"
)

// referenceBinary locates the Rust tgrep, which is the oracle for what the
// walk should admit. Without it these tests skip rather than fail: the port
// must remain testable on a machine with no Rust toolchain.
func referenceBinary(t *testing.T) string {
	t.Helper()
	for _, candidate := range []string{
		"../../tgrep/target/release/tgrep",
		"../../tgrep/target/debug/tgrep",
	} {
		if abs, err := filepath.Abs(candidate); err == nil {
			if _, err := os.Stat(abs); err == nil {
				return abs
			}
		}
	}
	if path, err := exec.LookPath("tgrep"); err == nil {
		return path
	}
	t.Skip("reference tgrep not built; run `cargo build --release` in ../tgrep")
	return ""
}

// oracleFiles asks the reference which paths it admits under root.
func oracleFiles(t *testing.T, root string) []string {
	t.Helper()
	bin := referenceBinary(t)
	indexDir := t.TempDir()

	cmd := exec.Command(bin, "--files", root, "--index-path", indexDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("reference --files failed: %v\n%s", err, out)
	}

	var paths []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// The reference prints paths prefixed with the root it was given.
		rel, err := filepath.Rel(root, line)
		if err != nil {
			rel = line
		}
		paths = append(paths, filepath.ToSlash(rel))
	}
	slices.Sort(paths)
	return paths
}

// ourFiles is every path our walk admits, in the same shape the oracle
// reports: text files plus the binaries and oversized files that are listable
// but not searchable.
func ourFiles(t *testing.T, root string, opts walker.Options) []string {
	t.Helper()
	res, err := walker.Walk(root, opts)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	paths := slices.Concat(res.Files, res.BinaryFiles, res.TooLarge)
	slices.Sort(paths)
	return paths
}

// tree writes a directory tree from a path→content map and makes it a git
// repository, since gitignore rules only apply inside one.
func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "init", "--allow-empty"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return root
}

// diff reports what each side has that the other does not.
func diff(got, want []string) (missing, extra []string) {
	for _, w := range want {
		if !slices.Contains(got, w) {
			missing = append(missing, w)
		}
	}
	for _, g := range got {
		if !slices.Contains(want, g) {
			extra = append(extra, g)
		}
	}
	return missing, extra
}

// TestWalkSetMatchesTheReference is the parity that actually matters.
//
// Walk ORDER cannot be compared — the reference's parallel walker is
// nondeterministic, producing different file orders for the same unchanged
// tree — so the assertion is on the SET of admitted paths. That set is stable
// across reference runs and is what decides whether a file is searchable.
func TestWalkSetMatchesTheReference(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
	}{
		{
			name: "plain tree",
			files: map[string]string{
				"a.txt": "alpha", "src/b.go": "beta", "src/deep/c.rs": "gamma",
			},
		},
		{
			name: "basename pattern applies at every depth",
			files: map[string]string{
				".gitignore":     "*.log\n",
				"keep.txt":       "keep",
				"app.log":        "drop",
				"src/nested.log": "drop",
				"src/deep/x.log": "drop",
				"src/keep.txt":   "keep",
			},
		},
		{
			name: "anchored pattern applies only at its own level",
			files: map[string]string{
				".gitignore":      "/build\n",
				"build/out.txt":   "drop",
				"src/build/y.txt": "keep",
			},
		},
		{
			name: "directory-only pattern spares a like-named file",
			files: map[string]string{
				".gitignore":  "cache/\n",
				"cache/x.txt": "drop",
				"cache.txt":   "keep",
			},
		},
		{
			name: "negation re-includes",
			files: map[string]string{
				".gitignore":    "*.log\n!important.log\n",
				"a.log":         "drop",
				"important.log": "keep",
			},
		},
		{
			name: "globstar spans directories",
			files: map[string]string{
				".gitignore":           "docs/**/private\n",
				"docs/private/a.txt":   "drop",
				"docs/x/private/b.txt": "drop",
				"docs/x/public/c.txt":  "keep",
			},
		},
		{
			name: "character class and single-char wildcard",
			files: map[string]string{
				".gitignore": "file[0-9].txt\nq?.dat\n",
				"file1.txt":  "drop",
				"fileA.txt":  "keep",
				"qz.dat":     "drop",
				"qzz.dat":    "keep",
			},
		},
		{
			name: "dotfiles are skipped unless asked for",
			files: map[string]string{
				".hidden":       "hidden",
				".config/x.txt": "hidden",
				"visible.txt":   "visible",
			},
		},
		{
			name: "binary content and binary extensions are still listed",
			files: map[string]string{
				"text.txt": "hello",
				"blob.bin": "whatever",
				"nul.dat":  "has\x00nul",
				"real.png": "\x89PNG\r\n\x1a\n",
			},
		},
		{
			name: "trailing spaces and comments",
			files: map[string]string{
				".gitignore": "# a comment\n\n*.bak   \n",
				"x.bak":      "drop",
				"y.txt":      "keep",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := tree(t, tc.files)
			want := oracleFiles(t, root)
			got := ourFiles(t, root, walker.Options{})

			missing, extra := diff(got, want)
			if len(missing) > 0 {
				t.Errorf("the reference admits these and we do not: %v", missing)
			}
			if len(extra) > 0 {
				t.Errorf("we admit these and the reference does not: %v", extra)
			}
			if t.Failed() {
				t.Logf("ours:      %v", got)
				t.Logf("reference: %v", want)
			}
		})
	}
}

// TestHiddenFilesMatchTheReference checks the flag separately, since the
// default walk hides them and the interesting comparison is with them shown.
func TestHiddenFilesMatchTheReference(t *testing.T) {
	root := tree(t, map[string]string{
		".hidden":       "hidden",
		".config/x.txt": "hidden",
		"visible.txt":   "visible",
	})

	bin := referenceBinary(t)
	cmd := exec.Command(bin, "--files", root, "--hidden", "--index-path", t.TempDir())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("reference: %v\n%s", err, out)
	}
	var want []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			rel, _ := filepath.Rel(root, line)
			want = append(want, filepath.ToSlash(rel))
		}
	}
	slices.Sort(want)

	got := ourFiles(t, root, walker.Options{IncludeHidden: true})
	missing, extra := diff(got, want)
	if len(missing) > 0 || len(extra) > 0 {
		t.Errorf("hidden walk differs\n  ours:      %v\n  reference: %v\n  missing: %v\n  extra: %v",
			got, want, missing, extra)
	}
}

// TestWalkIsDeterministic is the property this walker adds over the
// reference's: the same tree must always produce the same order, because file
// ids are positional and a varying order makes the index bytes vary.
func TestWalkIsDeterministic(t *testing.T) {
	root := tree(t, map[string]string{
		"z.txt": "z", "a.txt": "a", "m/n.txt": "n", "m/a.txt": "a",
		"b/c/d.txt": "d", "b/c/a.txt": "a", "y/x.txt": "x",
	})

	var first []string
	for run := range 8 {
		res, err := walker.Walk(root, walker.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if run == 0 {
			first = res.Files
			continue
		}
		if !slices.Equal(res.Files, first) {
			t.Fatalf("run %d ordered files differently\n  run 0: %v\n  run %d: %v", run, first, run, res.Files)
		}
	}
	if !slices.IsSorted(first) {
		t.Errorf("walk order is not sorted: %v", first)
	}
}

// TestGitGateIsReportedNotSilent covers the failure mode whose only other
// symptom is an index far larger than expected.
func TestGitGateIsReportedNotSilent(t *testing.T) {
	// A tree with a .gitignore but deliberately NOT a git repository.
	root := t.TempDir()
	for name, content := range map[string]string{
		".gitignore": "*.log\n",
		"a.log":      "log",
		"b.txt":      "text",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	res, err := walker.Walk(root, walker.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.GitignoreGateHint == "" {
		t.Error("no hint reported for a .gitignore disabled by the git gate")
	}
	if !slices.Contains(res.Files, "a.log") {
		t.Error("a.log was excluded, but outside a git repository the rule should not apply")
	}

	// With the gate lifted the rule applies.
	res, err = walker.Walk(root, walker.Options{NoRequireGit: true})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(res.Files, "a.log") {
		t.Error("a.log survived NoRequireGit, but the rule should now apply")
	}
}

// TestNoRequireGitMatchesTheReference compares the lifted gate against the
// reference, which has the same flag.
func TestNoRequireGitMatchesTheReference(t *testing.T) {
	root := t.TempDir()
	for name, content := range map[string]string{
		".gitignore":  "*.log\nbuild/\n",
		"a.log":       "log",
		"b.txt":       "text",
		"build/c.txt": "built",
		"src/d.txt":   "source",
	} {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	bin := referenceBinary(t)
	cmd := exec.Command(bin, "--files", root, "--no-require-git", "--index-path", t.TempDir())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("reference: %v\n%s", err, out)
	}
	var want []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "warning:") {
			rel, _ := filepath.Rel(root, line)
			want = append(want, filepath.ToSlash(rel))
		}
	}
	slices.Sort(want)

	got := ourFiles(t, root, walker.Options{NoRequireGit: true})
	missing, extra := diff(got, want)
	if len(missing) > 0 || len(extra) > 0 {
		t.Errorf("no-require-git walk differs\n  ours:      %v\n  reference: %v\n  missing: %v\n  extra: %v",
			got, want, missing, extra)
	}
}

// TestKnownReferenceDivergences pins the two places where this port
// deliberately does NOT match the reference, because the reference disagrees
// with git and with ripgrep — and ripgrep compatibility is one of its own
// stated goals.
//
// Both are negation-across-files cases, and in both the reference hides a file
// the user expects to be searchable. That failure is silent: the search simply
// returns fewer results.
//
// Ground truth was taken from the tools themselves, not from reading specs:
//
//	$ git check-ignore -v sub/b.tmp
//	sub/.gitignore:1:!*.tmp   sub/b.tmp        # a negation: NOT ignored
//	$ git status --porcelain -uall
//	?? sub/b.tmp                                # git agrees it is visible
//
//	$ rg --files                                # with .gitignore "*.data"
//	keep.data                                   # and .ignore "!keep.data"
//
// If a future reference release fixes these, these tests are where to notice:
// they assert our behaviour directly rather than parity, so they keep passing,
// and the parity table above will simply grow two more green cases.
func TestKnownReferenceDivergences(t *testing.T) {
	t.Run("a nested negation re-includes, as git says", func(t *testing.T) {
		root := tree(t, map[string]string{
			".gitignore":     "*.tmp\n",
			"a.tmp":          "drop",
			"sub/.gitignore": "!*.tmp\n",
			"sub/b.tmp":      "keep",
		})
		got := ourFiles(t, root, walker.Options{})
		if slices.Contains(got, "a.tmp") {
			t.Errorf("a.tmp is ignored by the root rule and must not be admitted: %v", got)
		}
		if !slices.Contains(got, "sub/b.tmp") {
			t.Errorf("sub/b.tmp is re-included by sub/.gitignore and must be admitted: %v", got)
		}
		// Record what the reference does, so a change there is visible in the
		// log rather than silently ignored.
		t.Logf("reference admits: %v", oracleFiles(t, root))
	})

	t.Run("an .ignore outranks a .gitignore beside it, as ripgrep says", func(t *testing.T) {
		root := tree(t, map[string]string{
			".gitignore": "*.data\n",
			".ignore":    "!keep.data\n",
			"drop.data":  "drop",
			"keep.data":  "keep",
		})
		got := ourFiles(t, root, walker.Options{})
		if slices.Contains(got, "drop.data") {
			t.Errorf("drop.data matches only the exclusion and must not be admitted: %v", got)
		}
		if !slices.Contains(got, "keep.data") {
			t.Errorf("keep.data is re-included by .ignore and must be admitted: %v", got)
		}
		t.Logf("reference admits: %v", oracleFiles(t, root))
	})
}

// TestWalkSetOnRealRepositories runs the same set comparison over whole real
// checkouts, which reach ignore patterns no hand-written fixture will: vendored
// trees, nested .gitignore files, build output, lockfiles, generated code.
//
// Opt-in, because it needs repositories this machine happens to have. Point it
// at a colon-separated list:
//
//	TGREP_REAL_REPOS=~/src/foo:~/src/bar go test ./compat/ -run RealRepositories
func TestWalkSetOnRealRepositories(t *testing.T) {
	spec := os.Getenv("TGREP_REAL_REPOS")
	if spec == "" {
		t.Skip("set TGREP_REAL_REPOS to a colon-separated list of checkouts")
	}
	for _, repo := range strings.Split(spec, ":") {
		repo = strings.TrimSpace(repo)
		if repo == "" {
			continue
		}
		t.Run(filepath.Base(repo), func(t *testing.T) {
			abs, err := filepath.Abs(repo)
			if err != nil {
				t.Fatal(err)
			}
			if resolved, err := filepath.EvalSymlinks(abs); err == nil {
				abs = resolved
			}
			if _, err := os.Stat(abs); err != nil {
				t.Skipf("%s is not present", abs)
			}

			want := oracleFiles(t, abs)
			got := ourFiles(t, abs, walker.Options{})
			missing, extra := diff(got, want)

			t.Logf("%s: reference admits %d paths, we admit %d", abs, len(want), len(got))
			if len(missing) > 0 {
				t.Errorf("the reference admits %d paths we do not, e.g. %v", len(missing), first(missing, 10))
			}
			if len(extra) > 0 {
				t.Errorf("we admit %d paths the reference does not, e.g. %v", len(extra), first(extra, 10))
			}
		})
	}
}

// first returns at most n elements, so a failure on a large repository prints
// a sample rather than thousands of paths.
func first(xs []string, n int) []string {
	if len(xs) <= n {
		return xs
	}
	return xs[:n]
}
