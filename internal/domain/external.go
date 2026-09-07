package domain

import (
	"context"
	"time"
)

// ExternalSource is one configured RSS/Atom feed or IMAP mailbox this
// service ingests from. Kind is an open string ("rss", "imap", ...): the
// set of supported adapters lives in Go, not in a schema-level enum.
type ExternalSource struct {
	ID          string
	Kind        string
	URI         string
	DisplayName *string
	// ActorID, Username, and Host are all nil for an imap-kind source
	// (Issue #77 PR4/ADR-0007's design-A host display is RSS-only) and
	// all set together for an rss-kind one, computed once when the
	// source is first registered (cmd/server's startup seeding) and
	// never recomputed afterward:
	//   - ActorID names this source's own dedicated ActorExternalSource
	//     row (the "1 external identity = 1 actor row" pattern Issue #52
	//     set for Open WebUI models).
	//   - Username is Misskey-username-charset-validated
	//     (internal/config's ownerUsernamePattern), owner-settable via
	//     RSS_FEED_URLS at registration or auto-derived from Host when
	//     left unset.
	//   - Host is this feed's own real origin (net/url.Parse(URI).Host)
	//     — a real, deployment-uncontrolled domain, unlike
	//     OPENWEBUI_PRESENTATION_HOST's single synthetic value. See
	//     ADR-0007 for why this is safe only because this deployment
	//     never federates, and the condition that would require
	//     revisiting it.
	ActorID  *string
	Username *string
	Host     *string
	// Cursor is an adapter-opaque resume token (for internal/ingest/rss,
	// a JSON string carrying the last response's ETag/Last-Modified). It
	// only ever advances after every item in a fetch batch has been
	// durably processed (see RecordFetchSuccess), so a crash mid-batch
	// re-fetches the same batch rather than skipping unprocessed items.
	Cursor              *string
	LastFetchedAt       *time.Time
	LastError           *string
	ConsecutiveFailures int
	// Active gates ingest.Scheduler's polling: List only ever returns
	// active sources. False means ReconcileFromConfig's most recent
	// round did not see this source's URI among its configured feed
	// URLs, most commonly because it was removed from RSS_FEED_URLS.
	// The row itself, and every ExternalItem/Entry it already produced,
	// is never deleted — removing a feed only stops future polling of
	// it, the same "never rewrite history" convention
	// openwebui_models.Active already follows.
	Active    bool
	CreatedAt time.Time
}

// ExternalItem is one fetched item from an ExternalSource, deduplicated
// before being promoted into the timeline as an Entry. EntryID is nil
// until promoted.
type ExternalItem struct {
	ID            string
	SourceID      string
	ExternalID    string // the source's own item ID (guid, Message-ID, ...)
	ProvenanceURL *string
	PublishedAt   *time.Time
	FetchedAt     time.Time
	// DedupeKey is a content-hash fallback for sources whose ExternalID
	// is unstable or absent.
	DedupeKey string
	EntryID   *string
	CreatedAt time.Time
}

// ExternalSourceRepository persists configured external sources.
type ExternalSourceRepository interface {
	Create(ctx context.Context, s ExternalSource) error
	Get(ctx context.Context, id string) (ExternalSource, error)
	// GetByURI returns the source matching (kind, uri) exactly —
	// UNIQUE(kind, uri) guarantees at most one — or ErrNotFound. Issue
	// #77 PR4's RSS registration (cmd/server) uses this to decide
	// whether a configured feed URL is genuinely new before creating its
	// paired ActorExternalSource row, rather than the older
	// create-and-ignore-conflict shape EnsureFromConfig still uses for
	// imap (which needs no paired actor to avoid orphaning on conflict).
	GetByURI(ctx context.Context, kind, uri string) (ExternalSource, error)
	// GetByActorID returns the source whose ActorID is actorID, or
	// ErrNotFound if none matches (every imap-kind source, and any
	// actorID that does not belong to this repository at all). Backs
	// internal/httpserver's ExternalSourceResolver
	// (Issue #77 PR4's resolveUserLite case for ActorExternalSource).
	GetByActorID(ctx context.Context, actorID string) (ExternalSource, error)
	// List returns every *active* configured source of kind, in creation
	// order (see ExternalSource.Active's own doc comment). A caller
	// (ingest.Scheduler) always scopes to its own kind: without this
	// filter, two Scheduler instances configured with different per-kind
	// poll intervals (RSS's default 15 minutes vs. an IMAP mailbox's own
	// interval) would each enqueue a job for every source regardless of
	// kind, double-enqueueing "external_source_poll" jobs for the same
	// source on every tick where their intervals overlap.
	List(ctx context.Context, kind string) ([]ExternalSource, error)
	// RecordFetchSuccess updates last_fetched_at, clears last_error, and
	// resets consecutive_failures to 0. When cursor is non-nil it also
	// advances the stored fetch cursor; a nil cursor leaves the existing
	// one unchanged (an unmodified-since-last-fetch outcome has no new
	// cursor to record but is still a success). Callers must not call
	// this until every item in the corresponding fetch batch has been
	// durably processed, so cursor never advances past unprocessed items.
	RecordFetchSuccess(ctx context.Context, id string, cursor *string, at time.Time) error
	// RecordFetchFailure records a fetch attempt's failure (last_error,
	// incremented consecutive_failures) for operator observability,
	// without touching cursor, so a failed batch is retried from the
	// same position. It is independent of internal/jobs' own
	// retry/dead-job bookkeeping, which this never influences.
	RecordFetchFailure(ctx context.Context, id string, errMsg string, at time.Time) error
	// ReconcileFromConfig reconciles kind's configured set of source URIs
	// (RSS_FEED_URLS's entries, or IMAP's single mailbox URI) against
	// existing external_sources rows of that kind (Issue #76 PR4a,
	// replacing the old create-only EnsureFromConfig): a URI not yet
	// known is created (active); one that exists but is currently
	// inactive is reactivated; an existing active source whose URI is no
	// longer in uris is deactivated. It never deletes a row or touches
	// display_name/cursor/last_fetched_at/last_error/
	// consecutive_failures — the same "reconcile presence, never rewrite
	// history" contract Registry.SyncCatalog already uses for
	// openwebui_models. Called once at startup and, for RSS/IMAP's
	// db-eligible poll interval, again on every ingest.Scheduler tick, so
	// a feed URL added or removed from config takes effect without a
	// restart.
	ReconcileFromConfig(ctx context.Context, kind string, uris []string, at time.Time) error
}

// ExternalItemRepository persists fetched external items and their
// promotion into the timeline.
type ExternalItemRepository interface {
	// Create inserts a new item. It returns ErrConflict if the
	// (source, external ID) pair or the dedupe key already exists.
	Create(ctx context.Context, i ExternalItem) error
	GetByDedupeKey(ctx context.Context, dedupeKey string) (ExternalItem, error)
	// Promote links item to its produced timeline Entry. Call together
	// with EntryRepository.Create for entryID inside the same UnitOfWork
	// transaction.
	Promote(ctx context.Context, id, entryID string) error
}
