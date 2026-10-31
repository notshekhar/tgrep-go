package matching

import (
	"fmt"
	"regexp"
	"strings"
)

// Matcher is a compiled search pattern.
//
// Go's regexp is RE2: linear time, no backtracking, and therefore no
// backreferences or lookaround. The Rust implementation falls back to
// fancy_regex for those; there is no equivalent in the standard library here,
// so such patterns are rejected at compile time with a clear message rather
// than silently mismatching. The index-side relaxation in package query
// already handles narrowing candidates for them, so adding an engine later
// touches only this file.
type Matcher struct {
	re *regexp.Regexp
	// multiline is whether a match may span line boundaries. It changes what
	// gets fed to the engine — whole file, or one line at a time — so it lives
	// with the matcher rather than with the caller.
	multiline bool
}

// Config describes how to build a matcher.
type Config struct {
	// FixedString treats every pattern as literal text, not a regex.
	FixedString bool
	// CaseInsensitive folds case.
	CaseInsensitive bool
	// SmartCase folds case only when no pattern contains an uppercase
	// literal, which is what makes a lowercase query forgiving without making
	// an explicitly capitalised one useless. See FoldsCase.
	SmartCase bool
	// WordBoundary requires each match to be a whole word.
	WordBoundary bool
	// LineRegexp requires each match to span a whole line.
	LineRegexp bool
	// Multiline lets a match cross line boundaries.
	Multiline bool
	// MultilineDotAll additionally lets '.' match a newline. It has no effect
	// without Multiline.
	MultilineDotAll bool
}

// Build compiles one matcher covering every pattern.
//
// Several patterns are joined into an alternation rather than matched one at a
// time: a file matches if any of them matches, and one pass of one automaton
// is both faster and the only way spans from different patterns end up sorted
// against each other correctly.
func Build(patterns []string, cfg Config) (*Matcher, error) {
	if len(patterns) == 0 {
		return nil, fmt.Errorf("no pattern given")
	}

	parts := make([]string, 0, len(patterns))
	for _, p := range patterns {
		if cfg.FixedString {
			p = regexp.QuoteMeta(p)
		} else if err := rejectUnsupported(p); err != nil {
			return nil, err
		}
		switch {
		case cfg.LineRegexp:
			// Anchoring each alternative separately is the only correct
			// placement: wrapping the whole alternation would anchor only its
			// first and last branches.
			p = `^(?:` + p + `)$`
		case cfg.WordBoundary:
			p = wordWrap(p)
		}
		parts = append(parts, `(?:`+p+`)`)
	}
	expr := strings.Join(parts, "|")

	var flags string
	if cfg.FoldsCase(patterns) {
		flags += "i"
	}
	if cfg.Multiline {
		// Anchors become per-line, which is what makes '^' useful in a
		// whole-file match. Without Multiline the engine never sees more than
		// one line at a time and this would be meaningless.
		flags += "m"
		if cfg.MultilineDotAll {
			flags += "s"
		}
	}
	if flags != "" {
		expr = "(?" + flags + ")" + expr
	}

	re, err := regexp.Compile(expr)
	if err != nil {
		return nil, fmt.Errorf("invalid pattern: %w", err)
	}
	return &Matcher{re: re, multiline: cfg.Multiline}, nil
}

// rejectUnsupported catches the two PCRE constructs RE2 cannot express, so the
// failure is a clear message instead of a pattern that quietly means something
// else.
func rejectUnsupported(pattern string) error {
	for i := 0; i+2 < len(pattern); i++ {
		if pattern[i] != '(' || pattern[i+1] != '?' {
			continue
		}
		switch pattern[i+2] {
		case '=', '!':
			return fmt.Errorf("lookaround is not supported by this engine: %q", pattern)
		case '<':
			if i+3 < len(pattern) && (pattern[i+3] == '=' || pattern[i+3] == '!') {
				return fmt.Errorf("lookbehind is not supported by this engine: %q", pattern)
			}
		}
	}
	for i := 0; i+1 < len(pattern); i++ {
		if pattern[i] == '\\' && pattern[i+1] >= '1' && pattern[i+1] <= '9' {
			return fmt.Errorf("backreferences are not supported by this engine: %q", pattern)
		}
	}
	return nil
}

// Multiline reports whether matches may cross line boundaries.
func (m *Matcher) Multiline() bool { return m.multiline }

// String returns the compiled expression, for diagnostics.
func (m *Matcher) String() string { return m.re.String() }

// FindSpans returns the match spans in content, as absolute byte offsets.
//
// all decides whether every match on a line is located or just the first. Only
// highlighting, column reporting and match counting need the later ones; a
// plain search only has to know the line matched, and scanning the rest of it
// is pure overhead.
func (m *Matcher) FindSpans(content []byte, all bool) []Span {
	if m.multiline {
		return m.findIn(content, 0, all)
	}

	// Line mode feeds one line at a time, which is what keeps '.' from
	// crossing a newline and makes '^' and '$' mean the line's edges without
	// depending on a flag.
	var spans []Span
	offset := 0
	for offset <= len(content) {
		end := len(content)
		if i := indexByte(content[offset:], '\n'); i >= 0 {
			end = offset + i
		}
		line := content[offset:end]
		// A CRLF file must match the same as an LF one, so the carriage
		// return is not part of the line the pattern sees.
		trimmed := line
		if len(trimmed) > 0 && trimmed[len(trimmed)-1] == '\r' {
			trimmed = trimmed[:len(trimmed)-1]
		}
		spans = append(spans, m.findIn(trimmed, offset, all)...)
		if end >= len(content) {
			break
		}
		offset = end + 1
	}
	return spans
}

// findIn runs the engine over one buffer, shifting results by base.
func (m *Matcher) findIn(buf []byte, base int, all bool) []Span {
	limit := 1
	if all {
		limit = -1
	}
	found := m.re.FindAllIndex(buf, limit)
	if found == nil {
		return nil
	}
	spans := make([]Span, 0, len(found))
	for _, loc := range found {
		spans = append(spans, Span{Start: base + loc[0], End: base + loc[1]})
	}
	return spans
}

// MatchLine reports whether one line matches, without locating anything. This
// is the hot path for a plain search and for inverted matching.
func (m *Matcher) MatchLine(line []byte) bool { return m.re.Match(line) }

// ReplaceAll rewrites every match in one line using a replacement template,
// where $1 and ${name} refer to capture groups.
func (m *Matcher) ReplaceAll(line []byte, template string) []byte {
	return m.re.ReplaceAll(line, []byte(template))
}

func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}
