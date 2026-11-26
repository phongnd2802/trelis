// Derived from WeKnora (https://github.com/Tencent/WeKnora), internal/models/api/special_tokens.go,
// without the multimodal parts Trelis does not send.
// Copyright (C) 2026 Tencent. Licensed under the MIT License; see NOTICE.

package api

import "strings"

// Message text is data, never chat-template syntax. Self-hosted tokenizers can
// recognize these literals even inside tool results or quoted documents. Break
// the literal with a zero-width space only in the outbound copy; actual roles
// continue to be serialized as structured provider fields.
var specialTokenLiterals = strings.NewReplacer(
	"<|", "<\u200b|",
	"<｜", "<\u200b｜",
	"<start_of_turn>", "<\u200bstart_of_turn>",
	"<end_of_turn>", "<\u200bend_of_turn>",
	"[INST]", "[\u200bINST]",
	"[/INST]", "[\u200b/INST]",
	"<<SYS>>", "<\u200b<SYS>>",
	"<</SYS>>", "<\u200b</SYS>>",
)

// NeutralizeMessageSpecialTokens returns a copy of the message with chat
// template delimiters broken so tokenizers treat them as data.
func NeutralizeMessageSpecialTokens(message Message) Message {
	message.Content = specialTokenLiterals.Replace(message.Content)
	message.ReasoningContent = specialTokenLiterals.Replace(message.ReasoningContent)
	if len(message.ToolCalls) != 0 {
		message.ToolCalls = append([]ToolCall(nil), message.ToolCalls...)
		for i := range message.ToolCalls {
			arguments := message.ToolCalls[i].Function.Arguments
			message.ToolCalls[i].Function.Arguments = specialTokenLiterals.Replace(arguments)
		}
	}
	return message
}
