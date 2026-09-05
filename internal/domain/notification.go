package domain

import (
	"context"
	"time"
)

// NotificationType is the wire-visible Misskey notification `type` value
// this deployment emits (Issue #23 PR6). Both values are pre-existing
// entries in the pinned `misskey_dart` `NotificationType` enum
// (docs/compat/aria-v1.5.11.md's "POST /api/i/notifications" section), so
// neither relies on the parser's unknown-value-to-null fallback.
type NotificationType string

const (
	// NotificationReply marks a notification generated when
	// timeline.Service.CreateGeneratedReply creates an llm_reply/
	// llm_follow_up entry: structurally the same "someone replied to your
	// note" shape real Misskey's own "reply" notification type covers.
	NotificationReply NotificationType = "reply"
	// NotificationApp marks a notification generated when
	// timeline.Service.CreateExternalEntry ingests a genuinely new (not a
	// redelivered duplicate) news/mail item. Real Misskey has no
	// federation-free "a followed source posted" notification type, and
	// "app" (free-text third-party notification) is the closest
	// structural match.
	NotificationApp NotificationType = "app"
)

// Notification records one delivered notification event (Issue #23 PR6).
// RelatedEntryID always names the entry the notification is about: the
// newly created llm_reply/llm_follow_up entry itself for
// NotificationReply, or the newly ingested news/mail entry for
// NotificationApp. There is no ReadAt/read-tracking field: PR2's trace
// confirmed Aria/misskey_dart has no wire path for mark-all-as-read or
// any other read-management call, so this service implements listing
// only (see docs/compat/aria-v1.5.11.md's "POST /api/notifications/
// mark-all-as-read" section).
type Notification struct {
	ID             string
	Type           NotificationType
	RelatedEntryID string
	CreatedAt      time.Time
}

// NotificationRepository persists and lists delivered notifications.
type NotificationRepository interface {
	// Create inserts n. Callers (timeline.Service.CreateGeneratedReply/
	// CreateExternalEntry) always call this in the same transaction as
	// the entry it relates to, so a notification never outlives, or is
	// missing for, its related entry.
	Create(ctx context.Context, n Notification) error
	// Get returns one notification by its opaque ID. It exists only to
	// resolve ListDesc's untilId pagination anchor, the same role
	// ReactionRepository.Get plays for notes/reactions' untilId. It
	// returns ErrNotFound if id does not exist.
	Get(ctx context.Context, id string) (Notification, error)
	// ListDesc returns notifications newest-first by (created_at, id).
	// When before is nil it returns the most recent page; otherwise
	// notifications strictly older than before in that same order — the
	// same paging contract as EntryRepository.ListTimelineDesc.
	ListDesc(ctx context.Context, before *Cursor, limit int) ([]Notification, error)
}
