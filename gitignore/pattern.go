// Package gitignore implements gitignore pattern matching.
//
// Rust's tgrep delegates this to the `ignore` crate; Go has no equivalent that
// covers the whole of gitignore(5), so the semantics are implemented here.
// They are worth stating precisely, because almost every subtlety is a place
// where a wrong answer silently changes which files are searchable:
//
//   - A pattern with no '/' except perhaps a trailing one matches a BASENAME
//     at any depth: "*.log" hides logs everywhere.
//   - A pattern containing '/' anywhere else is ANCHORED to the directory
//     holding the .gitignore: "doc/build" hides only that one path.
//   - A leading '/' anchors regardless: "/tmp" is the root's tmp alone.
//   - A trailing '/' matches directories only.
//   - A leading '!' negates, re-including a path an earlier pattern excluded.
//   - '*' does not cross '/'; '**' does. "a/**/b" also matches "a/b".
//   - '?' matches one character other than '/'.
//   - '[...]' is a character class, with '!' or '^' negating it.
//   - A '#' first line is a comment, and a trailing space is stripped unless
//     escaped.
//
// Within one file, the LAST matching pattern decides. Across files, a deeper
// .gitignore beats a shallower one, and .ignore beats .gitignore at the same
// depth.
package gitignore

import (
	"strings"
)

// Pattern is one compiled gitignore line.
type Pattern struct {
	// Source is the pattern as written, kept for diagnostics.
	Source string
	// Negated is true for a '!' pattern, which re-includes.
	Negated bool
	// DirOnly is true for a trailing-slash pattern, which matches only
	// directories.
	DirOnly bool
	// Anchored is true when the pattern is fixed to the directory holding the
	// ignore file, rather than matching a basename at any depth.
	Anchored bool

	// segments is the pattern split on '/', each already broken into match
	// tokens. A nil segment means '**'.
	segments []segment
}

// segment is one path component of a pattern. globstar marks '**', which is
// the only token allowed to span components.
type segment struct {
	globstar bool
	tokens   []token
}

// token is one element of a component matcher.
type token struct {
	kind tokenKind
	// literal holds the text for tokenLiteral.
	literal string
	// class holds the parsed body of a bracket expression.
	class charClass
}

type tokenKind int

const (
	tokenLiteral tokenKind = iota
	tokenStar              // '*', matches any run of non-'/' characters
	tokenAny               // '?', matches exactly one non-'/' character
	tokenClass             // '[...]'
)

// charClass is a bracket expression.
type charClass struct {
	negated bool
	ranges  []charRange
}

type charRange struct{ lo, hi rune }

func (c charClass) matches(r rune) bool {
	var in bool
	for _, cr := range c.ranges {
		if r >= cr.lo && r <= cr.hi {
			in = true
			break
		}
	}
	return in != c.negated
}

// Parse compiles one line of a gitignore file.
//
// ok is false for a line that is not a pattern at all — a comment or a blank —
// which the caller skips rather than treats as an error.
func Parse(line string) (Pattern, bool) {
	// A blank line matches nothing, and '#' opens a comment. An escaped '\#'
	// is a real pattern for a file literally named '#…'.
	if line == "" || strings.HasPrefix(line, "#") {
		return Pattern{}, false
	}

	p := Pattern{Source: line}

	// Trailing spaces are stripped unless escaped. Leading ones are not:
	// a file may legitimately begin with a space.
	line = trimUnescapedTrailingSpace(line)
	if line == "" {
		return Pattern{}, false
	}

	if strings.HasPrefix(line, "!") {
		p.Negated = true
		line = line[1:]
	} else if strings.HasPrefix(line, `\!`) {
		// An escaped bang is a literal one.
		line = line[1:]
	}
	if line == "" {
		return Pattern{}, false
	}

	if strings.HasSuffix(line, "/") {
		p.DirOnly = true
		line = strings.TrimSuffix(line, "/")
		if line == "" {
			return Pattern{}, false
		}
	}

	// A '/' anywhere but the very end anchors the pattern. A leading one
	// anchors it and is then dropped, since it is not part of the path.
	if strings.HasPrefix(line, "/") {
		p.Anchored = true
		line = line[1:]
	} else if strings.Contains(line, "/") {
		p.Anchored = true
	}
	if line == "" {
		return Pattern{}, false
	}

	for _, part := range strings.Split(line, "/") {
		if part == "**" {
			p.segments = append(p.segments, segment{globstar: true})
			continue
		}
		p.segments = append(p.segments, segment{tokens: tokenize(part)})
	}
	return p, true
}

// trimUnescapedTrailingSpace removes trailing spaces that are not backslash
// escaped, which is what git does when reading a pattern.
func trimUnescapedTrailingSpace(s string) string {
	end := len(s)
	for end > 0 && s[end-1] == ' ' {
		// Count the backslashes immediately before this space: an odd number
		// escapes it, so the space is part of the pattern.
		backslashes := 0
		for i := end - 2; i >= 0 && s[i] == '\\'; i-- {
			backslashes++
		}
		if backslashes%2 == 1 {
			break
		}
		end--
	}
	return s[:end]
}

// tokenize breaks one path component into match tokens.
func tokenize(part string) []token {
	var tokens []token
	var literal strings.Builder
	flush := func() {
		if literal.Len() > 0 {
			tokens = append(tokens, token{kind: tokenLiteral, literal: literal.String()})
			literal.Reset()
		}
	}

	runes := []rune(part)
	for i := 0; i < len(runes); i++ {
		switch runes[i] {
		case '\\':
			// An escape makes the next character literal. A trailing
			// backslash has nothing to escape and stands for itself.
			if i+1 < len(runes) {
				i++
				literal.WriteRune(runes[i])
			} else {
				literal.WriteRune('\\')
			}
		case '*':
			flush()
			// Consecutive stars inside a component are no more powerful than
			// one: neither crosses a '/'.
			for i+1 < len(runes) && runes[i+1] == '*' {
				i++
			}
			tokens = append(tokens, token{kind: tokenStar})
		case '?':
			flush()
			tokens = append(tokens, token{kind: tokenAny})
		case '[':
			if class, next, ok := parseClass(runes, i); ok {
				flush()
				tokens = append(tokens, token{kind: tokenClass, class: class})
				i = next - 1
				continue
			}
			// An unterminated '[' is a literal bracket.
			literal.WriteRune('[')
		default:
			literal.WriteRune(runes[i])
		}
	}
	flush()
	return tokens
}

// parseClass reads a bracket expression starting at runes[open].
func parseClass(runes []rune, open int) (charClass, int, bool) {
	i := open + 1
	var class charClass
	if i < len(runes) && (runes[i] == '!' || runes[i] == '^') {
		class.negated = true
		i++
	}
	// A ']' in the first position is a literal member, not the terminator.
	if i < len(runes) && runes[i] == ']' {
		class.ranges = append(class.ranges, charRange{']', ']'})
		i++
	}

	for i < len(runes) {
		switch {
		case runes[i] == ']':
			return class, i + 1, true
		case runes[i] == '\\' && i+1 < len(runes):
			i++
			class.ranges = append(class.ranges, charRange{runes[i], runes[i]})
			i++
		case i+2 < len(runes) && runes[i+1] == '-' && runes[i+2] != ']':
			class.ranges = append(class.ranges, charRange{runes[i], runes[i+2]})
			i += 3
		default:
			class.ranges = append(class.ranges, charRange{runes[i], runes[i]})
			i++
		}
	}
	return charClass{}, 0, false // unterminated
}

// Match reports whether this pattern matches a path relative to the directory
// holding the ignore file. isDir tells a trailing-slash pattern what it needs.
func (p Pattern) Match(rel string, isDir bool) bool {
	if p.DirOnly && !isDir {
		return false
	}
	parts := strings.Split(rel, "/")

	if !p.Anchored {
		// An unanchored pattern matches at any depth. Trying every suffix is
		// what makes "*.log" find "deep/nested/x.log", and what makes
		// "build" match a "build" component anywhere.
		for start := range parts {
			if matchSegments(p.segments, parts[start:]) {
				return true
			}
		}
		return false
	}
	return matchSegments(p.segments, parts)
}

// matchSegments matches pattern segments against path components, handling
// '**' by trying every split point.
func matchSegments(segs []segment, parts []string) bool {
	switch {
	case len(segs) == 0:
		return len(parts) == 0
	case segs[0].globstar:
		// '**' matches zero or more components. Zero is what makes "a/**/b"
		// match "a/b", which is required by gitignore(5) and is the case most
		// implementations get wrong.
		for skip := 0; skip <= len(parts); skip++ {
			if matchSegments(segs[1:], parts[skip:]) {
				return true
			}
		}
		return false
	case len(parts) == 0:
		return false
	case !matchTokens(segs[0].tokens, parts[0]):
		return false
	default:
		return matchSegments(segs[1:], parts[1:])
	}
}

// matchTokens matches one component against one segment's tokens.
func matchTokens(tokens []token, name string) bool {
	return matchTokensFrom(tokens, []rune(name))
}

func matchTokensFrom(tokens []token, name []rune) bool {
	for i, tok := range tokens {
		switch tok.kind {
		case tokenLiteral:
			lit := []rune(tok.literal)
			if len(name) < len(lit) {
				return false
			}
			for j, r := range lit {
				if name[j] != r {
					return false
				}
			}
			name = name[len(lit):]
		case tokenAny:
			if len(name) == 0 {
				return false
			}
			name = name[1:]
		case tokenClass:
			if len(name) == 0 || !tok.class.matches(name[0]) {
				return false
			}
			name = name[1:]
		case tokenStar:
			// A star is the only backtracking point: try every length it
			// could consume, longest first so a trailing literal binds to the
			// last occurrence the way a greedy glob does.
			rest := tokens[i+1:]
			if len(rest) == 0 {
				return true // a trailing star eats whatever is left
			}
			for take := len(name); take >= 0; take-- {
				if matchTokensFrom(rest, name[take:]) {
					return true
				}
			}
			return false
		}
	}
	return len(name) == 0
}
