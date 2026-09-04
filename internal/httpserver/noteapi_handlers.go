package httpserver

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/llmclassify"
	"github.com/nananek/miauth-private-portal/internal/llmreply"
	"github.com/nananek/miauth-private-portal/internal/logging"
	"github.com/nananek/miauth-private-portal/internal/timeline"
)

// defaultTimelineLimit and maxTimelineLimit bound /api/notes/timeline and
// /api/notes/children paging. docs/compat/aria-v1.5.11.md leaves the
// server's exact limit bounds 要実機確認 (needs real-instance
// verification); Aria's observed request always sends limit: 30, and 100
// is a conventional Misskey-style upper clamp chosen for this
// implementation rather than an unbounded caller-supplied value.
const (
	defaultTimelineLimit = 30
	maxTimelineLimit     = 100
)

// writeJSON writes status and v as this service's default JSON success
// response.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// decodeJSONBody decodes r's JSON body into T, treating an empty body the
// same as `{}` (docs/compat/aria-v1.5.11.md: several endpoints, e.g.
// /api/meta and /api/endpoints, send an empty object). It reports false
// only for actually malformed JSON.
func decodeJSONBody[T any](r *http.Request) (T, bool) {
	var v T
	if r.Body == nil {
		return v, true
	}
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
		if errors.Is(err, io.EOF) {
			return v, true
		}
		return v, false
	}
	return v, true
}

// entryVisible reports whether e should ever be projected onto a wire
// Note: an archived or hidden entry must not be, matching notes/show's
// "unify not-found and hidden" treatment used consistently across every
// note-reading endpoint below.
func entryVisible(e domain.Entry) bool {
	return e.ArchivedAt == nil && e.HiddenAt == nil
}

// handleMeta handles POST /api/meta, anonymous and body-independent
// (Aria sends `{}`). It returns the configured LOCAL_ORIGIN.
func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, metaResponse{URI: s.localOrigin, Features: metaFeatures{MiAuth: true}})
}

type metaResponse struct {
	URI      string       `json:"uri"`
	Features metaFeatures `json:"features"`
}

type metaFeatures struct {
	MiAuth bool `json:"miauth"`
}

// implementedEndpoints is POST /api/endpoints' anonymous response: only
// the note-API paths this issue actually implements, so Aria's edit-path
// probe (docs/compat/aria-v1.5.11.md) never sees notes/update advertised.
var implementedEndpoints = []string{
	"meta",
	"endpoints",
	"i",
	"notes/create",
	"notes/timeline",
	"notes/show",
	"notes/conversation",
	"notes/children",
}

func (s *Server) handleEndpoints(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, implementedEndpoints)
}

// handleAPII handles POST /api/i. See meDetailed's doc comment for why
// this always returns the MeDetailed superset regardless of which of
// Aria's two observed call sites is asking.
func (s *Server) handleAPII(w http.ResponseWriter, r *http.Request) {
	actorID := LocalActorIDFromContext(r.Context())
	owner, err := s.miauth.DescribeOwner(r.Context(), actorID)
	if err != nil {
		s.logger.Error("describe owner failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
		writeInternalError(w)
		return
	}
	writeJSON(w, http.StatusOK, newMeDetailed(owner, s.notesCountForOwner(r.Context(), owner.ActorID)))
}

// notesCreateRequest is the subset of Aria's create/reply request this
// service accepts. Every field beyond text/replyId/visibility/fileIds is
// present only so handleNotesCreate can detect it and fail explicitly
// (AGENTS.md: "Unsupported endpoints must fail explicitly and
// consistently; do not return fabricated success" — the same rule
// applies to unsupported fields on a supported endpoint).
type notesCreateRequest struct {
	Text               *string         `json:"text"`
	ReplyID            *string         `json:"replyId"`
	Visibility         *string         `json:"visibility"`
	FileIDs            []string        `json:"fileIds"`
	VisibleUserIDs     []string        `json:"visibleUserIds"`
	ReactionAcceptance *string         `json:"reactionAcceptance"`
	RenoteID           *string         `json:"renoteId"`
	ChannelID          *string         `json:"channelId"`
	Poll               json.RawMessage `json:"poll"`
	ScheduledAt        *int64          `json:"scheduledAt"`
}

type createdNoteResponse struct {
	CreatedNote note `json:"createdNote"`
}

func (s *Server) handleNotesCreate(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeJSONBody[notesCreateRequest](r)
	if !ok {
		writeInvalidParam(w, "malformed request body")
		return
	}

	switch {
	case len(req.VisibleUserIDs) > 0:
		writeUnsupportedFeature(w, "visibleUserIds")
		return
	case req.ReactionAcceptance != nil:
		writeUnsupportedFeature(w, "reactionAcceptance")
		return
	case req.RenoteID != nil:
		writeUnsupportedFeature(w, "renoteId")
		return
	case req.ChannelID != nil:
		writeUnsupportedFeature(w, "channelId")
		return
	case len(req.Poll) > 0 && string(req.Poll) != "null":
		writeUnsupportedFeature(w, "poll")
		return
	case req.ScheduledAt != nil:
		writeUnsupportedFeature(w, "scheduledAt")
		return
	case len(req.FileIDs) > 0:
		writeUnsupportedFeature(w, "fileIds")
		return
	case req.Visibility != nil && *req.Visibility != "public":
		writeUnsupportedFeature(w, "visibility")
		return
	}

	if req.Text == nil || *req.Text == "" {
		writeInvalidParam(w, "text is required")
		return
	}

	job := s.llmReplyJob(*req.Text)
	classificationJob := s.llmClassificationJob()

	var entry domain.Entry
	var err error
	if req.ReplyID != nil && *req.ReplyID != "" {
		// timeline.CreateReply's own parent lookup (repos.Entries.Get) does
		// not filter archived/hidden rows — by design, per GetEntry's doc
		// comment, enforcing visibility is this package's job. Without this
		// check, replying to a hidden/archived parent would silently
		// succeed, contradicting the uniform NO_SUCH_NOTE treatment every
		// note-reading endpoint gives the same parent ID (see entryVisible).
		parent, getErr := s.timeline.GetEntry(r.Context(), *req.ReplyID)
		switch {
		case errors.Is(getErr, domain.ErrNotFound):
			writeNoSuchNote(w)
			return
		case getErr != nil:
			s.logger.Error("get reply parent failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", getErr.Error())
			writeInternalError(w)
			return
		case !entryVisible(parent):
			writeNoSuchNote(w)
			return
		}

		entry, err = s.timeline.CreateReply(r.Context(), *req.ReplyID, domain.EntryUserPost, *req.Text, job, classificationJob)
		if errors.Is(err, timeline.ErrParentNotFound) {
			writeNoSuchNote(w)
			return
		}
	} else {
		entry, err = s.timeline.CreateRoot(r.Context(), domain.EntryUserPost, *req.Text, job, classificationJob)
	}
	if err != nil {
		s.logger.Error("create note failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
		writeInternalError(w)
		return
	}

	owner, err := s.miauth.DescribeOwner(r.Context(), LocalActorIDFromContext(r.Context()))
	if err != nil {
		s.logger.Error("describe owner failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
		writeInternalError(w)
		return
	}
	writeJSON(w, http.StatusOK, createdNoteResponse{CreatedNote: newNote(entry, newUserLiteFromOwner(owner))})
}

// llmReplyJob decides whether posting body should enqueue an
// Issue #9 "llm_generation" job, evaluating only internal/llmreply's
// synchronous, LLM-call-free policy (internal/llmreply.DecideReply)
// against body itself — never the wider thread, which the job handler
// fetches only once actually claimed. It returns nil whenever
// s.llmEnabled is false (LLM_ENABLED's safe default: no job is ever
// enqueued and no request ever reaches a provider) or the policy does
// not call for generation. The returned Job has no SourceEntryID yet:
// timeline.Service.CreateRoot/CreateReply's enqueueForEntry sets it
// atomically to the entry actually created, in the same transaction
// (AGENTS.md: "Commit the post and durable job intent atomically").
func (s *Server) llmReplyJob(body string) *domain.Job {
	if !s.llmEnabled {
		return nil
	}
	decision := llmreply.DecideReply(body)
	if !decision.ShouldGenerate {
		return nil
	}
	payload, err := llmreply.NewJobPayload(decision)
	if err != nil {
		// decision's fields are plain strings; NewJobPayload can only
		// fail here on an encoding bug, never on post content. Skip
		// generation rather than fail the note creation itself
		// (AGENTS.md: "Failure of an LLM...must never make a user post
		// disappear").
		s.logger.Warn("llm reply job payload encode failed", "error_category", "encode_error")
		return nil
	}
	now := time.Now().UTC()
	return &domain.Job{
		ID:             domain.NewID(),
		JobType:        llmreply.JobType,
		Payload:        payload,
		PayloadVersion: 1,
		State:          domain.JobPending,
		NextRunAt:      now,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
}

// llmClassificationJob returns Issue #10's "llm_classification" job to
// enqueue for a newly created user_post, or nil when
// s.llmClassificationEnabled is false (LLM_CLASSIFICATION_ENABLED's safe
// default: no job is ever enqueued and no request ever reaches a
// provider). Unlike llmReplyJob, there is no body-dependent policy to
// evaluate: classification v1 is enqueued unconditionally for every
// user_post (internal/llmclassify's scope is user_post only; generated
// llm_reply/llm_follow_up entries, created via
// timeline.Service.CreateGeneratedReply rather than this handler, are
// never classified). The returned Job has no SourceEntryID yet, set
// atomically by CreateRoot/CreateReply's enqueueForEntry, same as
// llmReplyJob.
func (s *Server) llmClassificationJob() *domain.Job {
	if !s.llmClassificationEnabled {
		return nil
	}
	payload, err := llmclassify.NewJobPayload()
	if err != nil {
		// llmclassify.NewJobPayload can only fail here on an encoding bug,
		// never on post content. Skip classification rather than fail the
		// note creation itself (AGENTS.md: "Failure of an LLM...must
		// never make a user post disappear").
		s.logger.Warn("llm classification job payload encode failed", "error_category", "encode_error")
		return nil
	}
	now := time.Now().UTC()
	return &domain.Job{
		ID:             domain.NewID(),
		JobType:        llmclassify.JobType,
		Payload:        payload,
		PayloadVersion: 1,
		State:          domain.JobPending,
		NextRunAt:      now,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
}

type notesTimelineRequest struct {
	Limit   *int    `json:"limit"`
	UntilID *string `json:"untilId"`
	// WithRenotes, WithFiles, and AllowPartial are accepted and ignored:
	// Aria always sends them, but this service has no renote/file
	// filtering or partial-result concept to honor them with (see
	// docs/compat/aria-v1.5.11.md's timeline notes).
	WithRenotes  *bool `json:"withRenotes"`
	WithFiles    *bool `json:"withFiles"`
	AllowPartial *bool `json:"allowPartial"`
}

// handleNotesTimeline handles POST /api/notes/timeline: the home
// timeline's initial load, reload, and untilId-paginated older pages,
// always newest-first (see EntryRepository.ListTimelineDesc).
// sinceId/sinceDate/untilDate are not implemented: Aria's home-timeline
// call path never sends them (docs/compat/aria-v1.5.11.md).
func (s *Server) handleNotesTimeline(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeJSONBody[notesTimelineRequest](r)
	if !ok {
		writeInvalidParam(w, "malformed request body")
		return
	}

	limit := defaultTimelineLimit
	if req.Limit != nil && *req.Limit > 0 {
		limit = *req.Limit
	}
	if limit > maxTimelineLimit {
		limit = maxTimelineLimit
	}

	var before *domain.Cursor
	if req.UntilID != nil && *req.UntilID != "" {
		anchor, err := s.timeline.GetEntry(r.Context(), *req.UntilID)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				// A stale/unknown untilId has nothing older to page to;
				// an empty page is the pagination-loop-safe response
				// (Aria stops paging on an empty result), not an error.
				writeJSON(w, http.StatusOK, []note{})
				return
			}
			s.logger.Error("resolve timeline untilId failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
			writeInternalError(w)
			return
		}
		before = &domain.Cursor{CreatedAt: anchor.CreatedAt, ID: anchor.ID}
	}

	entries, err := s.timeline.GetTimelineDesc(r.Context(), before, limit, false)
	if err != nil {
		s.logger.Error("get timeline failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
		writeInternalError(w)
		return
	}

	owner, err := s.miauth.DescribeOwner(r.Context(), LocalActorIDFromContext(r.Context()))
	if err != nil {
		s.logger.Error("describe owner failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
		writeInternalError(w)
		return
	}

	notes := make([]note, 0, len(entries))
	for _, e := range entries {
		notes = append(notes, newNote(e, s.resolveUserLite(r.Context(), e.AuthorActorID, owner)))
	}
	writeJSON(w, http.StatusOK, notes)
}

type notesShowRequest struct {
	NoteID string `json:"noteId"`
}

// handleNotesShow handles POST /api/notes/show: one note by ID, or the
// uniform NO_SUCH_NOTE error for an unknown, archived, or hidden ID (see
// writeNoSuchNote).
func (s *Server) handleNotesShow(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeJSONBody[notesShowRequest](r)
	if !ok || req.NoteID == "" {
		writeInvalidParam(w, "noteId is required")
		return
	}

	entry, err := s.timeline.GetEntry(r.Context(), req.NoteID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			writeNoSuchNote(w)
			return
		}
		s.logger.Error("get note failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
		writeInternalError(w)
		return
	}
	if !entryVisible(entry) {
		writeNoSuchNote(w)
		return
	}

	owner, err := s.miauth.DescribeOwner(r.Context(), LocalActorIDFromContext(r.Context()))
	if err != nil {
		s.logger.Error("describe owner failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
		writeInternalError(w)
		return
	}
	writeJSON(w, http.StatusOK, newNote(entry, s.resolveUserLite(r.Context(), entry.AuthorActorID, owner)))
}

type notesConversationRequest struct {
	NoteID string `json:"noteId"`
}

// handleNotesConversation handles POST /api/notes/conversation: the
// subject note's ancestor chain, oldest-first (root, then its child, ...,
// then the subject's direct parent), excluding the subject itself and
// any archived/hidden ancestor. Ordering direction is this issue's fixed
// decision for a call path docs/compat/aria-v1.5.11.md leaves 要実機確認.
func (s *Server) handleNotesConversation(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeJSONBody[notesConversationRequest](r)
	if !ok || req.NoteID == "" {
		writeInvalidParam(w, "noteId is required")
		return
	}

	subject, err := s.timeline.GetEntry(r.Context(), req.NoteID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			writeNoSuchNote(w)
			return
		}
		s.logger.Error("get note failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
		writeInternalError(w)
		return
	}
	if !entryVisible(subject) {
		writeNoSuchNote(w)
		return
	}

	thread, err := s.timeline.GetThread(r.Context(), subject.ThreadID)
	if err != nil {
		s.logger.Error("get thread failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
		writeInternalError(w)
		return
	}
	byID := make(map[string]domain.Entry, len(thread))
	for _, e := range thread {
		byID[e.ID] = e
	}

	var chain []domain.Entry
	for cur := subject.ParentEntryID; cur != nil; {
		e, ok := byID[*cur]
		if !ok {
			break
		}
		chain = append(chain, e)
		cur = e.ParentEntryID
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}

	owner, err := s.miauth.DescribeOwner(r.Context(), LocalActorIDFromContext(r.Context()))
	if err != nil {
		s.logger.Error("describe owner failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
		writeInternalError(w)
		return
	}

	notes := make([]note, 0, len(chain))
	for _, e := range chain {
		if !entryVisible(e) {
			continue
		}
		notes = append(notes, newNote(e, s.resolveUserLite(r.Context(), e.AuthorActorID, owner)))
	}
	writeJSON(w, http.StatusOK, notes)
}

type notesChildrenRequest struct {
	NoteID  string  `json:"noteId"`
	Depth   *int    `json:"depth"`
	UntilID *string `json:"untilId"`
	Limit   *int    `json:"limit"`
}

// handleNotesChildren handles POST /api/notes/children: the subject
// note's direct replies only (depth 1; deeper descendants are out of
// scope, matching EntryRepository.ListChildren), excluding archived/
// hidden children, paginated by continuing after untilId in the same
// oldest-first order ListChildren already returns.
func (s *Server) handleNotesChildren(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeJSONBody[notesChildrenRequest](r)
	if !ok || req.NoteID == "" {
		writeInvalidParam(w, "noteId is required")
		return
	}

	subject, err := s.timeline.GetEntry(r.Context(), req.NoteID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			writeNoSuchNote(w)
			return
		}
		s.logger.Error("get note failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
		writeInternalError(w)
		return
	}
	if !entryVisible(subject) {
		writeNoSuchNote(w)
		return
	}

	children, err := s.timeline.GetChildren(r.Context(), req.NoteID)
	if err != nil {
		s.logger.Error("get children failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
		writeInternalError(w)
		return
	}

	limit := defaultTimelineLimit
	if req.Limit != nil && *req.Limit > 0 {
		limit = *req.Limit
	}
	if limit > maxTimelineLimit {
		limit = maxTimelineLimit
	}
	page := paginateAfterID(children, req.UntilID, limit)

	owner, err := s.miauth.DescribeOwner(r.Context(), LocalActorIDFromContext(r.Context()))
	if err != nil {
		s.logger.Error("describe owner failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
		writeInternalError(w)
		return
	}

	notes := make([]note, 0, len(page))
	for _, e := range page {
		notes = append(notes, newNote(e, s.resolveUserLite(r.Context(), e.AuthorActorID, owner)))
	}
	writeJSON(w, http.StatusOK, notes)
}

// paginateAfterID returns up to limit archived/hidden-excluded entries
// strictly after the one matching untilID in entries' existing order.
// entries is GetChildren's full result, including archived/hidden
// children: the untilID lookup itself must search this unfiltered list,
// not just the visible ones, so an anchor that was visible on an earlier
// page but has since been hidden/archived still resolves to its correct
// position and pagination keeps surfacing later visible children —
// mirroring handleNotesTimeline's GetEntry-based untilId lookup, which
// likewise ignores visibility when resolving the cursor. If untilID is
// nil/empty, it returns from the start. Only a untilID that matches no
// entry at all (a stale/unknown ID, never seen in this list) yields an
// empty page rather than restarting from the beginning: that keeps a
// client that pages by repeating a bogus last-seen ID safe from looping
// back over the same page forever. This continues GetChildren's existing
// oldest-first order rather than introducing a second, newest-first
// children query: Issue #7's plan defers a dedicated ListChildrenDesc
// repository method until pagination performance actually requires it.
func paginateAfterID(entries []domain.Entry, untilID *string, limit int) []domain.Entry {
	start := 0
	if untilID != nil && *untilID != "" {
		found := false
		for i, e := range entries {
			if e.ID == *untilID {
				start = i + 1
				found = true
				break
			}
		}
		if !found {
			return nil
		}
	}

	page := make([]domain.Entry, 0, limit)
	for _, e := range entries[start:] {
		if !entryVisible(e) {
			continue
		}
		page = append(page, e)
		if len(page) == limit {
			break
		}
	}
	return page
}
