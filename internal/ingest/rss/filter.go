package rss

import (
	"fmt"
	"os"

	"go.starlark.net/starlark"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/ingest"
)

// filterMaxSteps bounds a single matches() call's Starlark execution
// (Issue #135): title/body reaching the script originate from an
// untrusted feed (AGENTS.md: treat external content as untrusted), so a
// pathological or adversarially-triggered slow path in an otherwise
// trusted, owner-authored script must still fail closed on step count
// rather than run unbounded. Generous for any legitimate filter rule.
// go.starlark.net's own default dialect (which LoadFilter uses
// unmodified) already disallows `while` statements and recursive
// function calls, so a script has no way to loop forever on its own —
// this bound exists only to catch a bounded-but-huge loop (for example
// `for i in range(10**18):`) before it runs for an unreasonable amount
// of wall-clock time.
const filterMaxSteps = 10_000_000

// Filter wraps a compiled Starlark "matches(...)" predicate, loaded once
// from RSS_FILTER_SCRIPT_PATH at startup (Issue #135). A nil *Filter
// means no filtering is configured: every item is kept, exactly today's
// pre-#135 behavior.
type Filter struct {
	matchesFn *starlark.Function
}

// LoadFilter parses path's Starlark source, executes its top level
// (which must do nothing but define functions/constants — script-level
// side effects have no builtin to perform anyway, see below), and
// requires a top-level "matches" function accepting exactly five
// parameters: (title, body, source_host, source_uri, provenance_url).
// Any failure — file not found, syntax error, no such function, wrong
// arity — is returned so the caller (cmd/server startup) can fail
// closed before the server ever binds a port, rather than surfacing as
// a confusing failure on the first poll tick.
//
// The script runs in go.starlark.net's default sandbox: no `load()`
// callback is configured (a script containing a load(...) statement
// fails outright, "load not implemented by this application"), and only
// the language's own predeclared builtins (len, range, str, list
// methods, ...) are available — there is no open/exec/os/http builtin
// exposed, so a script can neither read/write the filesystem nor make
// network calls.
func LoadFilter(path string) (*Filter, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("rss: read filter script %q: %w", path, err)
	}
	thread := &starlark.Thread{Name: "rss-filter-load"}
	globals, err := starlark.ExecFile(thread, path, src, nil)
	if err != nil {
		return nil, fmt.Errorf("rss: parse filter script %q: %w", path, err)
	}
	fn, ok := globals["matches"].(*starlark.Function)
	if !ok {
		return nil, fmt.Errorf("rss: filter script %q must define a top-level \"matches\" function", path)
	}
	if fn.NumParams() != 5 {
		return nil, fmt.Errorf("rss: filter script %q's matches() must take exactly (title, body, source_host, source_uri, provenance_url)", path)
	}
	return &Filter{matchesFn: fn}, nil
}

// shouldExclude reports whether item should be dropped (never promoted
// to the timeline). A non-nil error means the script itself failed for
// this item (a step-count bound exceeded, a type error triggered by
// unanticipated feed content, ...) — the caller (Adapter.applyFilter)
// MUST treat that as "keep the item": a filter-evaluation failure must
// never itself make an otherwise-legitimate article disappear, mirroring
// AGENTS.md's "an integration failure must never make a post disappear"
// principle applied to this filter's own failure mode.
func (f *Filter) shouldExclude(source domain.ExternalSource, item ingest.FetchedItem) (bool, error) {
	host, _ := HostFromFeedURL(source.URI) // best-effort; "" on parse failure, script decides what to do with an empty host
	var provenanceURL string
	if item.ProvenanceURL != nil {
		provenanceURL = *item.ProvenanceURL
	}
	kwargs := []starlark.Tuple{
		{starlark.String("title"), starlark.String(item.Title)},
		{starlark.String("body"), starlark.String(item.Body)},
		{starlark.String("source_host"), starlark.String(host)},
		{starlark.String("source_uri"), starlark.String(source.URI)},
		{starlark.String("provenance_url"), starlark.String(provenanceURL)},
	}

	// A fresh Thread per call, deliberately not a Thread stored on
	// Filter and reused: go.starlark.net's Thread.Steps counter (checked
	// against SetMaxExecutionSteps below) accumulates across every Call
	// made on the same Thread rather than resetting per call, and a
	// Thread is not safe for concurrent use by multiple goroutines.
	// internal/jobs.Manager can run several "external_source_poll" jobs
	// concurrently (JOBS_MAX_CONCURRENT), each potentially calling
	// Adapter.Fetch — and therefore shouldExclude — on this same shared
	// *Filter at the same time. A single reused Thread would both race
	// and, after enough cumulative execution across every item ever
	// evaluated, start failing every subsequent item permanently once
	// the running total crossed filterMaxSteps regardless of any single
	// item's actual cost. The compiled matchesFn itself is immutable
	// and safe to call from many Threads concurrently.
	thread := &starlark.Thread{Name: "rss-filter-evaluate"}
	thread.SetMaxExecutionSteps(filterMaxSteps)

	result, err := starlark.Call(thread, f.matchesFn, nil, kwargs)
	if err != nil {
		return false, fmt.Errorf("rss: evaluate filter for item: %w", err)
	}
	b, ok := result.(starlark.Bool)
	if !ok {
		return false, fmt.Errorf("rss: filter script's matches() must return a bool, got %s", result.Type())
	}
	return bool(b), nil
}
