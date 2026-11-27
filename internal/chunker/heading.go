package chunker

import (
	"strings"
	"unicode/utf8"
)

// heading is one boundary that starts a section: an ATX heading, or one of
// the heuristic tier's boundaries.
type heading struct {
	// start is the byte offset of the heading line's start, including up to
	// three spaces of indentation, so a Chunk beginning here keeps them.
	start int
	// level is 1 for "#" through 6 for "######", and for the heuristic tier
	// the depth of the marker (Phần 1 down to Điều 4).
	level int
	// text is the heading's text: markers, a closing run of hashes and
	// surrounding whitespace stripped.
	text string
	// keepPath marks a heuristic boundary that carries no heading text, a
	// page break or separator line: the new section keeps the breadcrumb it
	// had and the boundary adds nothing to the path.
	keepPath bool
	// end is the byte offset just past the heading's last line when its text
	// spans more than the first line, as a legal marker's title on the next
	// line does. Zero means the heading occupies its first line alone.
	end int
}

// headings returns the ATX headings of text in document order.
//
// Only lines outside protected spans count, so a "# comment" inside a fenced
// code block or a math block is content, not structure. Setext headings (a
// text line underlined with === or ---) are out of scope: the parser is
// line-local and has no use for them in the Corpora this serves.
func headings(text string) []heading {
	protected := protectedSpans(text)
	var out []heading
	lines(text, 0, len(text), func(start, end int) {
		line := text[start:end]
		level, body, ok := parseATXHeading(line)
		if !ok {
			return
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if insideSpan(protected, start+indent) {
			return
		}
		out = append(out, heading{start: start, level: level, text: body})
	})
	return out
}

// parseATXHeading parses one line as an ATX heading: up to three leading
// spaces, one to six "#", then either the line's end or a space or tab and
// the heading text. A closing run of "#" preceded by whitespace, or making up
// the whole text, is stripped (CommonMark). ok is false for any other line.
func parseATXHeading(line string) (level int, text string, ok bool) {
	line = strings.TrimSuffix(line, "\r")
	indent := 0
	for indent < len(line) && line[indent] == ' ' {
		indent++
	}
	if indent > 3 {
		return 0, "", false
	}
	rest := line[indent:]
	level = len(rest) - len(strings.TrimLeft(rest, "#"))
	if level == 0 || level > 6 {
		return 0, "", false
	}
	after := rest[level:]
	if after == "" {
		return level, "", true
	}
	if after[0] != ' ' && after[0] != '\t' {
		return 0, "", false
	}
	text = strings.TrimSpace(after)
	if end := len(text); end > 0 && text[end-1] == '#' {
		start := end
		for start > 0 && text[start-1] == '#' {
			start--
		}
		// "foo#" is text; "foo ##" and "###" are a closing sequence.
		if start == 0 || text[start-1] == ' ' || text[start-1] == '\t' {
			text = strings.TrimSpace(text[:start])
		}
	}
	return level, text, true
}

// headingSection is one part the heading tier emits Chunks for: the text
// from a heading line (or the Document start) up to the next heading line,
// with the heading path it sits under.
type headingSection struct {
	start, end int // byte offsets into the whole text
	// breadcrumb is the path including this section's own heading; later
	// Chunks of the section carry it because their Content lacks the heading.
	breadcrumb string
	// ancestors is the path above this section's own heading. Its first
	// Chunk starts with that heading line, so it carries only the ancestors.
	ancestors string
	// bounded is false for the preamble before the first boundary. A
	// heuristic page break or separator counts: the section starts at a line
	// the tier chose, not at arbitrary text.
	bounded bool
}

// headingAncestor is one heading on the stack a breadcrumb is built from.
type headingAncestor struct {
	level int
	text  string
}

// headingSections returns the sections that hold body text, in document
// order. A heading whose section is blank up to the next heading produces no
// section: it only nests the headings below it.
func headingSections(text string) []headingSection {
	return sectionsFor(text, headings(text))
}

// sectionsFor is headingSections with hs already scanned, so the profiler
// can measure the same sections the tier emits without scanning twice.
func sectionsFor(text string, hs []heading) []headingSection {
	var stack []headingAncestor
	breadcrumb := func() string {
		parts := make([]string, 0, len(stack))
		for _, h := range stack {
			// A heading with no text nests but adds nothing to the path.
			if h.text != "" {
				parts = append(parts, h.text)
			}
		}
		return strings.Join(parts, " > ")
	}
	var sections []headingSection
	add := func(start, end, bodyStart int, bounded bool, ancestors string) {
		if strings.TrimSpace(text[bodyStart:end]) == "" {
			return
		}
		sections = append(sections, headingSection{
			start:      start,
			end:        end,
			breadcrumb: breadcrumb(),
			ancestors:  ancestors,
			bounded:    bounded,
		})
	}

	if len(hs) == 0 {
		add(0, len(text), 0, false, "")
		return sections
	}
	if hs[0].start > 0 {
		// Text before the first heading is its own section.
		add(0, hs[0].start, 0, false, "")
	}
	for i, h := range hs {
		end := len(text)
		if i+1 < len(hs) {
			end = hs[i+1].start
		}
		if h.keepPath {
			// A page break or separator starts a new section without a
			// heading of its own. Its Content holds no heading line, so
			// every Chunk carries the path it had.
			add(h.start, end, h.start, true, breadcrumb())
			continue
		}
		for len(stack) > 0 && stack[len(stack)-1].level >= h.level {
			stack = stack[:len(stack)-1]
		}
		ancestors := breadcrumb()
		stack = append(stack, headingAncestor{level: h.level, text: h.text})
		bodyStart := end
		if h.end > h.start && h.end <= end {
			// A marker whose title is on the line below: the section's body
			// starts after the title, so a marker with nothing else emits no
			// Chunk and only feeds the breadcrumb.
			bodyStart = h.end
		} else if nl := strings.IndexByte(text[h.start:end], '\n'); nl >= 0 {
			bodyStart = h.start + nl + 1
		}
		add(h.start, end, bodyStart, true, ancestors)
	}
	return sections
}

// splitHeading is the heading tier. It splits text at ATX headings, so a
// section runs from its heading line to the next heading of any level, and
// puts the section's heading path in ContextHeader. A section that fits
// ChunkSize together with its ContextHeader is one Chunk; a larger one is
// split with the recursive tier's machinery over that section's text only,
// so overlap stays inside the section and the budget leaves room for the
// breadcrumb. Sections are never merged, so a heading line can be left out
// of every Chunk when its section has no body; ValidateChunks checks each
// Chunk's own range, not that the Chunks cover the text, and a Chunk holding
// only a heading would also count towards fragmentation.
//
// ContextHeader holds only what Content does not: a section's first Chunk
// starts with its own heading line, so it carries the ancestors alone; later
// Chunks, whose Content no longer holds the heading, carry the full path.
// cfg must have its defaults applied.
func splitHeading(text string, cfg SplitterConfig) []Chunk {
	return splitSections(text, cfg, headingSections(text))
}

// splitSections turns each section into Chunks the same way for every
// structured tier: one Chunk when the section fits its budget, otherwise
// pieces from the recursive tier's machinery over that section's text alone.
// Sections are never merged, so a boundary line can be left out of every
// Chunk when its section has no body; a Chunk holding only a heading would
// also count towards fragmentation. cfg must have its defaults applied.
func splitSections(text string, cfg SplitterConfig, sections []headingSection) []Chunk {
	var out []Chunk
	idx := runeIndex{text: text}
	caps := splitCaps{protected: protectedLimit(cfg, text), table: cfg.ChunkSize}
	for _, sec := range sections {
		base := idx.runeOffset(sec.start)
		content := text[sec.start:sec.end]
		n := utf8.RuneCountInString(content)
		if n <= sectionBudget(cfg, sec.ancestors) {
			out = append(out, Chunk{
				Content:       content,
				ContextHeader: sec.ancestors,
				Start:         base,
				End:           base + n,
			})
			continue
		}
		sectionCfg := cfg
		sectionCfg.ChunkSize = sectionBudget(cfg, sec.breadcrumb)
		sectionCfg.ChunkOverlap = min(cfg.ChunkOverlap, sectionCfg.ChunkSize/2)
		for _, c := range splitRecursiveLimited(content, sectionCfg, caps) {
			table := c.ContextHeader
			if c.Start == 0 {
				c.ContextHeader = joinContext(sec.ancestors, table)
			} else {
				c.ContextHeader = joinContext(sec.breadcrumb, table)
			}
			c.Start += base
			c.End += base
			out = append(out, c)
		}
	}
	for i := range out {
		out[i].Seq = i
	}
	return out
}

// joinContext stacks the path a structured tier supplies above the table
// header the splitter carried, either of which may be empty. EmbeddingContent
// separates ContextHeader from Content with a blank line, so the two are
// joined the same way.
func joinContext(path, header string) string {
	switch {
	case path == "":
		return header
	case header == "":
		return path
	default:
		return path + contextSeparator + header
	}
}

// contextCost is how many runes of a Chunk's budget its ContextHeader takes:
// the header's runes plus the blank line EmbeddingContent joins it with, or 0
// when there is no header.
func contextCost(header string) int {
	if header == "" {
		return 0
	}
	return utf8.RuneCountInString(header) + utf8.RuneCountInString(contextSeparator)
}

// sectionBudget is the most Content runes a section's Chunk may hold so that
// EmbeddingContent, which prepends ContextHeader and a blank line, fits
// ChunkSize. A header may not shrink Chunks below half the target: a path or
// table header that long is over budget on its own, and pieces that small
// would fragment. The 1 keeps a tiny ChunkSize from producing zero-length
// pieces. A Chunk that is one protected span keeps the Document's protected
// limit instead, because ValidateChunks requires such spans whole.
func sectionBudget(cfg SplitterConfig, header string) int {
	cost := contextCost(header)
	if cost == 0 {
		return cfg.ChunkSize
	}
	return max(1, cfg.ChunkSize/2, cfg.ChunkSize-cost)
}
