package service

import (
	"fmt"
	"strings"

	"github.com/phongnd2802/trelis/internal/types"
)

// rewriteAnswerMaxRunes is how much of each Conversation History answer a
// rewrite prompt carries. An answer is the longest part of the history, so
// cutting it keeps the prompt small without hiding the question's subject.
const rewriteAnswerMaxRunes = 1000

// rewriteSystemPrompt is the system message of a query rewrite: one
// standalone search query, in the question's language, with earlier turns
// resolved.
const rewriteSystemPrompt = `Rewrite the latest question as one standalone search query that makes sense without the conversation. ` +
	`Resolve pronouns and references to earlier turns, for example "and what about 2024?" becomes a query naming what the conversation was about and 2024. ` +
	`Write the query in the language of the latest question. ` +
	`Output only the query, with no explanation, labels or quotation marks.`

// rewriteTurns returns the chat turns for one query rewrite: the system
// prompt and a user message carrying the Conversation History oldest first as
// Q/A pairs, then the latest question. Each history answer is compacted to
// rewriteAnswerMaxRunes runes with an ellipsis when it was cut.
func rewriteTurns(history []types.ConversationTurn, query string) []types.ChatTurn {
	var b strings.Builder
	for _, turn := range history {
		fmt.Fprintf(&b, "Q: %s\nA: %s\n\n", turn.Question, compactAnswer(turn.Answer))
	}
	b.WriteString("Latest question: ")
	b.WriteString(query)
	return []types.ChatTurn{
		{Role: types.ChatRoleSystem, Content: rewriteSystemPrompt},
		{Role: types.ChatRoleUser, Content: b.String()},
	}
}

// compactAnswer cuts answer to rewriteAnswerMaxRunes runes, appending an
// ellipsis when it was cut.
func compactAnswer(answer string) string {
	runes := []rune(answer)
	if len(runes) <= rewriteAnswerMaxRunes {
		return answer
	}
	return string(runes[:rewriteAnswerMaxRunes]) + "…"
}
