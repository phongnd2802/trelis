package types

import (
	"time"

	"github.com/google/uuid"
)

// Conversation is a sequence of question and answer turns one Principal owns.
// A follow-up question carries its Conversation History into retrieval, so a
// reference such as "and what about 2024?" can be rewritten into a standalone
// query (GLOSSARY.md).
type Conversation struct {
	// ID identifies the Conversation on every route that reads it.
	ID uuid.UUID `json:"id"`
	// CreatedAt is when the Conversation was created.
	CreatedAt time.Time `json:"created_at"`
	// Turns are its turns in Seq order; it is never null in a read answer.
	Turns []ConversationTurn `json:"turns"`
}

// ConversationTurn is one question asked in a Conversation and the answer it
// got, kept so later questions can refer back to it. Its answer is derived
// data: a read shows it only while every source Document is still readable
// (ADR-0004).
type ConversationTurn struct {
	// Seq is the Conversation's own sequence number, starting at 1.
	Seq int64 `json:"seq"`
	// ConversationID is the Conversation the turn belongs to; it is not part
	// of the turn's JSON on the wire.
	ConversationID uuid.UUID `json:"-"`
	// Question is the raw question the caller asked.
	Question string `json:"question"`
	// RewrittenQuery is the standalone query a query rewrite produced and
	// retrieval searched with, or nil when no rewrite ran. It is the query
	// actually used, so a rewrite that fell back to the raw question stores
	// that.
	RewrittenQuery *string `json:"rewritten_query,omitempty"`
	// SourceDocumentIDs is the distinct Documents the answer depends on: every
	// retrieved result, plus, when a query rewrite ran, the source Documents
	// of the history answers the rewritten query was built from. It is empty
	// for a no-sources answer that did not rewrite. It is not part of the
	// turn's JSON on the wire; a read uses it to decide whether the answer may
	// still be shown.
	SourceDocumentIDs []uuid.UUID `json:"-"`
	// Withheld is true when any source Document is no longer readable to the
	// caller: Answer is empty, RewrittenQuery is nil, and only Seq, Question
	// and CreatedAt are shown.
	Withheld bool `json:"withheld,omitempty"`
	// Answer is the cleaned answer text, exactly as the answer returned it.
	Answer string `json:"answer"`
	// CreatedAt is when the turn was recorded.
	CreatedAt time.Time `json:"created_at"`
}
