// Package matching turns a compiled pattern and a file's contents into the
// lines a search should report.
//
// It is deliberately separate from output formatting: this package decides
// what matched and which lines are involved, and says nothing about how any of
// it is printed. Splitting them keeps the line arithmetic — which is where
// off-by-ones live — testable without a formatter in the way.
package matching

import (
	"bytes"
	"sort"
)

// LineIndex maps byte offsets in one file to line numbers and back.
//
// The line starts are computed on first use rather than at construction. Most
// files a search touches never match, and for those the whole index is wasted
// work; the ones that do match pay for it once.
type LineIndex struct {
	content []byte
	starts  []int
	built   bool
}

// NewLineIndex indexes content, which must outlive the index.
func NewLineIndex(content []byte) *LineIndex {
	return &LineIndex{content: content}
}

func (li *LineIndex) ensure() {
	if li.built {
		return
	}
	li.built = true
	if len(li.content) == 0 {
		return
	}
	// Sized exactly up front. Growing by doubling both overshoots — by up to
	// twice what is needed — and copies the whole slice on the way there; on a
	// large file that transient is tens of megabytes, and the exact count is
	// one scan away.
	li.starts = make([]int, 0, 1+bytes.Count(li.content, []byte{'\n'}))
	li.starts = append(li.starts, 0)
	for i, b := range li.content {
		if b == '\n' {
			li.starts = append(li.starts, i+1)
		}
	}
	// A trailing newline opens a final empty line that nothing else in the
	// pipeline yields. Dropping it keeps line counts agreeing everywhere.
	if len(li.starts) > 1 && li.starts[len(li.starts)-1] == len(li.content) {
		li.starts = li.starts[:len(li.starts)-1]
	}
}

// LineCount is how many lines the file has.
func (li *LineIndex) LineCount() int {
	li.ensure()
	return len(li.starts)
}

// LineOf returns the 0-based index of the line containing offset.
func (li *LineIndex) LineOf(offset int) int {
	li.ensure()
	if len(li.starts) == 0 {
		return 0
	}
	// The first start is always 0, so a miss can never land at index 0 and the
	// decrement below is safe.
	i := sort.SearchInts(li.starts, offset)
	if i < len(li.starts) && li.starts[i] == offset {
		return i
	}
	return i - 1
}

// LineStart is the offset of line idx.
func (li *LineIndex) LineStart(idx int) int {
	li.ensure()
	if idx < 0 || idx >= len(li.starts) {
		return len(li.content)
	}
	return li.starts[idx]
}

// LineEnd is the offset just past line idx's text, excluding its terminator.
func (li *LineIndex) LineEnd(idx int) int {
	li.ensure()
	start := li.LineStart(idx)
	end := len(li.content)
	if idx+1 < len(li.starts) {
		end = li.starts[idx+1]
	}
	if end > start && li.content[end-1] == '\n' {
		end--
	}
	// A CRLF file must report the same line text as an LF one, or a pattern
	// ending in '$' matches in one and not the other.
	if end > start && li.content[end-1] == '\r' {
		end--
	}
	return end
}

// LineTerminatorLen is the length of line idx's terminator: 2 for CRLF, 1 for
// a bare newline, 0 for a final line the file does not terminate.
//
// Needed because a column limit measures a line including its terminator.
func (li *LineIndex) LineTerminatorLen(idx int) int {
	li.ensure()
	end := len(li.content)
	if idx+1 < len(li.starts) {
		end = li.starts[idx+1]
	}
	return end - li.LineEnd(idx)
}

// LineText is line idx without its terminator.
func (li *LineIndex) LineText(idx int) []byte {
	return li.content[li.LineStart(idx):li.LineEnd(idx)]
}

// Span is a match range in file-absolute byte offsets.
type Span struct{ Start, End int }

// LineHit is one line together with the match ranges inside it.
type LineHit struct {
	// Idx is the 0-based line index.
	Idx int
	// Spans are match ranges relative to the start of this line's text,
	// sorted and non-overlapping.
	Spans []Span
}

// GroupSpansByLine distributes absolute match spans onto the lines they cover.
//
// A span may cross line boundaries under multiline matching, in which case
// every line it touches gets the portion falling inside it. Overlapping spans
// on one line are merged, so a highlighter never has to reason about nesting.
func GroupSpansByLine(li *LineIndex, spans []Span) []LineHit {
	byLine := map[int][]Span{}
	var order []int

	for _, s := range spans {
		first := li.LineOf(s.Start)
		// An empty match sits entirely on its start line.
		last := first
		if s.End > s.Start {
			last = li.LineOf(s.End - 1)
		}
		if maxLine := li.LineCount() - 1; last > maxLine {
			last = maxLine
		}
		for line := first; line <= last; line++ {
			ls, le := li.LineStart(line), li.LineEnd(line)
			cs := clamp(s.Start, ls, le) - ls
			ce := clamp(s.End, ls, le) - ls
			if _, seen := byLine[line]; !seen {
				order = append(order, line)
			}
			byLine[line] = append(byLine[line], Span{cs, ce})
		}
	}

	sort.Ints(order)
	hits := make([]LineHit, 0, len(order))
	for _, line := range order {
		spans := byLine[line]
		sort.Slice(spans, func(i, j int) bool {
			if spans[i].Start != spans[j].Start {
				return spans[i].Start < spans[j].Start
			}
			return spans[i].End < spans[j].End
		})
		merged := make([]Span, 0, len(spans))
		for _, s := range spans {
			// Strictly less than, not less-or-equal: two spans that merely
			// TOUCH are two matches, not one. Searching "context|Context"
			// over "ContextContext" finds a match at 0 and another at 7, and
			// the first ends exactly where the second begins — merging them
			// would report one match where ripgrep and the reference
			// implementation both report two, losing a column an editor needs
			// to jump to.
			//
			// Genuinely overlapping spans still merge, which is what this is
			// for: a multiline match clipped onto a line can produce them, and
			// a highlighter should not have to reason about nesting.
			if n := len(merged); n > 0 && s.Start < merged[n-1].End {
				merged[n-1].End = max(merged[n-1].End, s.End)
				continue
			}
			merged = append(merged, s)
		}
		hits = append(hits, LineHit{Idx: line, Spans: merged})
	}
	return hits
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// ExpandContextWindow returns every line index to display, given the matching
// lines and how much context to show around them. The result is sorted and
// deduplicated, so overlapping windows merge into one run.
func ExpandContextWindow(matchLines []int, totalLines, before, after int) []int {
	seen := map[int]struct{}{}
	var out []int
	for _, mi := range matchLines {
		start := mi - before
		if start < 0 {
			start = 0
		}
		end := min(mi+after+1, totalLines)
		for j := start; j < end; j++ {
			if _, dup := seen[j]; dup {
				continue
			}
			seen[j] = struct{}{}
			out = append(out, j)
		}
	}
	sort.Ints(out)
	return out
}
