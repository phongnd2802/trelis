package chunker

import (
	"strings"
	"unicode/utf8"
)

// maxProtectedSize is the most runes a protected span may hold before it is
// cut anyway: one Chunk of a giant table or code block would be truncated
// by any embedding model. With a TokenLimit the cap is that limit's rune
// budget instead, when lower.
const maxProtectedSize = 7500

// protectedLimit is the most runes a protected span of text may hold and
// still be kept whole under cfg.
func protectedLimit(cfg SplitterConfig, text string) int {
	if cfg.TokenLimit > 0 {
		return min(maxProtectedSize, CharsForTokenLimit(cfg.TokenLimit, tokenLanguage(cfg, text)))
	}
	return maxProtectedSize
}

// forceSplitLookback is how far back from a forced cut the splitter looks
// for a line break or space to cut after instead.
const forceSplitLookback = 200

// unit is a piece of the text the merge step never cuts: a protected span,
// or a separator-split piece of the text between them. Offsets are bytes;
// runes is its length in runes.
type unit struct {
	start, end, runes int
}

// splitRecursive is the recursive tier. It splits the text between
// protected spans at cfg.Separators, highest priority first, going down a
// level only for pieces still over ChunkSize; cuts what is left over by
// force; keeps protected spans as whole units; and then packs consecutive
// units into Chunks of at most ChunkSize runes, each starting with the
// overlap from the one before. cfg must have its defaults applied.
func splitRecursive(text string, cfg SplitterConfig) []Chunk {
	s := splitter{
		text:      text,
		cfg:       cfg,
		protected: protectedSpans(text),
		hardMax:   protectedLimit(cfg, text),
	}
	return s.merge(s.units())
}

type splitter struct {
	text      string
	cfg       SplitterConfig
	protected []span
	hardMax   int
	out       []unit
}

func (s *splitter) runes(start, end int) int {
	return utf8.RuneCountInString(s.text[start:end])
}

// units splits the whole text into units, in order and without gaps.
func (s *splitter) units() []unit {
	from := 0
	for _, p := range s.protected {
		s.splitFree(from, p.start, s.cfg.Separators)
		if n := s.runes(p.start, p.end); n > s.hardMax {
			s.forceSplit(p.start, p.end, s.hardMax)
		} else {
			s.out = append(s.out, unit{p.start, p.end, n})
		}
		from = p.end
	}
	s.splitFree(from, len(s.text), s.cfg.Separators)
	return s.out
}

// splitFree splits unprotected text[start:end] at the first of seps it
// holds. Each separator becomes a unit of its own, so the merge step can
// end a Chunk right after it; a piece still over ChunkSize is split at the
// remaining, lower-priority separators, and then by force.
func (s *splitter) splitFree(start, end int, seps []string) {
	if start >= end {
		return
	}
	n := s.runes(start, end)
	if n <= s.cfg.ChunkSize {
		s.out = append(s.out, unit{start, end, n})
		return
	}
	for i, sep := range seps {
		if sep == "" || !strings.Contains(s.text[start:end], sep) {
			continue
		}
		for at := start; at < end; {
			j := strings.Index(s.text[at:end], sep)
			if j < 0 {
				s.splitFree(at, end, seps[i+1:])
				break
			}
			s.splitFree(at, at+j, seps[i+1:])
			s.splitFree(at+j, at+j+len(sep), nil)
			at += j + len(sep)
		}
		return
	}
	s.forceSplit(start, end, s.cfg.ChunkSize)
}

// forceSplit cuts text[start:end] into units of at most limit runes, each
// cut moved back to just after a line break or space when one is close.
func (s *splitter) forceSplit(start, end, limit int) {
	for start < end {
		cut, n := start, 0
		for cut < end && n < limit {
			_, size := utf8.DecodeRuneInString(s.text[cut:end])
			cut += size
			n++
		}
		if cut < end {
			window := s.text[start:cut]
			back := strings.LastIndexAny(window, "\n ")
			if back >= 0 && s.runes(start+back+1, cut) <= forceSplitLookback && back+1 < len(window) {
				cut = start + back + 1
				n = s.runes(start, cut)
			}
		}
		s.out = append(s.out, unit{start, cut, n})
		start = cut
	}
}

// merge packs units into Chunks of at most ChunkSize runes. A unit over
// ChunkSize (a protected span) becomes a Chunk of its own.
func (s *splitter) merge(units []unit) []Chunk {
	var pieces []span
	cur := unit{}
	empty := true
	for _, u := range units {
		if !empty && cur.runes+u.runes > s.cfg.ChunkSize {
			pieces = append(pieces, span{cur.start, cur.end})
			cur, empty = s.overlap(cur, u.runes)
		}
		if empty {
			cur, empty = unit{u.start, u.end, u.runes}, false
			continue
		}
		cur.end = u.end
		cur.runes += u.runes
	}
	if !empty {
		pieces = append(pieces, span{cur.start, cur.end})
	}
	return s.chunks(pieces)
}

// chunks turns byte ranges into Chunks with rune offsets, dropping ranges
// that hold only whitespace.
func (s *splitter) chunks(pieces []span) []Chunk {
	starts, ends := runeIndex{text: s.text}, runeIndex{text: s.text}
	var out []Chunk
	for _, p := range pieces {
		content := s.text[p.start:p.end]
		if strings.TrimSpace(content) == "" {
			continue
		}
		out = append(out, Chunk{
			Content: content,
			Seq:     len(out),
			Start:   starts.runeOffset(p.start),
			End:     ends.runeOffset(p.end),
		})
	}
	return out
}

// overlapLookbehind is how many runes before the overlap window are also
// searched, so a separator the window cuts in half (at most "\r\n\r\n")
// is still found. Only its last rune may lie before the window.
const overlapLookbehind = 4

// overlap returns the tail of cur that the next Chunk repeats, and whether
// there is none. The tail starts right after a semantic boundary within
// the last ChunkOverlap runes, never more than leaves room for the next
// unit: a paragraph break if there is one, else a line break, else a
// sentence end, the earliest of its kind so the overlap is as long as
// allowed. A boundary inside a protected span, or with only whitespace
// after it, does not count. With no boundary there is no overlap.
func (s *splitter) overlap(cur unit, nextRunes int) (unit, bool) {
	limit := min(s.cfg.ChunkOverlap, s.cfg.ChunkSize-nextRunes)
	if limit <= 0 {
		return unit{}, true
	}
	// Byte offsets of the last limit+lookbehind runes of cur, and of its end.
	var pos []int
	for at, n := cur.end, 0; at > cur.start && n < limit+overlapLookbehind; n++ {
		_, size := utf8.DecodeLastRuneInString(s.text[cur.start:at])
		at -= size
		pos = append(pos, at)
	}
	for i, j := 0, len(pos)-1; i < j; i, j = i+1, j-1 {
		pos[i], pos[j] = pos[j], pos[i]
	}
	pos = append(pos, cur.end)
	window := len(pos) - 1           // runes in the window
	earliest := max(0, window-limit) // earliest rune index the overlap may start at

	best, bestPriority := -1, 0
	consider := func(endRune, priority int) {
		if endRune < earliest || endRune >= window ||
			insideSpan(s.protected, pos[endRune]) ||
			strings.TrimSpace(s.text[pos[endRune]:cur.end]) == "" {
			return
		}
		if best < 0 || priority < bestPriority {
			best, bestPriority = endRune, priority
		}
	}
	at := func(i int) string { return s.text[pos[i]:pos[i+1]] }
	for i := 0; i < window; i++ {
		switch {
		case i+1 < window && at(i) == "\n" && at(i+1) == "\n":
			consider(i+2, 1)
		case i+3 < window && at(i) == "\r" && at(i+1) == "\n" && at(i+2) == "\r" && at(i+3) == "\n":
			consider(i+4, 1)
		case at(i) == "\n" && (i == 0 || at(i-1) != "\n") && (i+1 == window || at(i+1) != "\n"):
			consider(i+1, 2)
		case strings.Contains("。？！", at(i)):
			consider(i+1, 3)
		case i+1 < window && strings.Contains(".?!", at(i)) && at(i+1) == " ":
			consider(i+2, 3)
		}
	}
	if best < 0 {
		return unit{}, true
	}
	return unit{pos[best], cur.end, window - best}, false
}
