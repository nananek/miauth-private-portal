package httpserver

import (
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/drive"
	"github.com/nananek/miauth-private-portal/internal/logging"
	"github.com/nananek/miauth-private-portal/internal/miauth"
)

// maxDriveTextFieldBytes bounds any single non-file multipart form field
// (folderId, name, comment, isSensitive, the "i" token) this service
// reads out of a POST /api/drive/files/create request — generous for
// every field's real shape (an opaque ID, a filename, a short comment)
// while still refusing to buffer an unbounded value, the same
// "bound request sizes" rule (AGENTS.md) withMaxBody enforces for the
// request as a whole.
const maxDriveTextFieldBytes = 4096

// Drive error IDs/codes below (no-such-file, folder-not-empty, ...) are
// this service's own invented-but-Misskey-flavored convention, the same
// one writeNoSuchNote/writeInvalidParam already establish
// (noteapi_errors.go's wireError doc comment: "an implementation
// contract this issue fixes, not something verified against a real
// Misskey instance yet") — not literal real-Misskey error IDs (which are
// UUIDs, not kebab-case strings), since no traced Aria call site
// branches on a Drive error's specific id/code (see drive_wire.go's
// request type doc comments for what is and is not traced).

func writeNoSuchFile(w http.ResponseWriter) {
	writeWireError(w, http.StatusBadRequest, "no-such-file", "NO_SUCH_FILE", "No such file.", "client", nil)
}

func writeNoSuchFolder(w http.ResponseWriter) {
	writeWireError(w, http.StatusBadRequest, "no-such-folder", "NO_SUCH_FOLDER", "No such folder.", "client", nil)
}

func writeFolderNotEmpty(w http.ResponseWriter) {
	writeWireError(w, http.StatusBadRequest, "folder-not-empty", "FOLDER_NOT_EMPTY", "This folder is not empty.", "client", nil)
}

func writeFolderCycle(w http.ResponseWriter) {
	writeInvalidParam(w, "a folder cannot be moved inside its own descendant")
}

func writeFileTooLarge(w http.ResponseWriter) {
	writeWireError(w, http.StatusBadRequest, "file-too-large", "FILE_TOO_LARGE", "This file exceeds the maximum allowed size.", "client", nil)
}

// writeDriveFileError maps a drive.Service file-operation error to a
// wire response, logging (and generalizing to a 500) anything this
// package does not have a specific client-facing shape for.
func (s *Server) writeDriveFileError(w http.ResponseWriter, r *http.Request, op string, err error) {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		writeNoSuchFile(w)
	case errors.Is(err, drive.ErrInvalidImage):
		writeInvalidParam(w, "not a valid raster image")
	case errors.Is(err, drive.ErrFileTooLarge):
		writeFileTooLarge(w)
	case errors.Is(err, drive.ErrInvalidUploadURL):
		writeInvalidParam(w, "invalid url")
	case errors.Is(err, drive.ErrUploadFromURLFailed):
		writeInvalidParam(w, "could not fetch url")
	default:
		s.logger.Error(op+" failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
		writeInternalError(w)
	}
}

func (s *Server) writeDriveFolderError(w http.ResponseWriter, r *http.Request, op string, err error) {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		writeNoSuchFolder(w)
	case errors.Is(err, drive.ErrFolderNotEmpty):
		writeFolderNotEmpty(w)
	case errors.Is(err, drive.ErrFolderCycle):
		writeFolderCycle(w)
	default:
		s.logger.Error(op+" failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
		writeInternalError(w)
	}
}

// handleDrive handles POST /api/drive: capacity/usage for the caller's
// own files.
func (s *Server) handleDrive(w http.ResponseWriter, r *http.Request) {
	actorID := LocalActorIDFromContext(r.Context())
	capacity, usage, err := s.drive.Stats(r.Context(), actorID)
	if err != nil {
		s.logger.Error("drive stats failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
		writeInternalError(w)
		return
	}
	writeJSON(w, http.StatusOK, driveResponse{Capacity: capacity, Usage: usage})
}

// handleDriveFiles handles POST /api/drive/files: a paginated listing of
// the caller's files directly inside req.FolderID (nil means Drive's
// root — never "every folder", matching Aria's own per-folder browsing).
func (s *Server) handleDriveFiles(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeJSONBody[driveFilesRequest](r)
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

	actorID := LocalActorIDFromContext(r.Context())
	files, err := s.drive.ListFiles(r.Context(), actorID, req.FolderID, req.UntilID, limit)
	if err != nil {
		s.writeDriveFileError(w, r, "list drive files", err)
		return
	}
	out := make([]driveFile, 0, len(files))
	for _, f := range files {
		out = append(out, s.projectDriveFile(f))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleDriveFilesShow handles POST /api/drive/files/show. Only fileId
// is implemented (see driveFilesShowRequest's doc comment).
func (s *Server) handleDriveFilesShow(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeJSONBody[driveFilesShowRequest](r)
	if !ok {
		writeInvalidParam(w, "malformed request body")
		return
	}
	if req.FileID == nil || *req.FileID == "" {
		writeInvalidParam(w, "fileId is required")
		return
	}

	actorID := LocalActorIDFromContext(r.Context())
	f, err := s.drive.ShowFile(r.Context(), actorID, *req.FileID)
	if err != nil {
		s.writeDriveFileError(w, r, "show drive file", err)
		return
	}
	writeJSON(w, http.StatusOK, s.projectDriveFile(f))
}

// readMultipartTextPart reads part fully (bounded by
// maxDriveTextFieldBytes), reporting false for an oversized or
// unreadable part rather than silently truncating it.
func readMultipartTextPart(part *multipart.Part) (string, bool) {
	data, err := io.ReadAll(io.LimitReader(part, maxDriveTextFieldBytes+1))
	if err != nil || int64(len(data)) > maxDriveTextFieldBytes {
		return "", false
	}
	return string(data), true
}

// handleDriveFilesCreate handles POST /api/drive/files/create — Aria's
// multipart/form-data upload (both DriveFilesNotifier.upload's
// file-path form and .uploadBinary's raw-bytes form arrive here
// identically: misskey_dart's postWithFile/postWithBinary both build the
// same multipart shape). Unlike every other Drive route, the caller's
// local API token arrives as a multipart form field named "i", not the
// JSON body RequireScope reads from (docs/compat/aria-v1.5.11.md's
// Drive API section traces ApiService.postWithFile/postWithBinary
// building the request as `FormData.fromMap({..., "i": token, "file":
// ...})`) — so this handler is registered directly, not wrapped in
// RequireScope, and authenticates itself against s.miauth once it has
// read the "i" part.
//
// The multipart body is read part-by-part via r.MultipartReader rather
// than r.ParseMultipartForm: ParseMultipartForm spills an
// over-maxMemory file part to a temp file this handler would then have
// to clean up, where reading each part through a bounded io.LimitReader
// keeps everything in memory, bounded, with nothing to clean up either
// way.
func (s *Server) handleDriveFilesCreate(w http.ResponseWriter, r *http.Request) {
	reader, err := r.MultipartReader()
	if err != nil {
		writeInvalidParam(w, "malformed multipart request")
		return
	}

	var (
		token, folderID, name, comment, isSensitiveRaw string
		haveToken, haveFolderID, haveComment           bool
		haveIsSensitive                                bool
		fileData                                       []byte
		haveFile                                       bool
	)

	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			writeInvalidParam(w, "malformed multipart request")
			return
		}
		switch part.FormName() {
		case "i":
			token, haveToken = readMultipartTextPart(part)
		case "folderId":
			folderID, haveFolderID = readMultipartTextPart(part)
		case "name":
			name, _ = readMultipartTextPart(part)
		case "comment":
			comment, haveComment = readMultipartTextPart(part)
		case "isSensitive":
			isSensitiveRaw, haveIsSensitive = readMultipartTextPart(part)
		case "file":
			data, err := io.ReadAll(io.LimitReader(part, s.driveMaxFileBytes+1))
			if err != nil {
				_ = part.Close()
				writeInvalidParam(w, "malformed file part")
				return
			}
			if int64(len(data)) > s.driveMaxFileBytes {
				_ = part.Close()
				writeFileTooLarge(w)
				return
			}
			fileData = data
			haveFile = true
		}
		_ = part.Close()
	}

	if !haveToken || token == "" {
		writeAuthenticationFailed(w)
		return
	}
	actorID, err := s.miauth.VerifyToken(r.Context(), token, miauth.ScopeWriteDrive)
	if err != nil {
		if errors.Is(err, miauth.ErrTokenInvalid) {
			writeAuthenticationFailed(w)
			return
		}
		s.logger.Error("local API token verification failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
		writeAuthenticationUnavailable(w)
		return
	}

	if !haveFile {
		writeInvalidParam(w, "file is required")
		return
	}

	var folderIDPtr *string
	if haveFolderID && folderID != "" {
		folderIDPtr = &folderID
	}
	var commentPtr *string
	if haveComment {
		commentPtr = &comment
	}

	f, err := s.drive.CreateFile(r.Context(), drive.CreateFileInput{
		OwnerActorID: actorID,
		FolderID:     folderIDPtr,
		Name:         name,
		Comment:      commentPtr,
		IsSensitive:  haveIsSensitive && isSensitiveRaw == "true",
		Data:         fileData,
	})
	if err != nil {
		s.writeDriveFileError(w, r, "create drive file", err)
		return
	}
	writeJSON(w, http.StatusOK, s.projectDriveFile(f))
}

// handleDriveFilesUploadFromUrl handles POST
// /api/drive/files/upload-from-url. Real Misskey performs this
// asynchronously; this service fetches synchronously before responding
// (see drive.Service.UploadFromURL's doc comment for why that is
// wire-compatible) and, matching real Misskey's own 204 response,
// returns no body.
func (s *Server) handleDriveFilesUploadFromUrl(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeJSONBody[driveFilesUploadFromUrlRequest](r)
	if !ok || req.URL == "" {
		writeInvalidParam(w, "url is required")
		return
	}

	actorID := LocalActorIDFromContext(r.Context())
	_, err := s.drive.UploadFromURL(r.Context(), drive.UploadFromURLInput{
		OwnerActorID: actorID,
		FolderID:     req.FolderID,
		URL:          req.URL,
		Comment:      req.Comment,
		IsSensitive:  req.IsSensitive != nil && *req.IsSensitive,
	})
	if err != nil {
		s.writeDriveFileError(w, r, "upload drive file from url", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// rawKeys decodes r's JSON body into a raw key set, so a handler can
// distinguish "field absent" from "field present" (including an
// explicit null) before applying only the fields a request actually
// named — the same technique handleAPIIUpdate uses.
func rawKeys(r *http.Request) (map[string]json.RawMessage, bool) {
	raw := map[string]json.RawMessage{}
	if r.Body == nil {
		return raw, true
	}
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil && !errors.Is(err, io.EOF) {
		return nil, false
	}
	return raw, true
}

// decodeRawStringPtr decodes raw[key] (if present) into a **string:
// absent -> nil (leave unchanged); present as JSON null -> a non-nil
// pointer to a nil *string (explicitly clear); present as a string ->
// a non-nil pointer to a non-nil *string (set this value).
func decodeRawStringPtr(raw map[string]json.RawMessage, key string) (**string, bool) {
	msg, present := raw[key]
	if !present {
		return nil, true
	}
	var v *string
	if err := json.Unmarshal(msg, &v); err != nil {
		return nil, false
	}
	return &v, true
}

// handleDriveFilesUpdate handles POST /api/drive/files/update. folderId
// and comment support an explicit null (clear the field); name and
// isSensitive do not need to, since every observed Aria call site that
// sets one always sets a real value (see drive.FileUpdate's doc
// comment).
func (s *Server) handleDriveFilesUpdate(w http.ResponseWriter, r *http.Request) {
	raw, ok := rawKeys(r)
	if !ok {
		writeInvalidParam(w, "malformed request body")
		return
	}
	var fileID string
	if err := decodeRawInto(raw, "fileId", &fileID); err != nil || fileID == "" {
		writeInvalidParam(w, "fileId is required")
		return
	}

	var upd drive.FileUpdate
	if msg, present := raw["name"]; present {
		var v string
		if err := json.Unmarshal(msg, &v); err != nil {
			writeInvalidParam(w, "name must be a string")
			return
		}
		upd.Name = &v
	}
	if msg, present := raw["isSensitive"]; present {
		var v bool
		if err := json.Unmarshal(msg, &v); err != nil {
			writeInvalidParam(w, "isSensitive must be a boolean")
			return
		}
		upd.IsSensitive = &v
	}
	if commentPtr, ok := decodeRawStringPtr(raw, "comment"); ok {
		upd.Comment = commentPtr
	} else {
		writeInvalidParam(w, "comment must be a string or null")
		return
	}
	if folderIDPtr, ok := decodeRawStringPtr(raw, "folderId"); ok {
		upd.FolderID = folderIDPtr
	} else {
		writeInvalidParam(w, "folderId must be a string or null")
		return
	}

	actorID := LocalActorIDFromContext(r.Context())
	f, err := s.drive.UpdateFile(r.Context(), actorID, fileID, upd)
	if err != nil {
		s.writeDriveFileError(w, r, "update drive file", err)
		return
	}
	writeJSON(w, http.StatusOK, s.projectDriveFile(f))
}

// decodeRawInto unmarshals raw[key] into dst if present; a missing key
// leaves dst unchanged (its caller treats that the same as an empty
// value).
func decodeRawInto(raw map[string]json.RawMessage, key string, dst any) error {
	msg, present := raw[key]
	if !present {
		return nil
	}
	return json.Unmarshal(msg, dst)
}

func (s *Server) handleDriveFilesDelete(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeJSONBody[driveFilesDeleteRequest](r)
	if !ok || req.FileID == "" {
		writeInvalidParam(w, "fileId is required")
		return
	}
	actorID := LocalActorIDFromContext(r.Context())
	if err := s.drive.DeleteFile(r.Context(), actorID, req.FileID); err != nil {
		s.writeDriveFileError(w, r, "delete drive file", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleDriveFilesAttachedNotes handles POST
// /api/drive/files/attached-notes. It always returns an empty list until
// Issue #77 PR6 adds entry_files (no traced schema, no note can be
// attached to any file yet) — but still verifies fileId names a file the
// caller owns first, so an unknown or foreign fileId is rejected the
// same way every other Drive route rejects one, rather than silently
// returning [] for a probe.
func (s *Server) handleDriveFilesAttachedNotes(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeJSONBody[driveFilesAttachedNotesRequest](r)
	if !ok || req.FileID == "" {
		writeInvalidParam(w, "fileId is required")
		return
	}
	actorID := LocalActorIDFromContext(r.Context())
	if _, err := s.drive.ShowFile(r.Context(), actorID, req.FileID); err != nil {
		s.writeDriveFileError(w, r, "attached notes", err)
		return
	}
	writeJSON(w, http.StatusOK, []note{})
}

func (s *Server) handleDriveFolders(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeJSONBody[driveFoldersRequest](r)
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

	actorID := LocalActorIDFromContext(r.Context())
	folders, err := s.drive.ListFolders(r.Context(), actorID, req.FolderID, req.UntilID, limit)
	if err != nil {
		s.writeDriveFolderError(w, r, "list drive folders", err)
		return
	}
	out := make([]driveFolder, 0, len(folders))
	for _, f := range folders {
		out = append(out, projectDriveFolder(f))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleDriveFoldersCreate(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeJSONBody[driveFoldersCreateRequest](r)
	if !ok {
		writeInvalidParam(w, "malformed request body")
		return
	}
	var name string
	if req.Name != nil {
		name = *req.Name
	}
	actorID := LocalActorIDFromContext(r.Context())
	f, err := s.drive.CreateFolder(r.Context(), actorID, name, req.ParentID)
	if err != nil {
		s.writeDriveFolderError(w, r, "create drive folder", err)
		return
	}
	writeJSON(w, http.StatusOK, projectDriveFolder(f))
}

func (s *Server) handleDriveFoldersShow(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeJSONBody[driveFoldersShowRequest](r)
	if !ok || req.FolderID == "" {
		writeInvalidParam(w, "folderId is required")
		return
	}
	actorID := LocalActorIDFromContext(r.Context())
	f, foldersCount, filesCount, err := s.drive.ShowFolder(r.Context(), actorID, req.FolderID)
	if err != nil {
		s.writeDriveFolderError(w, r, "show drive folder", err)
		return
	}
	writeJSON(w, http.StatusOK, projectDriveFolderWithCounts(f, foldersCount, filesCount))
}

func (s *Server) handleDriveFoldersUpdate(w http.ResponseWriter, r *http.Request) {
	raw, ok := rawKeys(r)
	if !ok {
		writeInvalidParam(w, "malformed request body")
		return
	}
	var folderID string
	if err := decodeRawInto(raw, "folderId", &folderID); err != nil || folderID == "" {
		writeInvalidParam(w, "folderId is required")
		return
	}

	var upd drive.FolderUpdate
	if msg, present := raw["name"]; present {
		var v string
		if err := json.Unmarshal(msg, &v); err != nil {
			writeInvalidParam(w, "name must be a string")
			return
		}
		upd.Name = &v
	}
	if parentIDPtr, ok := decodeRawStringPtr(raw, "parentId"); ok {
		upd.ParentID = parentIDPtr
	} else {
		writeInvalidParam(w, "parentId must be a string or null")
		return
	}

	actorID := LocalActorIDFromContext(r.Context())
	f, err := s.drive.UpdateFolder(r.Context(), actorID, folderID, upd)
	if err != nil {
		s.writeDriveFolderError(w, r, "update drive folder", err)
		return
	}
	writeJSON(w, http.StatusOK, projectDriveFolder(f))
}

func (s *Server) handleDriveFoldersDelete(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeJSONBody[driveFoldersDeleteRequest](r)
	if !ok || req.FolderID == "" {
		writeInvalidParam(w, "folderId is required")
		return
	}
	actorID := LocalActorIDFromContext(r.Context())
	if err := s.drive.DeleteFolder(r.Context(), actorID, req.FolderID); err != nil {
		s.writeDriveFolderError(w, r, "delete drive folder", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleFilesShow handles GET /files/{id}: the anonymous, unauthenticated
// byte-serving route DriveFile.url points at (see
// drive.Service.OpenFile's doc comment for why no auth check belongs
// here). A long, immutable Cache-Control is safe because Storage.Put is
// create-only (ADR-0006 D1) — the bytes behind a given id never change.
func (s *Server) handleFilesShow(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	f, rc, err := s.drive.OpenFile(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		s.logger.Error("open drive file failed", "request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
		writeInternalError(w)
		return
	}
	defer rc.Close()

	w.Header().Set("Content-Type", f.MIME)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	// f.MIME is always one of internal/drive.AllowedImageFormats' three
	// canonical values (ValidateImage decoded the bytes, never a
	// client-declared type), never attacker-controlled — but nosniff
	// costs nothing and is the standard defense-in-depth header for any
	// route serving uploaded content, guarding against a browser
	// content-sniffing a polyglot file as something other than the
	// declared image type.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = io.Copy(w, rc)
}
