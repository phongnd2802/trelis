package service

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/phongnd2802/trelis/internal/types"
)

// citationMarker matches a bracketed reference list such as [2], [1,3] or
// [1, 3]. Two markers written back to back are two matches.
var citationMarker = regexp.MustCompile(`\[(\d+(?:\s*,\s*\d+)*)\]`)

// parseCitations keeps only the source numbers in 1..sources that are real
// citation markers. A marker inside Markdown code (inline or fenced) or
// followed by "(" is part of the answer's text, not a reference to a source,
// so it is left untouched. It returns the answer text with every out-of-range
// number removed, whole markers and the whitespace before them included, and
// the cited source numbers once each, in order of first appearance.
func parseCitations(answer string, sources int) (string, []int) {
	locations := citationMarker.FindAllStringIndex(answer, -1)
	if len(locations) == 0 {
		return answer, nil
	}
	code := codeSpans(answer)
	cited := make([]int, 0)
	seen := make(map[int]bool)
	var b strings.Builder
	last := 0
	for _, loc := range locations {
		start, end := loc[0], loc[1]
		// Code and Markdown links are not citations. They stay in the text;
		// the next handled marker or the final write copies them unchanged.
		if inRanges(code, start) || (end < len(answer) && answer[end] == '(') {
			continue
		}
		valid := make([]int, 0, 2)
		for _, number := range strings.Split(answer[start+1:end-1], ",") {
			source, err := strconv.Atoi(strings.TrimSpace(number))
			if err != nil || source < 1 || source > sources {
				continue
			}
			valid = append(valid, source)
		}
		text := answer[last:start]
		if len(valid) == 0 {
			// Drop the space the marker was attached to, so "nguồn [7]."
			// does not become "nguồn .".
			b.WriteString(strings.TrimRight(text, " \t"))
			last = end
			continue
		}
		b.WriteString(text)
		b.WriteByte('[')
		for i, source := range valid {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(strconv.Itoa(source))
			if !seen[source] {
				seen[source] = true
				cited = append(cited, source)
			}
		}
		b.WriteByte(']')
		last = end
	}
	b.WriteString(answer[last:])
	return b.String(), cited
}

// citationList maps cited source numbers to the retrieved Chunks they stand
// for, in the order the markers first appeared.
func citationList(markers []int, results []types.RetrievedChunk) []types.Citation {
	citations := make([]types.Citation, 0, len(markers))
	for _, marker := range markers {
		result := results[marker-1]
		citations = append(citations, types.Citation{
			Marker:     marker,
			ChunkID:    result.ChunkID,
			DocumentID: result.DocumentID,
		})
	}
	return citations
}

// codeSpans returns the byte ranges of answer that are Markdown code: fenced
// blocks and inline code spans. A citation marker inside one is code, not a
// reference to a source.
func codeSpans(answer string) [][2]int {
	spans := fencedSpans(answer)
	for i := 0; i < len(answer); {
		if inRanges(spans, i) {
			i++
			continue
		}
		if answer[i] != '`' {
			i++
			continue
		}
		open := i
		for i < len(answer) && answer[i] == '`' {
			i++
		}
		run := i - open
		// An inline span closes with the next run of exactly the same length.
		closing := -1
		for j := i; j < len(answer); {
			if inRanges(spans, j) {
				j++
				continue
			}
			if answer[j] != '`' {
				j++
				continue
			}
			closingStart := j
			for j < len(answer) && answer[j] == '`' {
				j++
			}
			if j-closingStart == run {
				closing = j
				break
			}
		}
		if closing < 0 {
			// Unmatched backticks are literal text, not code.
			continue
		}
		spans = append(spans, [2]int{open, closing})
		i = closing
	}
	return spans
}

// fencedSpans returns the ranges of backtick-fenced blocks, fence lines
// included. An unclosed fence runs to the end of the answer, as it renders.
func fencedSpans(answer string) [][2]int {
	var spans [][2]int
	fenceStart, fenceLen := -1, 0
	for offset := 0; offset < len(answer); {
		line := answer[offset:]
		if end := strings.IndexByte(line, '\n'); end >= 0 {
			line = line[:end]
		}
		trimmed := strings.TrimLeft(line, " \t")
		run := 0
		for run < len(trimmed) && trimmed[run] == '`' {
			run++
		}
		switch {
		case fenceStart < 0 && run >= 3:
			fenceStart, fenceLen = offset, run
		case fenceStart >= 0 && run >= fenceLen && strings.TrimSpace(trimmed[run:]) == "":
			spans = append(spans, [2]int{fenceStart, offset + len(line)})
			fenceStart = -1
		}
		offset += len(line)
		if offset < len(answer) {
			offset++ // the newline
		}
	}
	if fenceStart >= 0 {
		spans = append(spans, [2]int{fenceStart, len(answer)})
	}
	return spans
}

// inRanges reports whether at falls inside one of ranges.
func inRanges(ranges [][2]int, at int) bool {
	for _, r := range ranges {
		if at >= r[0] && at < r[1] {
			return true
		}
	}
	return false
}
