package query

// Relaxation of PCRE-style patterns that Go's regexp/syntax cannot parse, so
// that a -P query can still narrow candidates through the index.
//
// # Soundness
//
// Every rewrite only ever widens the matched language, so L(pattern) is a
// subset of L(relaxed):
//
//   - deleting a zero-width lookaround removes a constraint on the match;
//   - turning an atomic group (?>…) into (?:…) only restores backtracking
//     paths.
//
// That direction is the one the index needs. A trigram the relaxed pattern
// requires is therefore required by the original too, so no file that really
// matches can be filtered out. Narrowing would be a correctness bug — it could
// drop a genuine hit — which is why anything ambiguous gives up instead of
// being rewritten optimistically. Backreferences, conditionals, \K and \G all
// bail for that reason, as do possessive quantifiers: a '+' is not reliably
// distinguishable from a repeat, and dropping the wrong one would narrow an
// anchored pattern.

// groupKind classifies what a '(' opens.
type groupKind int

const (
	// groupLookaround is (?=, (?!, (?<= or (?<! — a zero-width assertion.
	groupLookaround groupKind = iota
	// groupAtomic is (?> — an atomic group.
	groupAtomic
	// groupUnsupported has no safe relaxation.
	groupUnsupported
	// groupPlain is a capturing, named, or plain non-capturing group.
	groupPlain
)

// Relax rewrites a PCRE-style pattern into a more permissive one the standard
// parser accepts. ok is false when the pattern uses a construct that cannot be
// widened safely, in which case the caller must scan every file.
func Relax(pattern string) (relaxed string, ok bool) {
	chars := []rune(pattern)
	out := make([]rune, 0, len(chars))
	i := 0
	// Whether the previous token was an unescaped quantifier, which is what
	// makes a following '+' a possessive modifier rather than a repeat.
	afterQuantifier := false

	for i < len(chars) {
		c := chars[i]

		if c == '\\' {
			if i+1 >= len(chars) {
				return "", false // a trailing backslash is not a pattern
			}
			next := chars[i+1]
			// \1-\9, \k and \g are backreferences; \K resets the match start
			// and \G anchors to the previous match. None has an equivalent
			// here, and none can be widened.
			if (next >= '0' && next <= '9') || next == 'k' || next == 'g' || next == 'K' || next == 'G' {
				return "", false
			}
			out = append(out, c, next)
			i += 2
			afterQuantifier = false
			continue
		}

		switch {
		case c == '[':
			// A character class is copied verbatim. Its extent has to be found
			// exactly, because (?=, (?<! and (?> are ordinary members inside
			// one — treating them as syntax would delete real class members
			// and narrow the pattern.
			end, found := skipClass(chars, i)
			if !found {
				return "", false
			}
			out = append(out, chars[i:end]...)
			i = end
			afterQuantifier = false

		case c == '(':
			switch classifyGroup(chars, i) {
			case groupLookaround:
				end, found := skipGroup(chars, i)
				if !found {
					return "", false
				}
				i = end
			case groupAtomic:
				out = append(out, '(', '?', ':')
				i += 3
			case groupUnsupported:
				return "", false
			default:
				out = append(out, c)
				i++
			}
			afterQuantifier = false

		case c == '{':
			// Only a real {n,m} counts as a quantifier. A brace that is part
			// of \p{L} or a literal is not, and treating it as one would bail
			// out of perfectly relaxable patterns.
			if end, found := repetitionEnd(chars, i); found {
				out = append(out, chars[i:end]...)
				i = end
				afterQuantifier = true
			} else {
				out = append(out, c)
				i++
				afterQuantifier = false
			}

		case c == '+' && afterQuantifier:
			// A possessive quantifier. Rewriting it means deciding which '+'
			// is the modifier, which is not always knowable; bail instead.
			return "", false

		default:
			out = append(out, c)
			i++
			afterQuantifier = c == '*' || c == '+' || c == '?'
		}
	}

	return string(out), true
}

// skipClass returns the index just past the ']' closing the class opened at
// chars[open].
//
// This has to agree with the engines it feeds, which share two rules: a ']' in
// the first position (after an optional '^') is a literal member rather than
// the terminator, and '[' nests, so [[:digit:]] closes on the second ']'.
//
// Erring long is safe — the extra text is copied verbatim, and any lookaround
// swallowed with it survives into a pattern the parser rejects, which degrades
// to a full scan. Erring short is the dangerous direction, because the rest of
// the class body would then be reinterpreted as regex syntax.
func skipClass(chars []rune, open int) (int, bool) {
	i := open + 1
	if i < len(chars) && chars[i] == '^' {
		i++
	}
	if i < len(chars) && chars[i] == ']' {
		i++
	}

	depth := 1
	for i < len(chars) {
		switch chars[i] {
		case '\\':
			i++
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return i + 1, true
			}
		}
		i++
	}
	return 0, false
}

// repetitionEnd returns the index just past the '}' of a {n}, {n,} or {n,m}
// starting at chars[open], and whether this brace is one at all.
func repetitionEnd(chars []rune, open int) (int, bool) {
	i := open + 1
	digits := func() bool {
		start := i
		for i < len(chars) && chars[i] >= '0' && chars[i] <= '9' {
			i++
		}
		return i > start
	}

	if !digits() {
		return 0, false
	}
	if i < len(chars) && chars[i] == ',' {
		i++
		digits()
	}
	if i < len(chars) && chars[i] == '}' {
		return i + 1, true
	}
	return 0, false
}

// classifyGroup decides what the '(' at chars[open] opens.
func classifyGroup(chars []rune, open int) groupKind {
	at := func(i int) (rune, bool) {
		if i < len(chars) {
			return chars[i], true
		}
		return 0, false
	}

	if c, ok := at(open + 1); !ok || c != '?' {
		return groupPlain
	}
	c, ok := at(open + 2)
	if !ok {
		return groupPlain
	}
	switch c {
	case '=', '!':
		return groupLookaround
	case '<':
		// (?<= and (?<! are lookbehind; (?<name> is just a named group.
		if n, ok := at(open + 3); ok && (n == '=' || n == '!') {
			return groupLookaround
		}
		return groupPlain
	case '>':
		return groupAtomic
	case '(':
		// (?(1)yes|no) — a conditional.
		return groupUnsupported
	case 'P':
		// (?P=name) is a backreference; (?P<name> is a named group.
		if n, ok := at(open + 3); ok && n == '=' {
			return groupUnsupported
		}
	case '#':
		// (?#…) is a comment, whose body is arbitrary text rather than syntax.
		return groupUnsupported
	}
	// (?x) switches on verbose mode, which changes how whitespace and '#' are
	// lexed for the rest of the pattern. This scanner does not model that, so
	// a (?=-looking sequence inside an x-mode comment would be read as a real
	// lookaround. Bail rather than reason about it.
	if enablesExtendedMode(chars, open+2) {
		return groupUnsupported
	}
	return groupPlain
}

// enablesExtendedMode reports whether the (?… at start is an inline flag group
// that turns on x.
//
// Only a group made up entirely of flag letters counts, terminated by ')' or
// ':'; anything else is some other construct and is left alone.
func enablesExtendedMode(chars []rune, start int) bool {
	i := start
	negated := false
	extended := false
	for i < len(chars) {
		switch c := chars[i]; c {
		case '-':
			negated = true
		case ')', ':':
			return extended
		case 'i', 'm', 's', 'U', 'u', 'a':
			// An ordinary flag letter, carrying no consequence here.
		case 'x':
			if negated {
				extended = false
			} else {
				extended = true
			}
		default:
			return false // not a flag group at all
		}
		i++
	}
	return false
}

// skipGroup returns the index just past the ')' closing the group opened at
// chars[open].
func skipGroup(chars []rune, open int) (int, bool) {
	// Starts at the opening paren, so depth is 1 before any ')' is seen and
	// the decrement below cannot underflow.
	depth := 0
	i := open

	for i < len(chars) {
		switch chars[i] {
		case '\\':
			i += 2
			continue
		case '[':
			// A class body can contain unescaped parentheses, so it has to be
			// skipped as a unit or the depth count goes wrong.
			end, found := skipClass(chars, i)
			if !found {
				return 0, false
			}
			i = end
			continue
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i + 1, true
			}
		}
		i++
	}
	return 0, false
}

// BuildRelaxedMulti plans PCRE-style patterns by relaxing each one first.
//
// A pattern that cannot be relaxed, or that still fails to parse afterwards,
// degrades the whole plan to MatchAll. Relaxation failure is never an error:
// it costs only the index optimisation, and the caller still runs the real
// matcher over whatever candidates come back.
func BuildRelaxedMulti(patterns []string, caseInsensitive bool) Plan {
	plans := make([]Plan, 0, len(patterns))
	for _, pattern := range patterns {
		relaxed, ok := Relax(pattern)
		if !ok {
			return MatchAll()
		}
		plan, err := Build(relaxed, caseInsensitive)
		if err != nil || plan.IsMatchAll() {
			return MatchAll()
		}
		plans = append(plans, plan)
	}
	switch len(plans) {
	case 0:
		return MatchAll()
	case 1:
		return plans[0]
	default:
		return Plan{Kind: KindOr, Branches: plans}
	}
}
