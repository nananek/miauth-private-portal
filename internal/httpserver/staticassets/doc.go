// Package staticassets holds Issue #77 PR2's default favicon/OGP/PWA
// icon set: the "デフォルトのアプリアイコン、favicon、OGP／PWA用アイコン"
// and "未設定時はブランドに合う deterministic なデフォルト画像" the issue's
// requirement 1 asks for. This repository has no real logo or brand
// asset to embed, so every image here is a code-generated "monogram"
// placeholder (a solid-color square with a white letter) rather than
// hand-drawn art — see gen/main.go's doc comment for the exact design
// and why it is deliberately meant to be replaced later, not treated as
// final branding.
//
// The files in this directory (everything except doc.go and gen/) are
// generated output, not hand-edited: regenerate them with
//
//	go generate ./internal/httpserver/staticassets/...
//
// after changing gen/main.go. internal/httpserver embeds them directly
// (staticicons.go) and serves each at its conventional well-known path
// (for example GET /favicon.ico, GET /apple-touch-icon.png) so a
// browser tab pointed at this origin — for instance during the MiAuth
// flow's plain-text page (miauth_handlers.go's writePlainTextPage) —
// shows a real icon instead of a blank default, satisfying Issue #77's
// "新規インストール直後でもfavicon・アプリアイコンが表示される" acceptance
// criterion. This repository intentionally has no HTML page of its own
// (AGENTS.md: do not add a custom web UI), so there is nowhere to place
// a `<link rel="icon" media="(prefers-color-scheme: dark)">` tag yet;
// the *-dark.png variants are generated and served at their own paths
// now so a future page can reference them, but no such wiring exists in
// this PR.
package staticassets

//go:generate go run ./gen
