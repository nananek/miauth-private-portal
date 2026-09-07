// This file is Issue #53's (OWUI-B) reply-tree path construction and
// branch-continuation rule (plan §5.2, ADR-0005 D2/D4/D5). It depends
// only on internal/domain, like the rest of this package.
package openwebui

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/nananek/miauth-private-portal/internal/domain"
)

// Errors BuildTurnPath returns. Each is a distinct fail-closed reason a
// caller (the enqueue-time bridge, or a durable job's runtime recheck)
// tells apart: the bridge logs the category and skips enqueueing, while
// the job records it as the turn's failure_category (domain.
// FailureCategoryPathIneligible/Orphan/Cycle/RequestTooLarge's sibling
// bound).
var (
	// ErrPathCycle is a reply-tree walk that revisited an entry, which
	// local data must never produce (domain.FailureCategoryCycle).
	ErrPathCycle = errors.New("openwebui: reply-tree path revisits an entry")
	// ErrPathOrphan is a walk that could not reach the thread root — a
	// missing parent, or a parent whose thread_id does not match target's
	// own (domain.FailureCategoryOrphan).
	ErrPathOrphan = errors.New("openwebui: reply-tree path could not reach the thread root")
	// ErrPathIneligible is a path node the roadmap requires be treated as
	// a hard failure rather than silently skipped or truncated: hidden,
	// archived, or an entry kind/author this bridge never sends
	// (domain.FailureCategoryPathIneligible).
	ErrPathIneligible = errors.New("openwebui: reply-tree path contains an ineligible node")
	// ErrPathTooLong is a path whose length (including the new turn being
	// asked) exceeds PathBounds.MaxContextMessages
	// (domain.FailureCategoryRequestTooLarge).
	ErrPathTooLong = errors.New("openwebui: reply-tree path exceeds the configured message bound")
)

const (
	pathRoleUser      = "user"
	pathRoleAssistant = "assistant"
)

// PathNode is one entry BuildTurnPath admitted into a turn's context, in
// root-to-parent order, with the role it maps to on the wire.
type PathNode struct {
	Entry domain.Entry
	Role  string
}

// TurnPath is the root-to-parent context BuildTurnPath assembled for one
// new turn. It never includes the new turn's own entry — ProviderMessages
// converts it to the wire sequence a Provider call sends as context, and
// the caller appends the new turn separately.
type TurnPath struct {
	Nodes []PathNode
}

// PathBounds configures BuildTurnPath's size limit.
type PathBounds struct {
	// MaxContextMessages bounds len(path)+1 (the +1 accounts for the new
	// turn being asked, which is not itself part of the path). Zero
	// means unbounded — every production caller supplies
	// config.OpenWebUIConfig.MaxContextMessages, which validates to a
	// positive value.
	MaxContextMessages int
}

// BuildTurnPath walks target's reply chain from its parent back to the
// thread root, admitting only nodes ADR-0005 D4's fail-closed rule
// allows into a turn's context: an owner's own visible posts (role
// "user") and prior assistant replies not hidden or archived (role
// "assistant"), whether authored by the shared assistant actor (#9) or
// an Open WebUI VirtualActor (a prior turn on a different branch).
// Anything else — a news/mail/system entry, a hidden or archived one, an
// unresolvable author, a cycle, or a walk that cannot reach the root —
// fails the whole path closed (roadmap: "does not silently skip it or
// treat it as a root"). The returned path is root-to-parent order and
// never includes target itself.
func BuildTurnPath(ctx context.Context, repos domain.Repos, target domain.Entry, bounds PathBounds) (TurnPath, error) {
	var reversed []PathNode
	visited := map[string]bool{target.ID: true}

	currentID := target.ParentEntryID
	for currentID != nil {
		if visited[*currentID] {
			return TurnPath{}, ErrPathCycle
		}
		visited[*currentID] = true

		node, err := repos.Entries.Get(ctx, *currentID)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				return TurnPath{}, ErrPathOrphan
			}
			return TurnPath{}, fmt.Errorf("openwebui: build turn path: get entry: %w", err)
		}
		if node.ThreadID != target.ThreadID {
			return TurnPath{}, ErrPathOrphan
		}

		role, err := pathNodeRole(ctx, repos, node)
		if err != nil {
			return TurnPath{}, err
		}

		reversed = append(reversed, PathNode{Entry: node, Role: role})
		currentID = node.ParentEntryID
	}

	if bounds.MaxContextMessages > 0 && len(reversed)+1 > bounds.MaxContextMessages {
		return TurnPath{}, ErrPathTooLong
	}

	nodes := make([]PathNode, len(reversed))
	for i, n := range reversed {
		nodes[len(reversed)-1-i] = n
	}
	return TurnPath{Nodes: nodes}, nil
}

// pathNodeRole classifies one reply-tree node per ADR-0005 D4's
// fail-closed rule. See BuildTurnPath's doc comment for the admitted
// shapes; everything else is ErrPathIneligible, never a silent skip.
func pathNodeRole(ctx context.Context, repos domain.Repos, node domain.Entry) (string, error) {
	if node.HiddenAt != nil || node.ArchivedAt != nil {
		return "", ErrPathIneligible
	}
	switch node.Kind {
	case domain.EntryUserPost:
		author, err := repos.Actors.Get(ctx, node.AuthorActorID)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				return "", ErrPathIneligible
			}
			return "", fmt.Errorf("openwebui: build turn path: get author: %w", err)
		}
		if !author.IsLoginable() {
			return "", ErrPathIneligible
		}
		return pathRoleUser, nil
	case domain.EntryLLMReply, domain.EntryLLMFollowUp:
		author, err := repos.Actors.Get(ctx, node.AuthorActorID)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				return "", ErrPathIneligible
			}
			return "", fmt.Errorf("openwebui: build turn path: get author: %w", err)
		}
		if author.Type != domain.ActorAssistant && author.Type != domain.ActorOpenWebUIModel {
			return "", ErrPathIneligible
		}
		return pathRoleAssistant, nil
	default:
		return "", ErrPathIneligible
	}
}

// mentionReplacement is what stripMentionTagsForProvider substitutes
// for an entire matched @mention, username included: an external LLM
// provider gets no more reason to address, or reason to believe it
// knows, a specific local username than it would any other redacted
// detail.
const mentionReplacement = "[mention removed]"

// mentionPattern matches one Misskey-style @mention token anchored at
// its own start (the "^" matches the start of whatever suffix of body
// mentionPattern is applied to, i.e. right at a candidate "@" —
// stripMentionTagsForProvider never calls FindStringIndex anywhere
// else): a leading "@", a username of letters/digits/underscores, and
// an optional "@host" suffix taking any of the shapes Aria/Misskey wire
// mentions actually use — a dotted hostname (each "."-separated label
// letters/digits/hyphens, so a sentence-ending period immediately after
// the host is never consumed, since it is never followed by another
// label), a bracketed IPv6 literal ("@[::1]"), and either an optional
// trailing ":port". This pattern is deliberately generic (not pinned to
// one known username, unlike internal/timeline.Service's own
// self-mention detector) since any actor's mention — the owner, or an
// Open WebUI VirtualActor's arbitrary slug@host — must be caught here.
var mentionPattern = regexp.MustCompile(
	`^@[A-Za-z0-9_]+(?:@(?:\[[0-9A-Za-z:]+\]|[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*)(?::[0-9]+)?)?`,
)

// isMentionBoundaryByte reports whether b can never itself be part of a
// username, i.e. b immediately preceding an "@" cannot make that "@"
// the middle of a longer token (email-local-part@host and the like).
func isMentionBoundaryByte(b byte) bool {
	return !(b == '_' || (b >= '0' && b <= '9') || (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z'))
}

// stripMentionTagsForProvider rewrites every Misskey-style @mention in
// body (e.g. "@luna@ai.tail2c8c7.ts.net" or a bare "@owner") to
// mentionReplacement before the text is sent to an external LLM
// provider: neither the "@"/host wire syntax nor the bare username
// itself is something the model should see or need to address. This
// never touches domain.Entry.Body itself — only the copy of the text
// placed in a Provider-facing Message (Issue #70).
//
// This scans byte by byte instead of doing one regexp.ReplaceAllString
// pass over the whole body, deliberately: a single non-overlapping
// pass's own "boundary" group (requiring a non-word byte, or start of
// string, immediately before the "@") consumes that boundary byte as
// part of its match, so a second mention starting immediately after the
// first one's host — with no separating space or punctuation between
// them — finds the one byte that would have qualified it as a boundary
// already spent by the previous match, and is left completely
// unstripped. Scanning byte by byte lets a mention that starts exactly
// where the previous one ended qualify as a boundary on its own
// (justStrippedMention), independent of what byte used to sit there.
func stripMentionTagsForProvider(body string) string {
	var sb strings.Builder
	sb.Grow(len(body))
	justStrippedMention := false
	for i := 0; i < len(body); {
		c := body[i]
		if c == '@' && (i == 0 || justStrippedMention || isMentionBoundaryByte(body[i-1])) {
			if loc := mentionPattern.FindStringIndex(body[i:]); loc != nil {
				sb.WriteString(mentionReplacement)
				i += loc[1]
				justStrippedMention = true
				continue
			}
		}
		sb.WriteByte(c)
		i++
		justStrippedMention = false
	}
	return sb.String()
}

// ProviderMessages converts path's root-to-parent nodes into the
// data-minimized sequence a Provider call sends as context: role and
// body only, never a local entry id, Misskey metadata, credentials, or a
// system prompt (roadmap: "The sequence carries no local IDs, Misskey
// metadata, credentials, system prompt"). Body is each entry's own text
// with any Misskey-style @mention — username included — masked out
// (Issue #70) — domain.Entry.Body itself is untouched. The caller
// appends the new turn's own Message separately
// (StartChatRequest.NewTurn / ContinueTurnRequest.NewTurn).
func ProviderMessages(path TurnPath) []Message {
	messages := make([]Message, len(path.Nodes))
	for i, n := range path.Nodes {
		messages[i] = Message{Role: n.Role, Content: stripMentionTagsForProvider(n.Entry.Body)}
	}
	return messages
}

// LinkContinuation is what SelectBranch returns when target's post
// continues an existing branch rather than starting a new one.
type LinkContinuation struct {
	Link domain.OpenWebUIConversationLink
}

// SelectBranch applies ADR-0005 D4's branch rule: target continues a
// ready link only when target's parent is exactly that link's current
// head (its latest non-tombstoned succeeded turn's assistant entry) and
// no non-tombstoned turn already replies to that same parent. Every
// other case — a thread root, a reply to an earlier node, a second reply
// to the same head, or a re-ask after a failed turn — returns (nil, nil)
// so the caller starts a new branch instead, matching the roadmap's
// "must not mix another branch into the same remote chat".
func SelectBranch(ctx context.Context, repos domain.Repos, target domain.Entry) (*LinkContinuation, error) {
	if target.ParentEntryID == nil {
		return nil, nil
	}
	parentID := *target.ParentEntryID

	links, err := repos.OpenWebUILinks.ListByThread(ctx, target.ThreadID)
	if err != nil {
		return nil, fmt.Errorf("openwebui: select branch: list links: %w", err)
	}
	for _, link := range links {
		if link.State != domain.LinkReady {
			continue
		}
		turns, err := repos.OpenWebUITurnLinks.ListByLink(ctx, link.ID)
		if err != nil {
			return nil, fmt.Errorf("openwebui: select branch: list turns: %w", err)
		}
		head := linkHead(turns)
		if head == nil || *head != parentID {
			continue
		}
		if turnAlreadyRepliesTo(turns, parentID) {
			continue
		}
		continuation := link
		return &LinkContinuation{Link: continuation}, nil
	}
	return nil, nil
}

// linkHead returns the assistant entry id of turns' latest (in the
// (created_at, id) order ListByLink already returns) non-tombstoned
// succeeded turn, or nil if there is none.
func linkHead(turns []domain.OpenWebUITurnLink) *string {
	var head *string
	for _, t := range turns {
		if t.TombstonedAt != nil || t.Status != domain.TurnSucceeded || t.AssistantEntryID == nil {
			continue
		}
		head = t.AssistantEntryID
	}
	return head
}

// turnAlreadyRepliesTo reports whether any non-tombstoned turn in turns
// already has localParentID as its LocalParentID — regardless of that
// turn's own status, since a failed or still-pending turn off the same
// parent still means a second reply to it must start a new branch
// (ADR-0005 D4), not silently attach to the first turn's remote chat.
func turnAlreadyRepliesTo(turns []domain.OpenWebUITurnLink, localParentID string) bool {
	for _, t := range turns {
		if t.TombstonedAt != nil {
			continue
		}
		if t.LocalParentID != nil && *t.LocalParentID == localParentID {
			return true
		}
	}
	return false
}
