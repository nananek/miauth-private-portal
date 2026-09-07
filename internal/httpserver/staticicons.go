package httpserver

import (
	"embed"
	"net/http"
)

// staticIconsFS embeds Issue #77 PR2's default favicon/OGP/PWA icon set
// — see internal/httpserver/staticassets/doc.go for what generates
// these files and why they are code-generated placeholders rather than
// hand-designed art. gen/ is deliberately not matched by any pattern
// below: it holds the generator program, not served output.
//
//go:embed staticassets/favicon.ico staticassets/*.png staticassets/site.webmanifest
var staticIconsFS embed.FS

// staticIconRoute is one embedded icon/manifest file served at a fixed,
// well-known HTTP path. Content-Type is set explicitly rather than
// sniffed, since every file here is this package's own known, fixed
// asset, never user-supplied content.
type staticIconRoute struct {
	path        string
	file        string // path within staticIconsFS
	contentType string
}

var staticIconRoutes = []staticIconRoute{
	{"/favicon.ico", "staticassets/favicon.ico", "image/x-icon"},
	{"/favicon-16x16.png", "staticassets/favicon-16x16.png", "image/png"},
	{"/favicon-32x32.png", "staticassets/favicon-32x32.png", "image/png"},
	// The *-dark.png variants have no consumer yet: this repository has
	// no HTML page of its own to put a `<link
	// media="(prefers-color-scheme: dark)">` tag on (AGENTS.md: do not
	// add a custom web UI). They are served at their own well-known
	// paths now so a future page can reference them without a further
	// asset change.
	{"/favicon-16x16-dark.png", "staticassets/favicon-16x16-dark.png", "image/png"},
	{"/favicon-32x32-dark.png", "staticassets/favicon-32x32-dark.png", "image/png"},
	{"/apple-touch-icon.png", "staticassets/apple-touch-icon.png", "image/png"},
	{"/icon-192.png", "staticassets/icon-192.png", "image/png"},
	{"/icon-512.png", "staticassets/icon-512.png", "image/png"},
	{"/og-image.png", "staticassets/og-image.png", "image/png"},
	{"/site.webmanifest", "staticassets/site.webmanifest", "application/manifest+json"},
}

// registerStaticIcons wires every staticIconRoutes entry unconditionally
// — like /healthz, these need no configuration or dependency, and Issue
// #77's acceptance criterion ("新規インストール直後でもfavicon・アプリ
// アイコンが表示される") requires them to work on a fresh install with
// no setup at all. A browser tab pointed at this origin (for example
// during the MiAuth flow's plain-text page, miauth_handlers.go's
// writePlainTextPage) auto-requests /favicon.ico and
// /apple-touch-icon.png at these exact paths with no markup needed.
func (s *Server) registerStaticIcons() {
	for _, route := range staticIconRoutes {
		data, err := staticIconsFS.ReadFile(route.file)
		if err != nil {
			// Every entry above names a file staticIconsFS's go:embed
			// directive matches at compile time (a pattern matching
			// nothing fails the build itself), so a missing file here
			// would mean this slice and the embed directive have
			// silently drifted apart — a programming error to catch at
			// startup, not a runtime condition to degrade from.
			panic("httpserver: embedded static icon " + route.file + " not found: " + err.Error())
		}
		contentType, body := route.contentType, data
		s.Handle("GET "+route.path, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", contentType)
			// These are fixed, code-generated placeholders that only
			// change when an operator deliberately regenerates
			// staticassets and redeploys — a day-long cache still
			// surfaces that same-day, without every request re-serving
			// a file that never changes per-request.
			w.Header().Set("Cache-Control", "public, max-age=86400")
			_, _ = w.Write(body)
		}))
	}
}
