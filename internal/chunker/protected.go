package chunker

import (
	"cmp"
	"regexp"
	"slices"
	"strings"
)

// span is a half-open range of a text, in bytes unless a caller says runes.
type span struct {
	start, end int
}

// linkOrImage matches a Markdown link or image on one line. The text and
// destination are bounded and may not hold a line break (CommonMark
// forbids one across a blank line), so a stray "[" left by OCR cannot pair
// with a "](" paragraphs later and swallow everything in between.
var linkOrImage = regexp.MustCompile(`!?\[[^\]\n]{0,200}\]\([^)\n]{1,500}\)`)

// protectedSpans finds the parts of Markdown text no Chunk boundary may
// fall inside, sorted and non-overlapping:
//
//   - fenced code blocks (``` or ~~~, closed by a fence of the same
//     character at least as long), found first so nothing inside them
//     counts as anything else;
//   - tables: runs of lines that start and end with "|";
//   - math: from "$$" to the next "$$";
//   - inline code: a backtick run to the next run of the same length on
//     the same line;
//   - links and images.
//
// A fence, math block or inline code that is never closed protects
// nothing, so one stray marker cannot swallow the rest of a Document.
func protectedSpans(text string) []span {
	fences := fencedCodeSpans(text)
	var found []span
	found = append(found, fences...)
	from := 0
	for _, f := range append(fences, span{len(text), len(text)}) {
		found = append(found, spansOutsideCode(text, from, f.start)...)
		from = f.end
	}
	// Earliest first; of two starting together, the longer wins.
	slices.SortFunc(found, func(a, b span) int {
		return cmp.Or(cmp.Compare(a.start, b.start), cmp.Compare(b.end, a.end))
	})
	out := found[:0]
	end := 0
	for _, s := range found {
		if s.start >= end {
			out = append(out, s)
			end = s.end
		}
	}
	return out
}

// spansOutsideCode finds every protected span but fenced code in
// text[from:to].
func spansOutsideCode(text string, from, to int) []span {
	var out []span
	out = append(out, tableSpans(text, from, to)...)
	out = append(out, mathSpans(text, from, to)...)
	out = append(out, inlineCodeSpans(text, from, to)...)
	for _, loc := range linkOrImage.FindAllStringIndex(text[from:to], -1) {
		out = append(out, span{from + loc[0], from + loc[1]})
	}
	return out
}

// lines calls fn with each line of text[from:to]: its start, and its end
// before the line break.
func lines(text string, from, to int, fn func(start, end int)) {
	for start := from; start < to; {
		end := strings.IndexByte(text[start:to], '\n')
		if end < 0 {
			fn(start, to)
			return
		}
		fn(start, start+end)
		start += end + 1
	}
}

// fencedCodeSpans finds fenced code blocks, from the opening fence to the
// end of the closing one. Like CommonMark, a fence may be indented by up to
// three spaces, and a backtick fence's info string may not hold a backtick
// (such a line is inline code).
func fencedCodeSpans(text string) []span {
	var out []span
	var open struct {
		start  int
		marker byte
		length int
	}
	lines(text, 0, len(text), func(start, end int) {
		line := strings.TrimSuffix(text[start:end], "\r")
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if indent > 3 || indent == len(line) {
			return
		}
		marker := line[indent]
		if marker != '`' && marker != '~' {
			return
		}
		run := len(line[indent:]) - len(strings.TrimLeft(line[indent:], string(marker)))
		rest := line[indent+run:]
		switch {
		case run < 3:
		case open.length == 0:
			if marker == '`' && strings.Contains(rest, "`") {
				return
			}
			open.start, open.marker, open.length = start+indent, marker, run
		case marker == open.marker && run >= open.length && strings.TrimSpace(rest) == "":
			out = append(out, span{open.start, start + indent + run})
			open.length = 0
		}
	})
	return out
}

// tableSpans finds runs of table rows in text[from:to]: lines that, without
// surrounding whitespace, start and end with "|". The recursive tier keeps a
// table that fits ChunkSize whole and splits a larger one at its row
// boundaries, repeating its header (see tableUnits).
func tableSpans(text string, from, to int) []span {
	var out []span
	open := false
	lines(text, from, to, func(start, end int) {
		row := strings.TrimSpace(text[start:end])
		if len(row) < 2 || row[0] != '|' || row[len(row)-1] != '|' {
			open = false
			return
		}
		rowStart := start + strings.Index(text[start:end], "|")
		rowEnd := start + strings.LastIndex(text[start:end], "|") + 1
		if open {
			out[len(out)-1].end = rowEnd
		} else {
			out = append(out, span{rowStart, rowEnd})
			open = true
		}
	})
	return out
}

// mathSpans pairs each "$$" in text[from:to] with the next one.
func mathSpans(text string, from, to int) []span {
	const delim = "$$"
	var out []span
	for i := from; ; {
		open := strings.Index(text[i:to], delim)
		if open < 0 {
			return out
		}
		open += i
		body := open + len(delim)
		closing := strings.Index(text[body:to], delim)
		if closing < 0 {
			i = body
			continue
		}
		end := body + closing + len(delim)
		out = append(out, span{open, end})
		i = end
	}
}

// inlineCodeSpans finds inline code: a run of backticks, then text on the
// same line, then a run of exactly as many backticks.
func inlineCodeSpans(text string, from, to int) []span {
	var out []span
	for i := from; i < to; {
		open := strings.IndexByte(text[i:to], '`')
		if open < 0 {
			break
		}
		open += i
		n := len(text[open:to]) - len(strings.TrimLeft(text[open:to], "`"))
		body := open + n
		end := closingRun(text, body, to, n)
		if end < 0 {
			i = body
			continue
		}
		out = append(out, span{open, end})
		i = end
	}
	return out
}

// closingRun returns the end of the first run of exactly n backticks in
// text[from:to] before a line break, after at least one other byte, or -1.
func closingRun(text string, from, to, n int) int {
	for j := from; j < to && text[j] != '\n'; {
		if text[j] != '`' {
			j++
			continue
		}
		k := j
		for k < to && text[k] == '`' {
			k++
		}
		if k-j == n && j > from {
			return k
		}
		j = k
	}
	return -1
}

// toRuneSpans converts byte spans of text, sorted by start, to rune spans.
func toRuneSpans(text string, spans []span) []span {
	idx := runeIndex{text: text}
	out := make([]span, len(spans))
	for i, s := range spans {
		out[i] = span{idx.runeOffset(s.start), idx.runeOffset(s.end)}
	}
	return out
}

// insideSpan reports whether pos falls strictly inside one of the sorted
// spans, so cutting the text at pos would split it.
func insideSpan(spans []span, pos int) bool {
	i := firstEndingAfter(spans, pos)
	return i < len(spans) && spans[i].start < pos
}

// firstEndingAfter returns the index of the first of the sorted spans that
// ends after pos, or len(spans).
func firstEndingAfter(spans []span, pos int) int {
	i, _ := slices.BinarySearchFunc(spans, pos, func(s span, p int) int {
		if s.end <= p {
			return -1
		}
		return 1
	})
	return i
}
