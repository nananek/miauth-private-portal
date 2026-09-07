-- Issues #81 (citation resolution) + #84 (chat title / viewer link).
--
-- remote_chat_title is the provider's own most recently observed chat
-- title (GET /api/v1/chats/{id}'s top-level "title"), captured only once
-- it has moved past the "bridge-precreated" placeholder this adapter set
-- at chat creation — see internal/provider/openwebui/client.go's
-- precreatedChatTitle. It is written only while OPENWEBUI_VIEWER_BASE_URL
-- is configured (ADR-0005 D23): an unconfigured deployment never asks
-- for title generation and never persists a title, reproducing pre-#84
-- behavior exactly.
--
-- sources_json is Issue #81's normalized citation list (ADR-0005 D22) —
-- a JSON array of {kind, display_name, url, arguments} records, never
-- the raw, untrusted, potentially large document/page text a tool call
-- or web search returned. NULL when the turn's completions response
-- carried no sources[] at all (the ordinary case: no tool ran).
--
-- Both columns are owner-facing presentation metadata only: nothing here
-- is consulted for identity, ordering, or authorization (ADR-0005 D2),
-- the same rule every other Remote*/title-shaped column in this table
-- already follows.
ALTER TABLE openwebui_turn_links ADD COLUMN remote_chat_title TEXT;
ALTER TABLE openwebui_turn_links ADD COLUMN sources_json TEXT;

-- Backs OpenWebUITurnLinkRepository.GetByAssistantEntry (Issues #81/#84):
-- the wire-projection enrichment path (internal/httpserver's
-- projectNote) looks up a generated reply's turn by the assistant entry
-- it authored, once per note projected, so this needs an index rather
-- than a table scan. Partial and unique: at most one non-tombstoned
-- turn ever owns a given assistant entry (SetAssistantEntry is called
-- exactly once per turn, on the one entry timeline.CreateGeneratedReplyBy
-- just created for it), and the column is NULL for every turn that has
-- not completed yet, which a plain UNIQUE index would otherwise also
-- have to accept many-of.
CREATE UNIQUE INDEX idx_openwebui_turn_links_assistant_entry
    ON openwebui_turn_links (assistant_entry_id)
    WHERE assistant_entry_id IS NOT NULL;
