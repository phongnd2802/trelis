package chunker

import (
	"strings"
)

// tableRows returns the rows of table span s, each trimmed to the pipes that
// bound it. s comes from tableSpans, so every line inside it is a row.
func tableRows(text string, s span) []span {
	var rows []span
	lines(text, s.start, s.end, func(start, end int) {
		line := text[start:end]
		rows = append(rows, span{
			start: start + strings.Index(line, "|"),
			end:   start + strings.LastIndex(line, "|") + 1,
		})
	})
	return rows
}

// tableHeader returns the header rows of a table: its first row plus the
// delimiter row below it, joined by "\n". A table whose second row is not a
// delimiter row has no header and returns "".
func tableHeader(text string, rows []span) string {
	if len(rows) < 2 || !delimiterRow(text[rows[1].start:rows[1].end]) {
		return ""
	}
	return text[rows[0].start:rows[0].end] + "\n" + text[rows[1].start:rows[1].end]
}

// delimiterRow reports whether row, including its bounding pipes, is a
// table's delimiter row: cells of dashes, each optionally aligned with a
// leading or trailing ":" and with spaces around it.
func delimiterRow(row string) bool {
	row = strings.TrimSpace(row)
	if len(row) < 2 || row[0] != '|' || row[len(row)-1] != '|' {
		return false
	}
	for _, cell := range strings.Split(row[1:len(row)-1], "|") {
		cell = strings.TrimSpace(cell)
		if cell == "" {
			return false
		}
		dashes := 0
		for i := 0; i < len(cell); i++ {
			switch cell[i] {
			case '-':
				dashes++
			case ':':
				if i != 0 && i != len(cell)-1 {
					return false
				}
			default:
				return false
			}
		}
		if dashes == 0 {
			return false
		}
	}
	return true
}

// isTableSpan reports whether s is one of protectedSpans' table spans: it
// starts at a row's opening pipe and ends just past the last row's closing
// one. No other protected span starts and ends with a pipe.
func isTableSpan(text string, s span) bool {
	return s.end-s.start >= 2 && text[s.start] == '|' && text[s.end-1] == '|'
}

// tableLayout is a table in rune offsets for ValidateChunks: the budget its
// header takes, where its body starts, and its rows.
type tableLayout struct {
	headerCost int
	bodyStart  int
	rows       []span
}

// tableLayoutIn returns the layout of a table span from its byte offsets
// (byteSpan) and rune offsets (runeSpan).
func tableLayoutIn(text string, byteSpan, runeSpan span) tableLayout {
	rows := tableRows(text, byteSpan)
	layout := tableLayout{
		headerCost: contextCost(tableHeader(text, rows)),
		bodyStart:  runeSpan.end,
		rows:       toRuneSpans(text, rows),
	}
	if len(layout.rows) > 2 {
		layout.bodyStart = layout.rows[2].start
	}
	return layout
}

// tableCutBreaksSpan reports whether a Chunk starting or ending at rune pos
// inside table span s breaks the table: a cut is allowed at a row start, at
// the end of a row that had to be cut, or inside a row longer than the piece
// budget. budget is the room the Chunk has for Content after its
// ContextHeader; when the Chunk starts before the table body, its Content
// holds the header rows, which the continuation pieces do not repeat, so
// their cost comes off the row budget too.
func tableCutBreaksSpan(s span, layout tableLayout, pos, chunkStart, budget int) bool {
	if pos <= s.start || pos >= s.end {
		return false
	}
	if layout.headerCost > 0 && chunkStart < layout.bodyStart {
		budget -= layout.headerCost
	}
	for _, row := range layout.rows {
		if pos == row.start {
			return false
		}
		if row.start < pos && pos <= row.end {
			return row.end-row.start <= budget
		}
	}
	return true
}
