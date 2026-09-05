// Package mailfetch implements Issue #12's untrusted IMAP protocol
// handling and MIME parsing: everything ADR-0003
// (docs/decisions/0003-imap-mailfetch-isolation.md) requires to run
// outside cmd/server's process. Only cmd/mailfetch imports this package,
// so github.com/emersion/go-imap and github.com/emersion/go-message never
// appear in cmd/server's build graph.
//
// Fetch is the package's single entry point: given an
// internal/mailfetch/rpc.Request, it connects read-only (EXAMINE, never
// SELECT; BODY.PEEK, never BODY; no STORE/COPY/MOVE/EXPUNGE/APPEND —
// AGENTS.md: "IMAP is read-only by default and must not mark, move, or
// delete mail"), fetches new messages since the request's cursor, and
// returns a normalized internal/mailfetch/rpc.Response.
package mailfetch
