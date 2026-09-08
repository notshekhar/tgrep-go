// Package encoding turns raw file bytes into the text that gets indexed and
// searched.
//
// It exists because a file is not always UTF-8, and a search that reads raw
// bytes silently cannot find anything in the ones that are not. A UTF-16 file
// is the clear case: its ASCII text is interleaved with NUL bytes, so a
// byte-oriented reader classifies it as binary and skips it — the user gets no
// match and no explanation.
//
// The rules mirror ripgrep's, which is what the reference implementation
// follows too:
//
//   - A UTF-8 or UTF-16 byte-order mark is sniffed and the file transcoded
//     accordingly. No other detection is attempted; guessing an encoding from
//     content is unreliable and ripgrep deliberately does not.
//   - Otherwise the bytes are treated as UTF-8, with invalid sequences
//     repaired to U+FFFD rather than rejected.
//
// Repairing changes offsets — one bad byte becomes three — so a Fixups table
// records where, letting a caller map a position in the decoded text back to
// the byte in the file it came from. Without that, reported columns would
// point into the wrong place in every file that needed repair.
package encoding

import (
	"unicode/utf16"
	"unicode/utf8"
)

// Mode selects how bytes are decoded.
type Mode int

const (
	// Auto sniffs a BOM and otherwise treats the bytes as UTF-8. This is the
	// mode the index is always built in.
	Auto Mode = iota
	// None disables sniffing: the raw bytes are searched as they are.
	None
)

// replacementLen is the encoded length of U+FFFD.
const replacementLen = 3

// BOMs, longest first — UTF-8's is three bytes and the UTF-16 ones are two, so
// order matters when testing prefixes.
var (
	bomUTF8    = []byte{0xEF, 0xBB, 0xBF}
	bomUTF16LE = []byte{0xFF, 0xFE}
	bomUTF16BE = []byte{0xFE, 0xFF}
)

// hasPrefix avoids importing bytes for one three-byte comparison.
func hasPrefix(b, prefix []byte) bool {
	if len(b) < len(prefix) {
		return false
	}
	for i := range prefix {
		if b[i] != prefix[i] {
			return false
		}
	}
	return true
}

// DecodeBytes applies BOM-based transcoding only.
//
// The returned slice aliases the input when nothing had to be transcoded,
// which is very nearly every file; transcoded returns true when it does not.
func DecodeBytes(b []byte, mode Mode) (out []byte, transcoded bool) {
	if mode == None {
		return b, false
	}
	switch {
	case hasPrefix(b, bomUTF8):
		// Already UTF-8; only the mark itself has to go, or it would be
		// indexed as content.
		return b[len(bomUTF8):], false
	case hasPrefix(b, bomUTF16LE):
		return utf16ToUTF8(b[2:], false), true
	case hasPrefix(b, bomUTF16BE):
		return utf16ToUTF8(b[2:], true), true
	}
	return b, false
}

// utf16ToUTF8 transcodes UTF-16 code units to UTF-8.
//
// A trailing odd byte is dropped rather than treated as half a unit: it cannot
// be part of a well-formed pair, and inventing a value for it would put bytes
// in the index that are in no file.
func utf16ToUTF8(b []byte, bigEndian bool) []byte {
	units := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		if bigEndian {
			units = append(units, uint16(b[i])<<8|uint16(b[i+1]))
		} else {
			units = append(units, uint16(b[i+1])<<8|uint16(b[i]))
		}
	}
	// utf16.Decode already replaces unpaired surrogates with U+FFFD, which is
	// the same repair the UTF-8 path makes.
	runes := utf16.Decode(units)
	out := make([]byte, 0, len(runes))
	var buf [utf8.UTFMax]byte
	for _, r := range runes {
		n := utf8.EncodeRune(buf[:], r)
		out = append(out, buf[:n]...)
	}
	return out
}

// Fixups maps offsets in decoded text back to offsets in the source bytes.
type Fixups struct {
	// shifts holds (offset of a replacement in the decoded text, total bytes
	// gained up to and including it), ascending by offset.
	shifts []shift
	// unmappable marks a decode whose offsets bear no relation to the source
	// at all — a transcoded UTF-16 file. Mapping is then the identity, since
	// there is no better answer and pretending otherwise would be a lie
	// dressed as precision.
	unmappable bool
}

type shift struct {
	at     int
	gained int
}

// IsEmpty reports whether any repair was needed.
func (f Fixups) IsEmpty() bool { return len(f.shifts) == 0 }

// ToSourceOffset maps an offset in the decoded text to one in the file.
//
// An offset can land INSIDE a replacement rather than on its boundary: the
// repaired text is what gets searched, so a pattern may match starting
// part-way through one. Such an offset is clamped to the replacement's own
// source position — there is no finer answer, and subtracting a gain the
// offset has not yet realised would underflow.
func (f Fixups) ToSourceOffset(decoded int) int {
	if f.unmappable || len(f.shifts) == 0 {
		return decoded
	}
	// The first shift at or past the offset; everything before it applies.
	lo, hi := 0, len(f.shifts)
	for lo < hi {
		mid := (lo + hi) / 2
		if f.shifts[mid].at < decoded {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo == 0 {
		return decoded
	}
	s := f.shifts[lo-1]
	gainedBefore := 0
	if lo >= 2 {
		gainedBefore = f.shifts[lo-2].gained
	}
	if decoded < s.at+replacementLen {
		return s.at - gainedBefore
	}
	return decoded - s.gained
}

// Decode turns file bytes into searchable text.
//
// Valid UTF-8 with no BOM is returned untouched and aliases the input, which
// is the overwhelmingly common case and must not cost a copy.
func Decode(b []byte, mode Mode) ([]byte, Fixups) {
	body, transcoded := DecodeBytes(b, mode)
	if transcoded {
		// Transcoded output is valid UTF-8 by construction, and its offsets
		// have no correspondence to the source bytes.
		return body, Fixups{unmappable: true}
	}
	if mode == None {
		return body, Fixups{}
	}
	return repairUTF8(body)
}

// repairUTF8 replaces invalid sequences with U+FFFD, recording the shifts.
//
// One U+FFFD per MAXIMAL SUBPART, which is the substitution the Unicode
// standard recommends and what the reference implementation produces. It is
// not the same as one per byte, and the difference is observable: the two
// bytes 0xF0 0x9F are a truncated but well-formed prefix of a four-byte
// sequence and become ONE replacement, while 0xE0 0x80 is a valid start
// followed by a byte that cannot continue it and becomes TWO.
//
// Go's utf8.DecodeRune cannot express this — it reports invalid input one byte
// at a time — so the sequence structure is decoded here. Getting it wrong
// changes which trigrams a damaged file contributes, which is how the two
// implementations' indexes come to disagree.
func repairUTF8(b []byte) ([]byte, Fixups) {
	// Scan first: almost every file is already valid, and that case must cost
	// one pass and no allocation.
	if utf8.Valid(b) {
		return b, Fixups{}
	}

	out := make([]byte, 0, len(b)+16)
	var fx Fixups
	gained := 0
	for i := 0; i < len(b); {
		if r, size := utf8.DecodeRune(b[i:]); r != utf8.RuneError || size > 1 {
			out = append(out, b[i:i+size]...)
			i += size
			continue
		}
		// Invalid here. Consume the maximal well-formed prefix and emit one
		// replacement for it.
		n := maximalSubpart(b[i:])
		gained += replacementLen - n
		fx.shifts = append(fx.shifts, shift{at: len(out), gained: gained})
		out = append(out, 0xEF, 0xBF, 0xBD)
		i += n
	}
	return out, fx
}

// maximalSubpart returns the length of the longest prefix of b that could
// begin a well-formed UTF-8 sequence, given that b does not start with a
// complete one. It is always at least 1, so a scan cannot stall.
//
// The continuation ranges are not uniform — the second byte is restricted for
// 0xE0, 0xED, 0xF0 and 0xF4 to exclude overlong encodings, surrogates, and
// values past U+10FFFF — which is why this cannot be written as "a lead byte
// plus N bytes in 0x80..0xBF".
func maximalSubpart(b []byte) int {
	lo, hi, want := byte(0x80), byte(0xBF), 0
	switch c := b[0]; {
	case c >= 0xC2 && c <= 0xDF:
		want = 1
	case c == 0xE0:
		lo, want = 0xA0, 2
	case c >= 0xE1 && c <= 0xEC, c >= 0xEE && c <= 0xEF:
		want = 2
	case c == 0xED:
		hi, want = 0x9F, 2
	case c == 0xF0:
		lo, want = 0x90, 3
	case c >= 0xF1 && c <= 0xF3:
		want = 3
	case c == 0xF4:
		hi, want = 0x8F, 3
	default:
		// A continuation byte with no lead, or a byte no sequence can start
		// with (0xC0, 0xC1, 0xF5..0xFF).
		return 1
	}

	n := 1
	for range want {
		if n >= len(b) || b[n] < lo || b[n] > hi {
			return n // truncated, or the next byte cannot continue it
		}
		n++
		// Only the SECOND byte carries a restricted range; the rest are the
		// full continuation range.
		lo, hi = 0x80, 0xBF
	}
	return n
}

// IsUTF16 reports whether bytes carry a UTF-16 byte-order mark.
//
// Exported because the binary classifier needs it: a UTF-16 file is full of
// NUL bytes and would otherwise be rejected as binary before anything got the
// chance to transcode it.
func IsUTF16(b []byte) bool {
	return hasPrefix(b, bomUTF16LE) || hasPrefix(b, bomUTF16BE)
}
