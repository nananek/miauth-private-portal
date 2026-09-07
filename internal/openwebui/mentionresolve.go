// This file is Issue #75 PR3's mention-based model routing: which of a
// workspace's active models, if any, an owner's post addresses by
// @mention. Bridge.EnqueueTurn is the one caller, and applies the
// decision table ResolveModelMentions' own doc comment describes.
package openwebui

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// ResolveModelMentions scans body for Misskey-style @mention tokens —
// the same shape mentionTokens (path.go) finds and
// stripMentionTagsForProvider omits before a message reaches a
// provider — and returns the distinct set of workspace's own active
// models addressed by one, in the order each was first seen.
//
// A token is a candidate only when its optional "@host" suffix is
// either absent or matches workspace.PresentationHost exactly
// (case-insensitively — a presentation host is a DNS name): a mention of
// some other remote actor entirely (`@someone@elsewhere.example`) is
// never a model candidate no matter what its bare username reads as.
// Among candidates, a slug naming no model in this workspace, or naming
// an inactive one, is silently excluded — folded into the same "no
// match" outcome a body with no mention at all reaches, never an error
// of its own. Matching against actor_slug is case-insensitive because a
// person typing a mention may not reproduce a generated slug's case
// exactly, even though GenerateActorSlug itself only ever produces
// lowercase.
//
// The caller applies the actual routing rule this returns the raw
// material for: zero matches means fall back to the workspace's
// configured default model; exactly one means use it; two or more
// distinct matches is the "ambiguous_model_selection" case — Bridge.
// EnqueueTurn skips enqueueing entirely rather than guessing between
// them.
func ResolveModelMentions(ctx context.Context, repos domain.Repos, workspace domain.OpenWebUIWorkspace, body string) ([]domain.OpenWebUIModel, error) {
	var matched []domain.OpenWebUIModel
	seenSlugs := make(map[string]bool)

	for _, token := range mentionTokens(body) {
		rawSlug, host := splitMentionToken(token)
		slug := strings.ToLower(rawSlug)
		if host != "" && !strings.EqualFold(host, workspace.PresentationHost) {
			continue
		}
		if seenSlugs[slug] {
			continue
		}
		seenSlugs[slug] = true

		model, err := repos.OpenWebUIModels.GetByActorSlug(ctx, workspace.ID, slug)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				continue
			}
			return nil, fmt.Errorf("openwebui: resolve model mentions: get model by slug %q: %w", slug, err)
		}
		if !model.Active {
			continue
		}
		matched = append(matched, model)
	}
	return matched, nil
}

// splitMentionToken splits a mentionTokens result (e.g.
// "@luna@ai.tail2c8c7.ts.net" or a bare "@owner") into its username and
// host. host is "" when the token carries no "@host" suffix. Safe to
// split on the first '@' after the leading one: mentionPattern's
// username class ([A-Za-z0-9_]+) never itself contains '@', so the next
// '@' in the token, if any, is always exactly the host separator.
func splitMentionToken(token string) (slug, host string) {
	rest := token[1:]
	if idx := strings.IndexByte(rest, '@'); idx >= 0 {
		return rest[:idx], rest[idx+1:]
	}
	return rest, ""
}
