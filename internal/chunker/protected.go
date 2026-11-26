package chunker

import (
	"sort"
	"strings"
)

// SpanKind names what a protected span holds.
type SpanKind string

const (
	SpanTable SpanKind = "table"
	SpanCode  SpanKind = "code"
	SpanMath  SpanKind = "math"
)

// Span is a protected span: a byte range of the text that no Chunk
// boundary may fall inside.
type Span struct {
	Kind  SpanKind
	Start int
	End   int
}

// spans is the sorted, non-overlapping protected spans of one text.
type spans []Span

// ProtectedSpans finds the tables, fenced code blocks and math blocks in
// Markdown text, in order. A span runs over whole lines, less the
// whitespace around it, the same as a Chunk. Anything inside a fenced code
// block is code, even if it looks like a table or math. A fence or math
// block that is never closed protects nothing, so one stray marker cannot
// swallow the rest of a Document.
func ProtectedSpans(text string) []Span {
	lines := splitLines(text)
	var out []Span
	for i := 0; i < len(lines); {
		if end, ok := matchFence(lines, i); ok {
			out = appendSpan(out, text, SpanCode, lines[i].start, lines[end].end)
			i = end + 1
			continue
		}
		if end, ok := matchMath(lines, i); ok {
			out = appendSpan(out, text, SpanMath, lines[i].start, lines[end].end)
			i = end + 1
			continue
		}
		if end, ok := matchTable(lines, i); ok {
			out = appendSpan(out, text, SpanTable, lines[i].start, lines[end].end)
			i = end + 1
			continue
		}
		i++
	}
	return out
}

// inside reports whether pos falls strictly inside a span, so that cutting
// the text at pos would split it.
func (ss spans) inside(pos int) bool {
	i := sort.Search(len(ss), func(i int) bool { return ss[i].End > pos })
	return i < len(ss) && ss[i].Start < pos
}

// only reports whether [start,end) is, up to surrounding whitespace, a
// single protected span.
func (ss spans) only(text string, start, end int) bool {
	start, end = trim(text, start, end)
	i := sort.Search(len(ss), func(i int) bool { return ss[i].End > start })
	return i < len(ss) && ss[i].Start == start && ss[i].End == end
}

func appendSpan(out []Span, text string, kind SpanKind, start, end int) []Span {
	start, end = trim(text, start, end)
	return append(out, Span{Kind: kind, Start: start, End: end})
}

type line struct {
	start, end int // end excludes the line break
	text       string
}

func splitLines(text string) []line {
	var lines []line
	for start := 0; start <= len(text); {
		end := strings.IndexByte(text[start:], '\n')
		if end < 0 {
			lines = append(lines, line{start, len(text), text[start:]})
			break
		}
		end += start
		lines = append(lines, line{start, end, text[start:end]})
		start = end + 1
	}
	return lines
}

// matchFence reports whether lines[i] opens a fenced code block (three or
// more backticks or tildes) and returns the index of its closing line: a
// run of the same character, at least as long, and nothing else.
func matchFence(lines []line, i int) (int, bool) {
	open := strings.TrimSpace(lines[i].text)
	if len(open) < 3 || (open[0] != '`' && open[0] != '~') {
		return 0, false
	}
	marker := open[0]
	n := len(open) - len(strings.TrimLeft(open, string(marker)))
	if n < 3 {
		return 0, false
	}
	for j := i + 1; j < len(lines); j++ {
		closing := strings.TrimSpace(lines[j].text)
		if len(closing) >= n && strings.Trim(closing, string(marker)) == "" {
			return j, true
		}
	}
	return 0, false
}

// matchMath reports whether lines[i] opens a display math block ($$ or \[)
// and returns the index of the line that closes it.
func matchMath(lines []line, i int) (int, bool) {
	open := strings.TrimSpace(lines[i].text)
	var opener, closer string
	switch {
	case strings.HasPrefix(open, "$$"):
		opener, closer = "$$", "$$"
	case strings.HasPrefix(open, `\[`):
		opener, closer = `\[`, `\]`
	default:
		return 0, false
	}
	if len(open) > len(opener) && strings.HasSuffix(open[len(opener):], closer) {
		return i, true
	}
	for j := i + 1; j < len(lines); j++ {
		if strings.HasSuffix(strings.TrimSpace(lines[j].text), closer) {
			return j, true
		}
	}
	return 0, false
}

// matchTable reports whether a pipe table starts at lines[i] and returns
// the index of its last row. A table is a header row followed by a
// delimiter row (as in GFM), or at least two rows that start with a pipe;
// it runs until the first line without a pipe.
func matchTable(lines []line, i int) (int, bool) {
	if i+1 >= len(lines) || !strings.Contains(lines[i].text, "|") {
		return 0, false
	}
	header := isDelimiterRow(lines[i+1].text)
	pipeRows := startsWithPipe(lines[i].text) && startsWithPipe(lines[i+1].text)
	if !header && !pipeRows {
		return 0, false
	}
	end := i + 1
	for end+1 < len(lines) && isTableRow(lines[end+1].text, header) {
		end++
	}
	return end, true
}

func isTableRow(s string, header bool) bool {
	if header {
		return strings.Contains(s, "|") && strings.TrimSpace(s) != ""
	}
	return startsWithPipe(s)
}

func startsWithPipe(s string) bool {
	return strings.HasPrefix(strings.TrimSpace(s), "|")
}

// isDelimiterRow reports whether s is a GFM table delimiter row, such as
// "|---|:--:|" or "--- | ---".
func isDelimiterRow(s string) bool {
	s = strings.TrimSpace(s)
	if !strings.Contains(s, "|") {
		return false
	}
	s = strings.TrimPrefix(strings.TrimSuffix(s, "|"), "|")
	for _, c := range strings.Split(s, "|") {
		c = strings.TrimSpace(c)
		c = strings.TrimSuffix(strings.TrimPrefix(c, ":"), ":")
		if c == "" || strings.Trim(c, "-") != "" {
			return false
		}
	}
	return true
}
