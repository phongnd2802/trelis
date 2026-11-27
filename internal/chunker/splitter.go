package chunker

import (
	"strings"
	"unicode/utf8"
)

// maxProtectedSize is the most runes a protected span other than a table
// may hold before it is cut anyway: one Chunk of a giant code block or math
// block would be truncated by any embedding model. Tables are split at row
// boundaries when they pass the Chunk size, so they are never cut inside a
// row unless that row is itself over budget. With a TokenLimit the cap is
// that limit's rune budget instead, when lower.
const maxProtectedSize = 7500

// protectedLimit is the most runes a protected span of text may hold and
// still be kept whole under cfg. It does not apply to tables, which the
// recursive tier splits at rows once they pass the Chunk size.
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
// a table piece, or a separator-split piece of the text between them.
// Offsets are bytes; runes is its length in runes.
type unit struct {
	start, end, runes int
	// header is the ContextHeader a Chunk starting at this unit carries:
	// the header row of a table whose body the unit starts in. It is empty
	// for a unit at the table's first row, which holds the header in its
	// Content, and outside tables.
	header string
}

// splitCaps are the Document-level limits a split inherits. The heading tier
// splits one section at a time, so it passes the caps computed from the
// whole text; a table may stay whole for the Document's Chunk size even when
// a section's own budget is lower.
type splitCaps struct {
	// protected is the most runes a non-table protected span may hold and
	// still be kept whole.
	protected int
	// table is the size above which a table is split at its rows.
	table int
}

// splitRecursive is the recursive tier. It splits the text between
// protected spans at cfg.Separators, highest priority first, going down a
// level only for pieces still over ChunkSize; cuts what is left over by
// force; splits a table over ChunkSize at its rows; keeps other protected
// spans as whole units; and then packs consecutive units into Chunks of at
// most ChunkSize runes, each starting with the overlap from the one before.
// cfg must have its defaults applied.
func splitRecursive(text string, cfg SplitterConfig) []Chunk {
	return splitRecursiveLimited(text, cfg, splitCaps{
		protected: protectedLimit(cfg, text),
		table:     cfg.ChunkSize,
	})
}

// splitRecursiveLimited is splitRecursive with the Document-level caps
// decided by the caller. The heading tier splits one section at a time but
// must keep the whole Document's token-limit and table budgets, which its
// language detection and size set, so it passes the caps computed from the
// full text.
func splitRecursiveLimited(text string, cfg SplitterConfig, caps splitCaps) []Chunk {
	s := splitter{
		text:      text,
		cfg:       cfg,
		protected: protectedSpans(text),
		caps:      caps,
	}
	return s.merge(s.units())
}

type splitter struct {
	text      string
	cfg       SplitterConfig
	protected []span
	caps      splitCaps
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
		n := s.runes(p.start, p.end)
		switch {
		case isTableSpan(s.text, p):
			if n > s.caps.table {
				s.tableUnits(p)
			} else {
				s.out = append(s.out, unit{start: p.start, end: p.end, runes: n})
			}
		case n > s.caps.protected:
			s.forceSplit(p.start, p.end, s.caps.protected)
		default:
			s.out = append(s.out, unit{start: p.start, end: p.end, runes: n})
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
		s.out = append(s.out, unit{start: start, end: end, runes: n})
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

// tableUnits appends the units of a table span too large for one Chunk. Rows
// are packed greedily, from the header down; each unit starts at a row and
// ends at the next row's start, so a Chunk boundary between two units is a
// row boundary. The first unit holds the header in its Content; later units
// carry it as ContextHeader, and their budget leaves room for it. A row that
// alone passes its budget is cut inside the row, and every piece after the
// first carries the header too.
func (s *splitter) tableUnits(p span) {
	rows := tableRows(s.text, p)
	if len(rows) == 0 {
		s.forceSplit(p.start, p.end, s.cfg.ChunkSize)
		return
	}
	header := tableHeader(s.text, rows)
	first := true
	for i := 0; i < len(rows); {
		budget, carry := s.cfg.ChunkSize, ""
		if !first {
			budget = sectionBudget(s.cfg, header)
			carry = header
		}
		start := rows[i].start
		if s.runes(start, rows[i].end) > budget {
			for k, seg := range s.forceCut(start, rows[i].end, budget) {
				segHeader := carry
				if first && k == 0 {
					segHeader = ""
				}
				s.out = append(s.out, s.tableUnit(seg.start, seg.end, segHeader))
			}
			i++
			first = false
			continue
		}
		next := i + 1
		end := p.end
		if next < len(rows) {
			end = rows[next].start
		}
		for k := i + 1; k <= len(rows); k++ {
			boundary := p.end
			if k < len(rows) {
				boundary = rows[k].start
			}
			// The first boundary is a single row plus the line break after
			// it; EmbeddingContent trims that break, so it may pass the
			// budget even when a larger piece would not.
			if k > i+1 && s.runes(start, boundary) > budget {
				break
			}
			end, next = boundary, k
		}
		s.out = append(s.out, s.tableUnit(start, end, carry))
		i = next
		first = false
	}
}

// tableUnit is one unit of a table piece and the header a Chunk starting
// there carries.
func (s *splitter) tableUnit(start, end int, header string) unit {
	return unit{start: start, end: end, runes: s.runes(start, end), header: header}
}

// forceSplit cuts text[start:end] into units of at most limit runes.
func (s *splitter) forceSplit(start, end, limit int) {
	for _, p := range s.forceCut(start, end, limit) {
		s.out = append(s.out, unit{start: p.start, end: p.end, runes: s.runes(p.start, p.end)})
	}
}

// forceCut returns the spans forceSplit would make, in order: pieces of at
// most limit runes, each cut moved back to just after a line break or space
// when one is close.
func (s *splitter) forceCut(start, end, limit int) []span {
	var out []span
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
			}
		}
		out = append(out, span{start: start, end: cut})
		start = cut
	}
	return out
}

// piece is a Chunk's byte range and the ContextHeader it carries.
type piece struct {
	start, end int
	header     string
}

// merge packs units into Chunks of at most ChunkSize runes. A unit over
// ChunkSize (a protected span) becomes a Chunk of its own. A Chunk that
// starts at a table body unit spends its header's runes before the rows, so
// it packs fewer of them.
func (s *splitter) merge(units []unit) []Chunk {
	var pieces []piece
	cur := unit{}
	empty := true
	for _, u := range units {
		if empty {
			cur, empty = u, false
			continue
		}
		gap := s.runes(cur.end, u.start)
		if cur.runes+contextCost(cur.header)+gap+u.runes > s.cfg.ChunkSize {
			pieces = append(pieces, piece{start: cur.start, end: cur.end, header: cur.header})
			next, none := s.overlap(cur, u.runes)
			if none {
				cur, empty = u, false
				continue
			}
			// The overlap starts in text before any table the Chunk holds,
			// so the header of the Chunk it came from does not apply.
			next.header = ""
			cur = next
		}
		cur.end = u.end
		cur.runes += gap + u.runes
	}
	if !empty {
		pieces = append(pieces, piece{start: cur.start, end: cur.end, header: cur.header})
	}
	return s.chunks(pieces)
}

// chunks turns byte ranges into Chunks with rune offsets, dropping ranges
// that hold only whitespace.
func (s *splitter) chunks(pieces []piece) []Chunk {
	starts, ends := runeIndex{text: s.text}, runeIndex{text: s.text}
	var out []Chunk
	for _, p := range pieces {
		content := s.text[p.start:p.end]
		if strings.TrimSpace(content) == "" {
			continue
		}
		out = append(out, Chunk{
			Content:       content,
			ContextHeader: p.header,
			Seq:           len(out),
			Start:         starts.runeOffset(p.start),
			End:           ends.runeOffset(p.end),
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
	return unit{start: pos[best], end: cur.end, runes: window - best}, false
}
