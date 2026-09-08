package compat

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/notshekhar/tgrep-go/encoding"
	"github.com/notshekhar/tgrep-go/search"
)

// utf16LE encodes text as UTF-16 little-endian with a byte-order mark.
func utf16LE(text string) []byte {
	units := utf16.Encode([]rune("\ufeff" + text))
	out := make([]byte, 0, len(units)*2)
	for _, u := range units {
		out = append(out, byte(u), byte(u>>8))
	}
	return out
}

// utf16BE is the same, big-endian.
func utf16BE(text string) []byte {
	units := utf16.Encode([]rune("\ufeff" + text))
	out := make([]byte, 0, len(units)*2)
	for _, u := range units {
		out = append(out, byte(u>>8), byte(u))
	}
	return out
}

// TestUTF16FilesAreSearchable covers a defect that made a whole class of file
// silently unfindable.
//
// A UTF-16 file interleaves NUL bytes with its text, so a byte-oriented reader
// classifies it as binary and skips it: the user gets no match, no error, and
// no way to tell the difference from "not present". The reference sniffs the
// byte-order mark and transcodes; this port now does too.
//
// Found by indexing 159,000 files and noticing the two implementations'
// trigram tables had diverged — never by a hand-written case.
func TestUTF16FilesAreSearchable(t *testing.T) {
	root := t.TempDir()
	files := map[string][]byte{
		"le.txt":    utf16LE("hello utf16 little endian\n"),
		"be.txt":    utf16BE("hello utf16 big endian\n"),
		"plain.txt": []byte("hello plain ascii\n"),
		"bom8.txt":  append([]byte{0xEF, 0xBB, 0xBF}, "hello utf8 with bom\n"...),
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, name), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A git repo, so the walk behaves as it does in a real checkout.
	for _, args := range [][]string{{"init", "-q"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	s := indexed(t, root)
	res, err := s.Run(search.Options{Patterns: []string{"hello"}})
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, fr := range res.Files {
		found = append(found, fr.Path)
	}
	slices.Sort(found)

	for _, want := range []string{"be.txt", "bom8.txt", "le.txt", "plain.txt"} {
		if !slices.Contains(found, want) {
			t.Errorf("%s was not searchable; found %v", want, found)
		}
	}

	// And the same answer as the reference.
	got := ourSearch(t, s, search.Options{Patterns: []string{"utf16"}})
	want := oracleSearch(t, root, "utf16")
	compareHits(t, "utf16", got, want)
}

// TestBOMIsNotIndexedAsContent guards the other half: the mark itself must be
// stripped, not treated as three bytes of text at the start of the file.
func TestBOMIsNotIndexedAsContent(t *testing.T) {
	body := "package main\n"
	withBOM := append([]byte{0xEF, 0xBB, 0xBF}, body...)

	decoded, fx := encoding.Decode(withBOM, encoding.Auto)
	if string(decoded) != body {
		t.Errorf("decoded to %q, want %q — the byte-order mark survived", decoded, body)
	}
	if !fx.IsEmpty() {
		t.Error("stripping a mark should need no offset fixups")
	}
}

// TestLossyRepairMatchesTheReference pins the replacement granularity.
//
// It is not one U+FFFD per bad byte. The Unicode standard substitutes one per
// MAXIMAL SUBPART, so 0xF0 0x9F — a truncated but well-formed prefix of a
// four-byte sequence — is ONE replacement, while 0xE0 0x80 is a valid start
// followed by a byte that cannot continue it and is TWO.
//
// Go's utf8.DecodeRune cannot express this; it reports invalid input one byte
// at a time. Getting it wrong changes which trigrams a damaged file
// contributes, which is precisely how the two indexes came to disagree.
//
// The expected counts were measured from the reference implementation, not
// derived from reading the standard.
func TestLossyRepairMatchesTheReference(t *testing.T) {
	cases := []struct {
		name  string
		input []byte
		want  int // U+FFFD count the reference produces
	}{
		{"a single invalid byte", []byte("ok \xff bad"), 1},
		{"two standalone invalid bytes", []byte("ok \xff\xfe bad"), 2},
		{"valid start, impossible continuation", []byte("ok \xe0\x80 bad"), 2},
		{"truncated four-byte prefix", []byte("ok \xf0\x9f bad"), 1},
		{"truncated two-byte prefix", []byte("ok \xc3 bad"), 1},
		{"already valid", []byte("ok plain ascii"), 0},
		{"valid multibyte", []byte("ok café naïve"), 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, _ := encoding.Decode(tc.input, encoding.Auto)
			if got := bytes.Count(out, []byte("�")); got != tc.want {
				t.Errorf("%q produced %d replacements, want %d (decoded %q)", tc.input, got, tc.want, out)
			}
			if !utf8.Valid(out) {
				t.Errorf("repaired output is still not valid UTF-8: %q", out)
			}
		})
	}
}

// TestFixupsMapBackToSourceOffsets checks the bookkeeping that keeps reported
// columns pointing at the right byte.
//
// One repaired byte becomes three, so every offset after it shifts. Without
// the mapping, a column in a repaired file would point past where the match
// actually is — by three bytes for each earlier repair.
func TestFixupsMapBackToSourceOffsets(t *testing.T) {
	// "ab\xffcd" decodes to "ab<U+FFFD>cd": 'c' sits at decoded offset 5 but
	// source offset 3.
	src := []byte("ab\xffcd")
	decoded, fx := encoding.Decode(src, encoding.Auto)

	if string(decoded) != "ab�cd" {
		t.Fatalf("decoded to %q", decoded)
	}
	if fx.IsEmpty() {
		t.Fatal("a repaired file reported no fixups")
	}

	for _, tc := range []struct{ decoded, source int }{
		{0, 0}, // 'a'
		{1, 1}, // 'b'
		{2, 2}, // the replacement itself maps to the byte it replaced
		{5, 3}, // 'c'
		{6, 4}, // 'd'
	} {
		if got := fx.ToSourceOffset(tc.decoded); got != tc.source {
			t.Errorf("decoded offset %d maps to source %d, want %d", tc.decoded, got, tc.source)
		}
	}

	// An unrepaired file maps identically, and must not pay for a lookup.
	_, clean := encoding.Decode([]byte("plain ascii"), encoding.Auto)
	if !clean.IsEmpty() {
		t.Error("a clean file reported fixups")
	}
	if got := clean.ToSourceOffset(7); got != 7 {
		t.Errorf("clean mapping moved offset 7 to %d", got)
	}
}

// TestEncodingNoneLeavesBytesAlone covers the opt-out.
func TestEncodingNoneLeavesBytesAlone(t *testing.T) {
	src := utf16LE("hello")
	out, _ := encoding.Decode(src, encoding.None)
	if !bytes.Equal(out, src) {
		t.Error("Mode None transcoded anyway")
	}
}
