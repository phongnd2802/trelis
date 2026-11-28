package service

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/phongnd2802/trelis/internal/types"
)

// decompositionMaxSubQuestions is how many sub-questions a decomposition
// keeps. The prompt asks for 2 to 4; the cap also guards against a reply
// that lists more.
const decompositionMaxSubQuestions = 4

// decomposeSystemPrompt is the system message of a query decomposition: 2 to
// 4 self-contained sub-questions that together cover the question, each
// answerable from one source, in the question's language, as a JSON array.
const decomposeSystemPrompt = `Split the question into 2 to 4 self-contained sub-questions that together cover what it needs. ` +
	`Each sub-question must be answerable from one source on its own. ` +
	`Write the sub-questions in the language of the question. ` +
	`Output only a JSON array of strings, with no explanation, labels or code fence.`

// decomposeTurns returns the chat turns for one query decomposition: the
// system prompt and a user message carrying the question.
func decomposeTurns(query string) []types.ChatTurn {
	return []types.ChatTurn{
		{Role: types.ChatRoleSystem, Content: decomposeSystemPrompt},
		{Role: types.ChatRoleUser, Content: query},
	}
}

// parseSubQuestions turns the chat model's reply into the usable
// sub-questions: a JSON array of strings, or, when the reply is not that, its
// non-empty lines with list bullets and numbering removed. Each entry is
// trimmed; a blank one, one longer than RetrievalQueryMaxRunes and one that
// repeats the original or an earlier sub-question, case-insensitively, is
// dropped, and at most decompositionMaxSubQuestions remain.
func parseSubQuestions(reply, query string) []string {
	text := stripCodeFence(strings.TrimSpace(reply))
	candidates, ok := parseJSONSubQuestions(text)
	if !ok {
		candidates = lineSubQuestions(text)
	}
	seen := make(map[string]struct{}, len(candidates)+1)
	seen[strings.ToLower(strings.TrimSpace(query))] = struct{}{}
	subQuestions := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		subQuestion := strings.TrimSpace(candidate)
		if subQuestion == "" || utf8.RuneCountInString(subQuestion) > types.RetrievalQueryMaxRunes {
			continue
		}
		key := strings.ToLower(subQuestion)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		subQuestions = append(subQuestions, subQuestion)
		if len(subQuestions) == decompositionMaxSubQuestions {
			break
		}
	}
	return subQuestions
}

// parseJSONSubQuestions reads text as a JSON array of strings. It reports
// false for anything else, including null, so a reply that is not an array
// falls back to one sub-question per line.
func parseJSONSubQuestions(text string) ([]string, bool) {
	var subQuestions []string
	if err := json.Unmarshal([]byte(text), &subQuestions); err != nil || subQuestions == nil {
		return nil, false
	}
	return subQuestions, true
}

// stripCodeFence removes one Markdown code fence around text: the opening
// line starting with ``` and, when present, the closing ```.
func stripCodeFence(text string) string {
	if !strings.HasPrefix(text, "```") {
		return text
	}
	firstNewline := strings.IndexByte(text, '\n')
	if firstNewline < 0 {
		return text
	}
	body := text[firstNewline+1:]
	if end := strings.LastIndex(body, "```"); end >= 0 {
		body = body[:end]
	}
	return strings.TrimSpace(body)
}

// lineSubQuestions reads text as one sub-question per line, dropping list
// bullets and numbering such as "- " or "1. ".
func lineSubQuestions(text string) []string {
	lines := strings.Split(text, "\n")
	subQuestions := make([]string, 0, len(lines))
	for _, line := range lines {
		if subQuestion := trimListMarker(line); subQuestion != "" {
			subQuestions = append(subQuestions, subQuestion)
		}
	}
	return subQuestions
}

// trimListMarker removes a leading list bullet or numbering from line and
// trims it.
func trimListMarker(line string) string {
	line = strings.TrimSpace(line)
	for _, bullet := range []string{"- ", "* ", "+ ", "• "} {
		if after, ok := strings.CutPrefix(line, bullet); ok {
			return strings.TrimSpace(after)
		}
	}
	digits := 0
	for digits < len(line) && line[digits] >= '0' && line[digits] <= '9' {
		digits++
	}
	if digits > 0 && digits+1 < len(line) && line[digits+1] == ' ' {
		switch line[digits] {
		case '.', ')', ':':
			return strings.TrimSpace(line[digits+1:])
		}
	}
	return line
}
