package httpserver

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/logging"
	"github.com/nananek/miauth-private-portal/internal/webadmin"
)

// adminSessionContextKey mirrors scope_middleware.go's contextKey/
// localActorIDKey pattern exactly, as its own distinct key type/const —
// never sharing a context key between the two authentication surfaces
// (ADR-0010 Decision 1's "structurally distinct" principle extended to
// request-context plumbing, not just storage).
type adminSessionContextKey int

const webAdminSessionKey adminSessionContextKey = iota

const adminSessionCookieName = "admin_session"

// RequireAdminSession authenticates a browser via the admin_session
// cookie (never Aria's local API token, never accepted anywhere
// RequireScope is — see server.go's Path=/admin cookie scoping). On any
// failure — missing cookie, unknown/expired/revoked session — it writes
// one generic 401 (plan-136-phase2 §1 Decision 4; mirrors RequireScope's
// "indistinguishable failure reasons" precedent). On success it stores
// the verified domain.WebAdminSession in the request context.
func RequireAdminSession(logger *slog.Logger, svc *webadmin.Service) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cookie, err := r.Cookie(adminSessionCookieName)
			if err != nil || cookie.Value == "" {
				writeWebAdminError(w, http.StatusUnauthorized, "authentication required")
				return
			}
			session, err := svc.VerifySession(r.Context(), cookie.Value)
			if err != nil {
				if errors.Is(err, webadmin.ErrSessionInvalid) {
					writeWebAdminError(w, http.StatusUnauthorized, "authentication required")
					return
				}
				logger.Error("admin session verification failed",
					"request_id", logging.RequestIDFromContext(r.Context()), "error", err.Error())
				writeWebAdminError(w, http.StatusInternalServerError, "internal error")
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), webAdminSessionKey, session)))
		})
	}
}

// AdminSessionFromContext returns the domain.WebAdminSession
// RequireAdminSession verified for this request, or the zero value if
// none is set.
func AdminSessionFromContext(ctx context.Context) domain.WebAdminSession {
	s, _ := ctx.Value(webAdminSessionKey).(domain.WebAdminSession)
	return s
}

// RequireAdminCSRF must run *after* RequireAdminSession in the chain
// (it reads the verified session from context) and guards every
// state-changing /admin/* route: the request's X-Admin-CSRF-Token
// header must equal the session's own CSRFToken exactly
// (plan-136-phase2 §1 Decision 5 — defense-in-depth alongside
// SameSite=Strict, ADR-0010 Decision 7). A constant-time compare is not
// strictly necessary here the way it is for a bearer secret —
// SameSite=Strict already means only same-site script can ever read the
// header value to forge, and a timing side-channel on this particular
// comparison leaks the CSRF token itself, not the session credential —
// but crypto/subtle.ConstantTimeCompare is used anyway, matching this
// codebase's "compare secrets safely" convention without needing to
// litigate whether this specific value strictly qualifies.
func RequireAdminCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session := AdminSessionFromContext(r.Context())
		got := r.Header.Get("X-Admin-CSRF-Token")
		if session.CSRFToken == nil || got == "" ||
			subtle.ConstantTimeCompare([]byte(got), []byte(*session.CSRFToken)) != 1 {
			writeWebAdminError(w, http.StatusForbidden, "missing or invalid CSRF token")
			return
		}
		next.ServeHTTP(w, r)
	})
}
