package matching

import (
	"strconv"
	"strings"
	"unicode"
)

// FoldsCase reports whether a search over these patterns is case-insensitive.
//
// The planner must ask this too, not just the matcher: a plan built
// case-sensitively for a search that then folds case looks up only the exact
// letters, and loses every file that matches through folding.
func (c Config) FoldsCase(patterns []string) bool {
	return c.CaseInsensitive || (c.SmartCase && smartCaseFolds(patterns, c.FixedString))
}

// smartCaseFolds is ripgrep's smart-case rule: fold when the patterns contain
// at least one literal character and none of their literals is uppercase.
//
// Only literals count. `\S`, `\W` and `\p{Lu}` are regex syntax that happens to
// be spelled in capitals, and a group name is a label — none of them asks for
// an uppercase letter in the text, so none may switch folding off. A pattern
// with no literal at all, like `\w+`, gives no evidence either way and stays
// case-sensitive.
func smartCaseFolds(patterns []string, fixedString bool) bool {
	var s smartCase
	for _, p := range patterns {
		if fixedString {
			for _, r := range p {
				s.literal(r)
			}
			continue
		}
		s.scan(p)
	}
	return s.hasLiteral && !s.hasUpper
}

type smartCase struct {
	hasLiteral, hasUpper bool
}

func (s *smartCase) literal(r rune) {
	s.hasLiteral = true
	s.hasUpper = s.hasUpper || unicode.IsUpper(r)
}

// scan walks RE2 syntax, reporting each literal character it spells.
func (s *smartCase) scan(p string) {
	inClass := false
	for i := 0; i < len(p); {
		r, size := decodeRune(p[i:])
		switch {
		case r == '\\':
			i += s.escape(p[i+1:]) + 1
			continue
		case inClass && strings.HasPrefix(p[i:], "[:"):
			// A POSIX class such as [:upper:] names a set, not letters.
			if end := strings.Index(p[i+2:], ":]"); end >= 0 {
				i += end + 4
				continue
			}
			s.literal(r)
		case inClass && r == ']':
			inClass = false
		case inClass:
			s.literal(r)
		case r == '[':
			inClass = true
			i++
			// A leading ^ negates, and a ] right after the opener is a member.
			if i < len(p) && p[i] == '^' {
				i++
			}
			if i < len(p) && p[i] == ']' {
				s.literal(']')
				i++
			}
			continue
		case r == '(' && strings.HasPrefix(p[i:], "(?"):
			i += groupPrefixLen(p[i:])
			continue
		case strings.ContainsRune(".^$|()*+?{}", r):
			// Operators. A literal brace such as `a{x}` still counts its x,
			// which is the next character scanned.
		default:
			s.literal(r)
		}
		i += size
	}
}

// escape consumes one escape sequence after its backslash and returns its
// length.
func (s *smartCase) escape(rest string) int {
	if rest == "" {
		return 0
	}
	switch c := rest[0]; {
	case strings.IndexByte("dDwWsSbBAz", c) >= 0:
		return 1
	case c == 'p' || c == 'P':
		if strings.HasPrefix(rest[1:], "{") {
			if end := strings.IndexByte(rest, '}'); end >= 0 {
				return end + 1
			}
		}
		return min(2, len(rest))
	case c == 'Q':
		body, _, _ := strings.Cut(rest[1:], `\E`)
		for _, r := range body {
			s.literal(r)
		}
		return min(len(rest), 1+len(body)+2)
	case c == 'x':
		return 1 + s.hexEscape(rest[1:])
	case c >= '0' && c <= '7':
		n := 1
		for n < 3 && n < len(rest) && rest[n] >= '0' && rest[n] <= '7' {
			n++
		}
		if v, err := strconv.ParseUint(rest[:n], 8, 32); err == nil {
			s.literal(rune(v))
		}
		return n
	default:
		// \n, \t and escaped punctuation all stand for one literal character.
		r, size := decodeRune(rest)
		s.literal(r)
		return size
	}
}

// hexEscape reads the digits of \xHH or \x{H...}, which spell a literal: \x41
// is an uppercase A as surely as A is.
func (s *smartCase) hexEscape(rest string) int {
	digits, n := rest, 0
	if strings.HasPrefix(rest, "{") {
		end := strings.IndexByte(rest, '}')
		if end < 0 {
			return len(rest)
		}
		digits, n = rest[1:end], end+1
	} else {
		n = min(2, len(rest))
		digits = rest[:n]
	}
	if v, err := strconv.ParseUint(digits, 16, 32); err == nil {
		s.literal(rune(v))
	}
	return n
}

// groupPrefixLen measures a group opener that carries no literal text: flags
// such as (?i) or (?i:, and the name in (?P<name> or (?<name>.
func groupPrefixLen(p string) int {
	if strings.HasPrefix(p, "(?P<") || strings.HasPrefix(p, "(?<") {
		if end := strings.IndexByte(p, '>'); end >= 0 {
			return end + 1
		}
	}
	if end := strings.IndexAny(p, ":)"); end >= 0 {
		return end + 1
	}
	return len(p)
}

func decodeRune(s string) (rune, int) {
	for _, r := range s {
		return r, len(string(r))
	}
	return 0, 0
}
