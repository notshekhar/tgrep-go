// Package output formats search results.
//
// Every formatter here is byte-compatible with ripgrep's, because that is what
// editors, scripts and the reference implementation all expect. The details
// that look arbitrary are not: a match line joins its fields with ':' and a
// context line with '-', so a caller can tell them apart without parsing; and
// '--' separates non-contiguous blocks so that two windows of context are not
// mistaken for one run of lines.
package output

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strconv"

	"github.com/notshekhar/tgrep-go/matching"
	"github.com/notshekhar/tgrep-go/search"
)

// Format selects a formatter.
type Format int

const (
	// FormatDefault is `path:line:text`, or `path-line-text` for context.
	FormatDefault Format = iota
	// FormatVimgrep is `path:line:col:text`, one row per match.
	FormatVimgrep
	// FormatJSON is ripgrep's JSON Lines stream.
	FormatJSON
	// FormatCount is `path:count` of matching lines.
	FormatCount
	// FormatCountMatches is `path:count` of individual matches.
	FormatCountMatches
	// FormatFilesWithMatches is one path per matching file.
	FormatFilesWithMatches
	// FormatFilesWithoutMatch is one path per searched file that did not match.
	FormatFilesWithoutMatch
	// FormatQuiet prints nothing; the exit status carries the answer.
	FormatQuiet
)

// Options tune a formatter.
type Options struct {
	Format Format
	// LineNumbers includes line numbers in FormatDefault.
	LineNumbers bool
	// Heading prints the path once above its matches rather than on each line.
	Heading bool
	// NullSeparator terminates paths with NUL instead of newline, for xargs -0.
	NullSeparator bool
	// PathPrefix is prepended to every path, so output can name files the way
	// the user asked for them rather than relative to the index root.
	PathPrefix string
	// WithContext tells the formatter that context lines may appear, which is
	// what turns on the '--' block separators.
	WithContext bool
}

// Writer formats results onto an output stream.
type Writer struct {
	w    *bufio.Writer
	opts Options
	// wroteBlock tracks whether anything has been emitted yet, so the first
	// block is not preceded by a separator.
	wroteBlock bool
}

// NewWriter returns a formatter writing to w. Close must be called to flush.
func NewWriter(w io.Writer, opts Options) *Writer {
	return &Writer{w: bufio.NewWriterSize(w, 64<<10), opts: opts}
}

// Close flushes buffered output.
func (w *Writer) Close() error { return w.w.Flush() }

func (w *Writer) path(p string) string { return w.opts.PathPrefix + p }

// WriteResult emits a whole search.
func (w *Writer) WriteResult(res *search.Result) error {
	for _, fr := range res.Files {
		if err := w.WriteFile(fr); err != nil {
			return err
		}
	}
	return nil
}

// WriteFile emits one file's results.
func (w *Writer) WriteFile(fr search.FileResult) error {
	switch w.opts.Format {
	case FormatQuiet:
		return nil
	case FormatFilesWithMatches:
		if fr.MatchedLines > 0 {
			return w.writePathLine(fr.Path)
		}
		return nil
	case FormatFilesWithoutMatch:
		if fr.MatchedLines == 0 {
			return w.writePathLine(fr.Path)
		}
		return nil
	case FormatCount:
		if fr.MatchedLines > 0 {
			_, err := fmt.Fprintf(w.w, "%s:%d\n", w.path(fr.Path), fr.MatchedLines)
			return err
		}
		return nil
	case FormatCountMatches:
		if fr.Matches > 0 {
			_, err := fmt.Fprintf(w.w, "%s:%d\n", w.path(fr.Path), fr.Matches)
			return err
		}
		return nil
	case FormatVimgrep:
		return w.writeVimgrep(fr)
	case FormatJSON:
		return w.writeJSON(fr)
	default:
		return w.writeDefault(fr)
	}
}

func (w *Writer) writePathLine(p string) error {
	terminator := byte('\n')
	if w.opts.NullSeparator {
		terminator = 0
	}
	if _, err := w.w.WriteString(w.path(p)); err != nil {
		return err
	}
	return w.w.WriteByte(terminator)
}

// writeDefault emits `path:line:text` for matches and `path-line-text` for
// context, with '--' between non-contiguous blocks.
func (w *Writer) writeDefault(fr search.FileResult) error {
	if len(fr.Lines) == 0 {
		return nil
	}

	if w.opts.Heading {
		if w.wroteBlock {
			if _, err := w.w.WriteString("\n"); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(w.w, "%s\n", w.path(fr.Path)); err != nil {
			return err
		}
	}

	prevLine := -1
	for _, line := range fr.Lines {
		// A gap between displayed lines means two context windows that do not
		// touch. Without the separator they would read as one continuous run,
		// which is exactly the ambiguity '--' exists to remove.
		if w.opts.WithContext && prevLine >= 0 && line.Number != prevLine+1 {
			if _, err := w.w.WriteString("--\n"); err != nil {
				return err
			}
		} else if w.opts.WithContext && prevLine < 0 && w.wroteBlock && !w.opts.Heading {
			if _, err := w.w.WriteString("--\n"); err != nil {
				return err
			}
		}
		prevLine = line.Number

		sep := ":"
		if line.Kind == matching.KindContext {
			sep = "-"
		}
		if !w.opts.Heading {
			if _, err := w.w.WriteString(w.path(fr.Path) + sep); err != nil {
				return err
			}
		}
		if w.opts.LineNumbers {
			if _, err := w.w.WriteString(strconv.Itoa(line.Number) + sep); err != nil {
				return err
			}
		}
		if _, err := w.w.Write(line.Text); err != nil {
			return err
		}
		if err := w.w.WriteByte('\n'); err != nil {
			return err
		}
		w.wroteBlock = true
	}
	return nil
}

// writeVimgrep emits one row per match, which is what an editor consumes to
// build a jump list.
//
// Context lines, when asked for, are emitted in the default format's
// `path-line-text` shape rather than as vimgrep rows. That looks inconsistent
// and is deliberate: a context line has no match in it and therefore no column
// to report, and both ripgrep and the reference implementation print it this
// way, so an editor parsing the output already expects it.
func (w *Writer) writeVimgrep(fr search.FileResult) error {
	for _, line := range fr.Lines {
		if line.Kind == matching.KindContext {
			if _, err := fmt.Fprintf(w.w, "%s-%d-%s\n", w.path(fr.Path), line.Number, line.Text); err != nil {
				return err
			}
			w.wroteBlock = true
			continue
		}
		cols := line.Columns()
		if len(cols) == 0 {
			// An inverted match has no located span but is still one row.
			cols = []int{1}
		}
		for _, col := range cols {
			if _, err := fmt.Fprintf(w.w, "%s:%d:%d:%s\n", w.path(fr.Path), line.Number, col, line.Text); err != nil {
				return err
			}
			w.wroteBlock = true
		}
	}
	return nil
}

// ripgrep's JSON Lines events. The nesting looks redundant but is the shape
// consumers already parse, so it is reproduced exactly.
type jsonText struct {
	Text string `json:"text"`
}

type jsonSubmatch struct {
	Match jsonText `json:"match"`
	Start int      `json:"start"`
	End   int      `json:"end"`
}

type jsonBeginEnd struct {
	Path jsonText `json:"path"`
}

type jsonMatch struct {
	Path           jsonText       `json:"path"`
	Lines          jsonText       `json:"lines"`
	LineNumber     int            `json:"line_number"`
	AbsoluteOffset int            `json:"absolute_offset"`
	Submatches     []jsonSubmatch `json:"submatches"`
}

type jsonEvent struct {
	Type string `json:"type"`
	Data any    `json:"data"`
}

func (w *Writer) writeJSON(fr search.FileResult) error {
	if len(fr.Lines) == 0 {
		return nil
	}
	encode := func(ev jsonEvent) error {
		encoded, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		if _, err := w.w.Write(encoded); err != nil {
			return err
		}
		return w.w.WriteByte('\n')
	}

	path := w.path(fr.Path)
	if err := encode(jsonEvent{Type: "begin", Data: jsonBeginEnd{Path: jsonText{path}}}); err != nil {
		return err
	}
	for _, line := range fr.Lines {
		kind := "match"
		if line.Kind == matching.KindContext {
			kind = "context"
		}
		// ripgrep reports the line WITH its terminator in `lines.text`, which
		// is why the newline is added back here rather than being an accident.
		event := jsonMatch{
			Path:           jsonText{path},
			Lines:          jsonText{string(line.Text) + "\n"},
			LineNumber:     line.Number,
			AbsoluteOffset: line.Offset,
			Submatches:     []jsonSubmatch{},
		}
		for _, s := range line.Spans {
			event.Submatches = append(event.Submatches, jsonSubmatch{
				Match: jsonText{string(line.Text[s.Start:min(s.End, len(line.Text))])},
				Start: s.Start,
				End:   s.End,
			})
		}
		if err := encode(jsonEvent{Type: kind, Data: event}); err != nil {
			return err
		}
	}
	if err := encode(jsonEvent{Type: "end", Data: jsonBeginEnd{Path: jsonText{path}}}); err != nil {
		return err
	}
	w.wroteBlock = true
	return nil
}
