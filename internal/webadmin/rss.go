package webadmin

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/nananek/miauth-private-portal/internal/config"
	"github.com/nananek/miauth-private-portal/internal/domain"
)

// RSSFeed is one RSS_FEED_URLS entry, parsed for display/editing.
type RSSFeed struct {
	URL string
	// Username is Issue #77 PR4/ADR-0008's optional per-feed posting
	// username ("|username" suffix), nil when an entry sets none.
	Username *string
}

// RSSFeedsSnapshot is ListRSSFeeds' result: the current effective feed
// list, plus whether it is currently backed by an app_config override
// (Decision 5) — an operator needs to know this before Decision 4's
// "cannot remove the last bootstrap-sourced feed" restriction surprises
// them.
type RSSFeedsSnapshot struct {
	Feeds        []RSSFeed
	FromOverride bool
}

var (
	// ErrRSSFeedAlreadyExists is AddRSSFeed's error for a URL already
	// present in the current list (exact string match — see
	// plan-136-phase4 §1 Decision 3 on why no normalization).
	ErrRSSFeedAlreadyExists = errors.New("webadmin: RSS feed URL is already configured")
	// ErrRSSFeedNotFound is RemoveRSSFeed's error for a URL not present
	// in the current list.
	ErrRSSFeedNotFound = errors.New("webadmin: RSS feed URL is not currently configured")
	// ErrRSSFeedCannotRemoveLastBootstrapFeed is RemoveRSSFeed's error
	// when removing the target would leave zero feeds AND no
	// app_config override currently exists (Decision 4) — the
	// bootstrap value itself cannot be made empty through this path.
	ErrRSSFeedCannotRemoveLastBootstrapFeed = errors.New(
		"webadmin: cannot remove the last RSS feed while RSS_FEED_URLS has no database override yet; " +
			"edit RSS_FEED_URLS in the config file/environment and restart, or add a replacement feed first")
	// ErrRSSFeedInvalid wraps a config.ValidateKeyValue failure — never
	// exposes the concrete *config.ValidationError type outside this
	// package (internal/httpserver must not import internal/config; see
	// plan-136-phase4 §0's "What changed" note), only this sentinel
	// plus the wrapped error's message text via Error().
	ErrRSSFeedInvalid = errors.New("webadmin: invalid RSS feed URL or username")
)

// ListRSSFeeds returns the current effective RSS_FEED_URLS entries: an
// app_config override's entries if one exists, else s's bootstrap
// value (Config.RSSFeedURLsBootstrap/RSSFeedUsernamesBootstrap).
func (s *Service) ListRSSFeeds(ctx context.Context) (RSSFeedsSnapshot, error) {
	urls, usernames, fromOverride, err := s.currentRSSFeedURLs(ctx)
	if err != nil {
		return RSSFeedsSnapshot{}, err
	}
	feeds := make([]RSSFeed, len(urls))
	for i, u := range urls {
		feeds[i] = RSSFeed{URL: u, Username: usernames[i]}
	}
	return RSSFeedsSnapshot{Feeds: feeds, FromOverride: fromOverride}, nil
}

// currentRSSFeedURLs returns the raw RSS_FEED_URLS entries currently in
// effect, and whether they came from an app_config override. The
// bootstrap fallback returns a clone of s.rssFeedURLsBootstrap/
// rssFeedUsernamesBootstrap, never those fields' own backing arrays:
// AddRSSFeed/RemoveRSSFeed mutate their (urls, usernames) return value
// in place (append/slices.Delete), and slices.Delete in particular
// shifts and zeroes elements of its backing array — handing out the
// Service's own stored bootstrap slice directly would let a single
// mutation permanently corrupt it for the process's remaining lifetime.
func (s *Service) currentRSSFeedURLs(ctx context.Context) (urls []string, usernames []*string, fromOverride bool, err error) {
	entry, err := s.repos.Config.Get(ctx, config.KeyRSSFeedURLs)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return slices.Clone(s.rssFeedURLsBootstrap), slices.Clone(s.rssFeedUsernamesBootstrap), false, nil
		}
		return nil, nil, false, fmt.Errorf("webadmin: read RSS_FEED_URLS override: %w", err)
	}
	urls, usernames = config.SplitRSSFeedURLs(entry.Value)
	return urls, usernames, true, nil
}

// AddRSSFeed appends url (with optional username, Issue #77 PR4/ADR-0008
// per-feed username) to RSS_FEED_URLS, writing the resulting value
// through the same Config.Set + ConfigAudit.Record transaction
// cmd/miauthctl/config.go's configSet already uses (plan-136-phase4 §1
// Decision 1: no new domain logic). ownerActorID is
// AppConfigAuditEntry.ChangedBy/AppConfigEntry.UpdatedBy — the Owner
// actor bound to the Web UI session that called this (ADR-0010 Decision
// 6), the same actor a CLI operator's config set would also attribute
// to (there is exactly one Owner either way).
func (s *Service) AddRSSFeed(ctx context.Context, ownerActorID, url string, username *string) error {
	urls, usernames, _, err := s.currentRSSFeedURLs(ctx)
	if err != nil {
		return err
	}
	if slices.Contains(urls, url) {
		return ErrRSSFeedAlreadyExists
	}
	urls = append(urls, url)
	usernames = append(usernames, username)
	newValue := config.JoinRSSFeedURLs(urls, usernames)
	if err := config.ValidateKeyValue(config.KeyRSSFeedURLs, newValue); err != nil {
		return fmt.Errorf("%w: %w", ErrRSSFeedInvalid, err)
	}
	return s.setRSSFeedURLs(ctx, ownerActorID, newValue)
}

// RemoveRSSFeed removes url from RSS_FEED_URLS and returns that entry's
// own username (nil if it had none), so the HTTP handler can attribute
// the web_admin_action_audit row's BeforeValue without a second lookup
// (Phase 3 established that audit writes happen in the HTTP handler,
// not the service method — this phase keeps that same boundary, so
// RemoveRSSFeed hands back everything the caller needs rather than
// writing the audit row itself).
//
// If url was the last entry of an active app_config override, this
// unsets the override (reverting to the bootstrap value, mirroring
// cmd/miauthctl/config.go's configUnset exactly). If it was the last
// entry of the *bootstrap* value (no override exists), this returns
// ErrRSSFeedCannotRemoveLastBootstrapFeed instead of silently reverting
// to a non-empty bootstrap list (plan-136-phase4 §1 Decision 4).
func (s *Service) RemoveRSSFeed(ctx context.Context, ownerActorID, url string) (removedUsername *string, err error) {
	urls, usernames, fromOverride, err := s.currentRSSFeedURLs(ctx)
	if err != nil {
		return nil, err
	}
	idx := slices.Index(urls, url)
	if idx < 0 {
		return nil, ErrRSSFeedNotFound
	}
	removedUsername = usernames[idx]
	urls = slices.Delete(urls, idx, idx+1)
	usernames = slices.Delete(usernames, idx, idx+1)

	if len(urls) == 0 {
		if !fromOverride {
			return nil, ErrRSSFeedCannotRemoveLastBootstrapFeed
		}
		if err := s.unsetRSSFeedURLs(ctx, ownerActorID); err != nil {
			return nil, err
		}
		return removedUsername, nil
	}
	newValue := config.JoinRSSFeedURLs(urls, usernames)
	// No ValidateKeyValue call needed: removing entries from an
	// already-valid list cannot produce an invalid one.
	if err := s.setRSSFeedURLs(ctx, ownerActorID, newValue); err != nil {
		return nil, err
	}
	return removedUsername, nil
}

// setRSSFeedURLs writes newValue as RSS_FEED_URLS's app_config override,
// compare-and-set on the row's current version (0/no row for a
// first-ever override), in the same transaction as its
// app_config_audit entry — identical shape to
// cmd/miauthctl/config.go's configSet.
func (s *Service) setRSSFeedURLs(ctx context.Context, ownerActorID, newValue string) error {
	expectedVersion := 0
	var oldValue *string
	if existing, err := s.repos.Config.Get(ctx, config.KeyRSSFeedURLs); err == nil {
		v := existing.Value
		oldValue = &v
		expectedVersion = existing.Version
	} else if !errors.Is(err, domain.ErrNotFound) {
		return fmt.Errorf("webadmin: read RSS_FEED_URLS before set: %w", err)
	}
	now := s.clock.Now()
	return s.uow.WithinTx(ctx, func(ctx context.Context, repos domain.Repos) error {
		if err := repos.Config.Set(ctx, config.KeyRSSFeedURLs, newValue, expectedVersion, ownerActorID, now); err != nil {
			return err
		}
		return repos.ConfigAudit.Record(ctx, domain.AppConfigAuditEntry{
			ID: domain.NewID(), Key: config.KeyRSSFeedURLs, OldValue: oldValue, NewValue: &newValue,
			Version: expectedVersion + 1, ChangedAt: now, ChangedBy: ownerActorID,
		})
	})
}

// unsetRSSFeedURLs removes RSS_FEED_URLS's app_config override
// entirely, in the same transaction as its app_config_audit entry —
// identical shape to cmd/miauthctl/config.go's configUnset. Only called
// when an override is known to currently exist (RemoveRSSFeed's
// fromOverride check).
func (s *Service) unsetRSSFeedURLs(ctx context.Context, ownerActorID string) error {
	existing, err := s.repos.Config.Get(ctx, config.KeyRSSFeedURLs)
	if err != nil {
		return fmt.Errorf("webadmin: read RSS_FEED_URLS before unset: %w", err)
	}
	oldValue := existing.Value
	now := s.clock.Now()
	return s.uow.WithinTx(ctx, func(ctx context.Context, repos domain.Repos) error {
		if err := repos.Config.Unset(ctx, config.KeyRSSFeedURLs); err != nil {
			return err
		}
		return repos.ConfigAudit.Record(ctx, domain.AppConfigAuditEntry{
			ID: domain.NewID(), Key: config.KeyRSSFeedURLs, OldValue: &oldValue, NewValue: nil,
			Version: 0, ChangedAt: now, ChangedBy: ownerActorID,
		})
	})
}
