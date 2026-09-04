package httpserver

import (
	"encoding/json"
	"net/http"
)

// wireError is docs/compat/aria-v1.5.11.md's documented error envelope:
// the pinned misskey_dart ApiService decodes a non-2xx response this
// shape. id/code/message are required strings; kind and info are
// optional. The exact status/code mapping is an implementation contract
// this issue fixes (see docs/compat/aria-v1.5.11.md's Issue #7 notes),
// not something verified against a real Misskey instance yet.
type wireError struct {
	ID      string         `json:"id"`
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Kind    string         `json:"kind,omitempty"`
	Info    map[string]any `json:"info,omitempty"`
}

type wireErrorResponse struct {
	Error wireError `json:"error"`
}

// writeWireError writes status and the Misskey-compatible error envelope.
// It never logs body/message content beyond what the caller already
// decided is safe to put in a client-facing error (AGENTS.md: never log
// message bodies or user content).
func writeWireError(w http.ResponseWriter, status int, id, code, message, kind string, info map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(wireErrorResponse{
		Error: wireError{ID: id, Code: code, Message: message, Kind: kind, Info: info},
	})
}

func writeInvalidParam(w http.ResponseWriter, message string) {
	writeWireError(w, http.StatusBadRequest, "invalid-param", "INVALID_PARAM", message, "client", nil)
}

// writeNoSuchNote is the uniform response for a note ID that does not
// exist, is archived, or is hidden — deliberately not distinguished, the
// same generic-denial treatment used for authentication failures,
// so a caller cannot use this endpoint to probe which case applies.
func writeNoSuchNote(w http.ResponseWriter) {
	writeWireError(w, http.StatusBadRequest, "no-such-note", "NO_SUCH_NOTE", "No such note.", "client", nil)
}

func writeUnsupportedFeature(w http.ResponseWriter, field string) {
	writeWireError(w, http.StatusBadRequest, "unsupported-feature", "UNSUPPORTED_FEATURE",
		"This field is not supported by this service.", "client", map[string]any{"field": field})
}

func writeInternalError(w http.ResponseWriter) {
	writeWireError(w, http.StatusInternalServerError, "internal-error", "INTERNAL_ERROR", "internal error", "server", nil)
}
