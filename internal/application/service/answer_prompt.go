package service

import (
	"fmt"
	"strings"

	"github.com/phongnd2802/trelis/internal/types"
)

// answerSystemPrompt is the system message of every Pipeline Answer: it
// constrains the chat role to the numbered sources, asks for a marker per
// claim, and keeps the answer in the question's language.
const answerSystemPrompt = `Answer using only the numbered sources in the user message. ` +
	`Cite each claim with the source numbers in square brackets, for example [2] or [1][3]. ` +
	`If the sources do not answer the question, say that you could not find the answer in them. ` +
	`Answer in the language of the question.`

// answerTurns returns the chat turns for one answer: the system prompt and a
// user message listing the sources in retrieval order as [1]..[n], each with
// its text, then the question.
func answerTurns(query string, results []types.RetrievedChunk) []types.ChatTurn {
	var b strings.Builder
	b.WriteString("Sources:\n")
	for i, result := range results {
		fmt.Fprintf(&b, "[%d] %s\n\n", i+1, retrievedChunkText(result))
	}
	b.WriteString("Question: ")
	b.WriteString(query)
	return []types.ChatTurn{
		{Role: types.ChatRoleSystem, Content: answerSystemPrompt},
		{Role: types.ChatRoleUser, Content: b.String()},
	}
}

// retrievedChunkText is the text a source is shown with: chunkText's context
// header above content, the same text ingest embedded.
func retrievedChunkText(c types.RetrievedChunk) string {
	return chunkText(&types.Chunk{Content: c.Content, ContextHeader: c.ContextHeader})
}
