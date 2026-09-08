package matching

// Options control how one file's matches are assembled.
type Options struct {
	// InvertMatch reports the lines that do NOT match.
	InvertMatch bool
	// OnlyMatching reports each match on its own, rather than its whole line.
	OnlyMatching bool
	// BeforeContext and AfterContext are how many surrounding lines to emit.
	BeforeContext int
	AfterContext  int
	// MaxCount stops after this many matches. Zero means no limit.
	MaxCount int
	// AllSpans locates every match on a line rather than just the first.
	// Required for highlighting, columns and match counts.
	AllSpans bool
	// PassThru emits every line, matching or not, with matches still marked.
	PassThru bool
}

// FileMatches is one file's search result: which lines matched, and where.
type FileMatches struct {
	Index *LineIndex
	Hits  []LineHit
}

// Find matches content and assembles the per-line hits.
func Find(content []byte, m *Matcher, opts Options) *FileMatches {
	index := NewLineIndex(content)
	fm := &FileMatches{Index: index}

	if opts.InvertMatch {
		fm.Hits = invertedHits(index, m, opts)
		return fm
	}

	// A max count of zero from an explicit --max-count=0 means "no matches
	// wanted", which is different from the unset default.
	spans := m.FindSpans(content, opts.AllSpans || opts.OnlyMatching)
	fm.Hits = GroupSpansByLine(index, spans)
	if opts.MaxCount > 0 {
		fm.Hits = limitHits(fm.Hits, opts.MaxCount, m.Multiline())
	}
	return fm
}

// limitHits applies a match limit, which means different things in the two
// matching modes.
//
// In line mode a limit of N means N matching LINES, verified against the
// reference: `-m 1` over a file whose first two lines both match reports only
// the first. In multiline mode a single match may cover several lines, so the
// unit becomes a contiguous block — otherwise one match straddling two lines
// would spend two of the caller's budget.
func limitHits(hits []LineHit, max int, multiline bool) []LineHit {
	if !multiline {
		return hits[:min(max, len(hits))]
	}
	return limitToBlocks(hits, max)
}

// invertedHits reports the lines with no match on them.
//
// Inversion is line-oriented even under multiline matching: "this line has no
// match" is the only reading that stays meaningful when a match may span
// several lines, since a partially covered line is still matched.
func invertedHits(index *LineIndex, m *Matcher, opts Options) []LineHit {
	covered := map[int]struct{}{}
	for _, hit := range GroupSpansByLine(index, m.FindSpans(indexContent(index), true)) {
		covered[hit.Idx] = struct{}{}
	}

	var hits []LineHit
	for i := range index.LineCount() {
		if _, isMatch := covered[i]; isMatch {
			continue
		}
		hits = append(hits, LineHit{Idx: i})
		if opts.MaxCount > 0 && len(hits) >= opts.MaxCount {
			break
		}
	}
	return hits
}

func indexContent(li *LineIndex) []byte { return li.content }

// limitToBlocks keeps only the first max contiguous blocks of matching lines.
//
// A block is a run of lines that matches touch without a gap. That is the unit
// a match count is measured in, so several matches sharing a line spend one
// unit between them, and a match straddling two lines also spends just one —
// which is what makes --max-count mean the same thing with and without
// multiline matching.
func limitToBlocks(hits []LineHit, max int) []LineHit {
	if len(hits) == 0 {
		return hits
	}
	blocks := 1
	for i := 1; i < len(hits); i++ {
		if hits[i].Idx != hits[i-1].Idx+1 {
			blocks++
			if blocks > max {
				return hits[:i]
			}
		}
	}
	return hits
}

// Count is how many matching lines the file has.
func (fm *FileMatches) Count() int { return len(fm.Hits) }

// CountMatches is how many individual matches the file has, which differs from
// Count when several fall on one line.
func (fm *FileMatches) CountMatches() int {
	total := 0
	for _, hit := range fm.Hits {
		total += max(len(hit.Spans), 1)
	}
	return total
}

// Matched reports whether the file matched at all.
func (fm *FileMatches) Matched() bool { return len(fm.Hits) > 0 }

// Kind distinguishes the two sorts of emitted line.
type Kind int

const (
	// KindMatch is a line carrying at least one match.
	KindMatch Kind = iota
	// KindContext is a line shown only because it neighbours a match.
	KindContext
)

// Line is one line of output.
type Line struct {
	Kind Kind
	// Number is the 1-based line number.
	Number int
	// Text is the line without its terminator.
	Text []byte
	// Spans are match ranges within Text, empty for context lines.
	Spans []Span
	// Offset is the line's absolute byte offset in the file.
	Offset int
	// TerminatorLen is how many bytes of line terminator Text was stripped of.
	TerminatorLen int
}

// Emit produces the lines to display, in file order, with context filled in.
//
// Context lines are resolved here rather than by the caller because the
// windows around neighbouring matches overlap, and a caller merging them
// itself would print the shared lines twice.
func (fm *FileMatches) Emit(opts Options) []Line {
	if len(fm.Hits) == 0 && !opts.PassThru {
		return nil
	}

	spansByLine := make(map[int][]Span, len(fm.Hits))
	matchLines := make([]int, 0, len(fm.Hits))
	for _, hit := range fm.Hits {
		spansByLine[hit.Idx] = hit.Spans
		matchLines = append(matchLines, hit.Idx)
	}

	var display []int
	if opts.PassThru {
		display = make([]int, fm.Index.LineCount())
		for i := range display {
			display[i] = i
		}
	} else {
		display = ExpandContextWindow(matchLines, fm.Index.LineCount(), opts.BeforeContext, opts.AfterContext)
	}

	out := make([]Line, 0, len(display))
	for _, idx := range display {
		spans, isMatch := spansByLine[idx]
		line := Line{
			Kind:          KindContext,
			Number:        idx + 1,
			Text:          fm.Index.LineText(idx),
			Offset:        fm.Index.LineStart(idx),
			TerminatorLen: fm.Index.LineTerminatorLen(idx),
		}
		if isMatch {
			line.Kind = KindMatch
			line.Spans = spans
		}
		out = append(out, line)
	}
	return out
}

// Columns returns the 1-based byte columns of each match on a line, which is
// what an editor-facing format reports.
func (l Line) Columns() []int {
	cols := make([]int, 0, len(l.Spans))
	for _, s := range l.Spans {
		cols = append(cols, s.Start+1)
	}
	return cols
}
