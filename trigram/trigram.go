// Package trigram implements trigram extraction and hashing.
//
// A trigram is every overlapping 3-byte window in a byte sequence, packed into
// a uint32 as (a<<16)|(b<<8)|c. That packing is injective over 24 bits, so the
// "hash" collides with nothing and needs no mixing: the trigram IS its key.
//
// Beyond the bare trigram, each occurrence contributes two 8-bit masks that
// let a query reject a file without ever opening it:
//
//   - locMask records the byte offset modulo 8, so two trigrams that must be
//     adjacent in a literal can be checked with a rotate-and-AND.
//   - nextMask is a Bloom filter of the byte that follows the trigram, so a
//     literal's fourth character can be tested against a file that only ever
//     promised to contain the first three.
//
// Neither mask can produce a false negative, which is the only property that
// matters: a candidate wrongly kept costs one file read, a candidate wrongly
// dropped is a missing search result.
package trigram

// Hash packs three bytes into a single trigram key.
func Hash(a, b, c byte) uint32 {
	return uint32(a)<<16 | uint32(b)<<8 | uint32(c)
}

// Masks are one file's per-trigram evidence.
type Masks struct {
	// LocMask has bit i set when the trigram occurs at some offset where
	// offset%8 == i.
	LocMask uint8
	// NextMask is an 8-bit Bloom filter of the bytes immediately following
	// this trigram anywhere in the file.
	NextMask uint8
}

// Entry is a trigram together with the masks merged across one file.
type Entry struct {
	Hash  uint32
	Masks Masks
}

// locBit maps a byte offset to its bit in a LocMask.
//
// offset%8 rather than a hash: consecutive offsets must land on adjacent bits
// for the rotate-and-AND adjacency test in CanBeAdjacent to mean anything.
func locBit(offset int) uint8 {
	return 1 << uint(offset%8)
}

// BloomHash maps a byte to its bit in a NextMask.
//
// A multiplicative hash rather than the low three bits: ASCII text clusters
// hard in the low bits (every lowercase letter, every digit), and three bits
// taken straight off the bottom would put most of a source file into two or
// three of the eight buckets.
func BloomHash(b byte) uint8 {
	return 1 << ((b * 0x9E >> 5) & 0x07)
}

// Extract returns every distinct trigram in data, in first-occurrence order.
func Extract(data []byte) []uint32 {
	if len(data) < 3 {
		return nil
	}
	seen := make(map[uint32]struct{}, len(data)/4)
	out := make([]uint32, 0, len(data)/4)
	for i := 0; i+3 <= len(data); i++ {
		h := Hash(data[i], data[i+1], data[i+2])
		if _, dup := seen[h]; dup {
			continue
		}
		seen[h] = struct{}{}
		out = append(out, h)
	}
	return out
}

// ExtractWithMasks returns every distinct trigram in data with its masks
// merged across all occurrences, in first-occurrence order.
//
// Order matters to the caller, not to correctness: the builder sorts before
// writing, but a stable order keeps spill segments reproducible, which is what
// makes two builds of the same tree compare byte for byte.
func ExtractWithMasks(data []byte) []Entry {
	if len(data) < 3 {
		return nil
	}
	// A map rather than a 16M-entry array: a real file holds a few thousand
	// distinct trigrams, and allocating 16 MiB per file to avoid a map lookup
	// loses badly on every input that isn't pathological.
	at := make(map[uint32]int, len(data)/4)
	out := make([]Entry, 0, len(data)/4)
	for i := 0; i+3 <= len(data); i++ {
		h := Hash(data[i], data[i+1], data[i+2])
		idx, ok := at[h]
		if !ok {
			idx = len(out)
			at[h] = idx
			out = append(out, Entry{Hash: h})
		}
		out[idx].Masks.LocMask |= locBit(i)
		// The following byte exists only when the window is not the last one.
		if i+3 < len(data) {
			out[idx].Masks.NextMask |= BloomHash(data[i+3])
		}
	}
	return out
}

// ExtractMergedMasks extracts trigrams from both the content and its ASCII
// lowercasing, merging the masks per trigram. This is the extraction the
// builder and the live overlay both use, and it is what lets one index answer
// case-sensitive and case-insensitive queries alike.
//
// The two cases are folded into a single pass rather than run twice over a
// lowercased copy. Peak memory then stays independent of file size, which
// matters because the builder holds several whole files in flight at once, and
// most windows in real source are already lowercase — for those the lowered
// key is the same key, collapsing the second pass into one extra OR on an
// entry already in hand.
//
// This is equivalent to extracting from a lowercased duplicate: ASCII
// lowercasing is 1:1 on bytes, so every offset (and therefore every locBit) is
// unchanged, and OR is associative, so merging per window lands on the same
// masks as merging two completed sets.
func ExtractMergedMasks(content []byte) map[uint32]Masks {
	out := make(map[uint32]Masks)
	if len(content) < 3 {
		return out
	}

	hasUpper := false
	for _, b := range content {
		if b >= 'A' && b <= 'Z' {
			hasUpper = true
			break
		}
	}

	n := len(content)
	for i := 0; i+3 <= n; i++ {
		tri := Hash(content[i], content[i+1], content[i+2])
		loc := locBit(i)
		var next uint8
		if i+3 < n {
			next = BloomHash(content[i+3])
		}

		if !hasUpper {
			// No uppercase anywhere means the lowercased pass would recompute
			// this identical entry for every window.
			m := out[tri]
			m.LocMask |= loc
			m.NextMask |= next
			out[tri] = m
			continue
		}

		lowered := Hash(lower(content[i]), lower(content[i+1]), lower(content[i+2]))
		// The lowercased pass lowercases the FOLLOWING byte too, so an
		// otherwise-lowercase window followed by an uppercase byte still
		// contributes a second nextMask bit to the same entry.
		var nextLowered uint8
		if i+3 < n {
			nextLowered = BloomHash(lower(content[i+3]))
		}

		m := out[tri]
		m.LocMask |= loc
		if lowered == tri {
			m.NextMask |= next | nextLowered
			out[tri] = m
			continue
		}
		m.NextMask |= next
		out[tri] = m

		lm := out[lowered]
		lm.LocMask |= loc
		lm.NextMask |= nextLowered
		out[lowered] = lm
	}

	return out
}

func lower(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + 32
	}
	return b
}

// IsBinary reports whether data looks binary, by the same rule the Rust
// implementation uses: a NUL byte anywhere in the first 8KB.
func IsBinary(data []byte) bool {
	n := min(len(data), 8192)
	for _, b := range data[:n] {
		if b == 0 {
			return true
		}
	}
	return false
}

// CheckAdjacency reports whether a run of trigrams taken from consecutive
// offsets of one literal could all appear at those relative offsets in a file,
// judged only by their location masks.
func CheckAdjacency(masks []Masks) bool {
	for i := 1; i < len(masks); i++ {
		if !CanBeAdjacent(masks[i-1], masks[i]) {
			return false
		}
	}
	return true
}

// CanBeAdjacent reports whether a trigram carrying first could sit exactly one
// byte before a trigram carrying second, judged only by their location masks.
//
// The masks record offset%8, so shifting the first mask up by one position
// (with wraparound, since the residues are cyclic) and intersecting with the
// second answers "is there any offset pair one apart that both trigrams
// actually occupy". A zero intersection is proof they are never adjacent; a
// non-zero one is only a hint, because two occurrences 8 bytes apart share a
// residue with two occurrences 1 byte apart.
func CanBeAdjacent(first, second Masks) bool {
	rotated := first.LocMask<<1 | first.LocMask>>7
	return rotated&second.LocMask != 0
}

// MayBeFollowedBy reports whether b could be the byte immediately after a
// trigram with these masks. False means it definitely never is.
func (m Masks) MayBeFollowedBy(b byte) bool {
	return m.NextMask&BloomHash(b) != 0
}
