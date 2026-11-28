-- name: CreateConversation :one
INSERT INTO conversations (principal_id)
VALUES ($1)
RETURNING *;

-- A Conversation is visible only to the Principal who owns it, so a foreign
-- id matches no row and is the repository's not-found error.
-- name: GetConversation :one
SELECT * FROM conversations
WHERE id = sqlc.arg(id) AND principal_id = sqlc.arg(principal_id);

-- Appends one turn to its Conversation and returns it with the next seq and
-- created_at Postgres assigned. The UPDATE advances the Conversation's
-- turn_seq under its row lock and only matches the owner, so zero rows means
-- the Conversation is gone or belongs to another Principal; the repository
-- maps that to ErrConversationNotFound. rewritten_query is NULL when no
-- rewrite ran; when one ran the repository passes the query actually used,
-- including a fallback equal to the raw question.
-- source_document_ids holds the distinct Documents the answer depends on:
-- every retrieved result, plus, when a rewrite ran, the sources of the
-- history answers it was rewritten from. It is empty for a no-sources answer
-- that did not rewrite.
-- name: InsertConversationTurn :one
WITH next AS (
    UPDATE conversations
    SET turn_seq = turn_seq + 1
    WHERE id = sqlc.arg(conversation_id) AND principal_id = sqlc.arg(principal_id)
    RETURNING turn_seq
)
INSERT INTO conversation_turns (conversation_id, seq, question, rewritten_query, source_document_ids, answer)
SELECT sqlc.arg(conversation_id), next.turn_seq, sqlc.arg(question),
       NULLIF(sqlc.arg(rewritten_query)::text, ''),
       sqlc.arg(source_document_ids)::uuid[], sqlc.arg(answer)
FROM next
RETURNING *;

-- The turns of one Conversation in seq order.
-- name: ListConversationTurns :many
SELECT * FROM conversation_turns
WHERE conversation_id = sqlc.arg(conversation_id)
ORDER BY seq;

-- The last turn_limit turns of one Conversation, in ascending seq order, so
-- the caller gets Conversation History oldest first.
-- name: ListRecentConversationTurns :many
SELECT * FROM (
    SELECT * FROM conversation_turns
    WHERE conversation_id = sqlc.arg(conversation_id)
    ORDER BY seq DESC
    LIMIT sqlc.arg(turn_limit)::bigint
) recent
ORDER BY seq;
