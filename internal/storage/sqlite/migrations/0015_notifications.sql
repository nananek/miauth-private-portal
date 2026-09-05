-- Issue #23 PR6: POST /api/i/notifications real delivery. A row is
-- inserted here, in the same transaction as the entry it relates to, by
-- timeline.Service.CreateGeneratedReply (type "reply", for a newly
-- created llm_reply/llm_follow_up entry) and CreateExternalEntry (type
-- "app", only when a news/mail item is genuinely new, never on a
-- redelivered duplicate). There is no read/unread column: PR2's trace
-- found no Aria/misskey_dart wire path for mark-all-as-read or any other
-- read-management call, so this service implements listing only.
CREATE TABLE notifications (
    id TEXT PRIMARY KEY,
    type TEXT NOT NULL,
    related_entry_id TEXT NOT NULL REFERENCES entries (id),
    created_at TEXT NOT NULL
);

-- Backs ListDesc's newest-first paging.
CREATE INDEX idx_notifications_created_at ON notifications (created_at, id);
