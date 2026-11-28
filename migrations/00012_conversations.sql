-- Conversations (#44): the questions one Principal asks and the answers they
-- got, so a follow-up can be rewritten into a standalone query and every turn
-- belongs to one Principal. A Conversation and its turns live in Postgres
-- beside the answers they describe; deleting a Principal takes them with it.
-- +goose Up
CREATE TABLE conversations (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    principal_id uuid        NOT NULL REFERENCES principals (id) ON DELETE CASCADE,
    -- The last turn seq allocated for the Conversation. The insert in
    -- internal/db/queries/conversations.sql advances it under the row lock,
    -- so two turns of one Conversation cannot take the same seq.
    turn_seq     bigint      NOT NULL DEFAULT 0,
    created_at   timestamptz NOT NULL DEFAULT now()
);
COMMENT ON TABLE conversations IS 'Question and answer conversations, one owner each';
COMMENT ON COLUMN conversations.turn_seq IS
    'Last Conversation turn seq allocated; advanced under the row lock so turns take distinct seqs';

CREATE TABLE conversation_turns (
    conversation_id uuid        NOT NULL REFERENCES conversations (id) ON DELETE CASCADE,
    -- Per-Conversation sequence number, starting at 1.
    seq             bigint      NOT NULL,
    -- The raw question the caller asked.
    question        text        NOT NULL,
    -- The standalone query a query rewrite produced; NULL when no rewrite
    -- ran. A rewrite that fell back to the raw question still stores it.
    rewritten_query text        NULL,
    -- The distinct Documents the answer depends on: every retrieved result,
    -- plus, when a query rewrite ran, the sources of the history answers it
    -- was rewritten from. A no-sources answer that did not rewrite stores an
    -- empty array.
    source_document_ids uuid[]  NOT NULL DEFAULT '{}',
    -- The cleaned answer text, exactly as the answer returned it.
    answer          text        NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    -- The primary-key index also serves (conversation_id, seq) filters and
    -- order.
    PRIMARY KEY (conversation_id, seq)
);
COMMENT ON TABLE conversation_turns IS 'Turns of each Conversation, in seq order';
COMMENT ON COLUMN conversation_turns.seq IS
    'Per-Conversation sequence number, starting at 1';
COMMENT ON COLUMN conversation_turns.rewritten_query IS
    'Standalone query a query rewrite produced; NULL when no rewrite ran';
COMMENT ON COLUMN conversation_turns.source_document_ids IS
    'Distinct Documents the answer depends on: every retrieved result plus the history sources a rewrite used; empty when no rewrite ran and nothing was retrieved';

-- +goose Down
DROP TABLE conversation_turns;
DROP TABLE conversations;
