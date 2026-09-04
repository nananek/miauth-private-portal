package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/nananek/miauth-private-portal/internal/miauth"
)

type contextKey int

const localActorIDKey contextKey = iota

// RequireScope returns middleware that authenticates a Misskey-style
// request by local API token and enforces an exact required scope.
//
// The token arrives as the JSON request body's "i" field, matching
// Aria's convention (docs/compat/aria-v1.5.11.md: "Aria does not use an
// Authorization header for this client path"), not an Authorization
// header. The body is fully read to extract it and then re-wrapped so
// the downstream handler can still decode its own fields; it relies on
// the body already being bounded by the outer withMaxBody middleware
// rather than imposing a second limit here. On success it stores the
// verified local actor ID in the request context
// (LocalActorIDFromContext). On failure it writes a generic
// authentication-failed response without revealing why (unknown token,
// revoked, or wrong scope are all indistinguishable to the caller).
//
// Issue #5 defines this middleware but wires it to no route: the MiAuth
// endpoints this issue adds are all pre-authentication by definition,
// and no protected notes endpoint exists yet. Issue #7 is expected to
// apply it to the endpoints it introduces.
func RequireScope(svc *miauth.Service, scope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				writeAuthenticationFailed(w)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))

			var payload struct {
				I string `json:"i"`
			}
			if err := json.Unmarshal(body, &payload); err != nil || payload.I == "" {
				writeAuthenticationFailed(w)
				return
			}

			actorID, err := svc.VerifyToken(r.Context(), payload.I, scope)
			if err != nil {
				writeAuthenticationFailed(w)
				return
			}

			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), localActorIDKey, actorID)))
		})
	}
}

// LocalActorIDFromContext returns the local actor ID RequireScope
// verified for this request, or "" if none is set.
func LocalActorIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(localActorIDKey).(string)
	return id
}

func writeAuthenticationFailed(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":{"id":"authentication-failed","code":"AUTHENTICATION_FAILED","message":"authentication failed","kind":"client"}}`))
}
