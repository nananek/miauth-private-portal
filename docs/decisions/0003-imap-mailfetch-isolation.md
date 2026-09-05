# ADR-0003: Isolate untrusted IMAP/MIME parsing in a separate mailfetch process

- Status: Accepted for Issue #12
- Date: 2026-09-05
- Scope: Issue #12 (read-only IMAP ingestion)

## Context

Issue #12 extends Issue #11's `internal/ingest` framework with an IMAP
adapter. Unlike the RSS/Atom adapter (`internal/ingest/rss`), which parses
XML with the standard library's `encoding/xml`, an IMAP adapter must parse
`ENVELOPE`/`BODYSTRUCTURE` responses and MIME bodies (quoted-printable and
base64 transfer encodings, RFC 2047 encoded headers, arbitrary declared
charsets, nested multipart structures) from a remote server this service
does not control. That parsing surface is substantially larger and more
attacker-influenced than RSS/Atom's, and AGENTS.md already requires
treating mail as untrusted data.

The user's explicit condition for this issue: adding an external IMAP/MIME
library (for example `github.com/emersion/go-imap`) is acceptable, but the
code that fetches from and parses responses from an untrusted mail server
must run in a separate execution unit from the main service process, not
merely behind a Go-level interface in the same process and address space.

Two isolation shapes were considered:

1. **A resident sidecar process/container** (`cmd/mailfetch`) that owns the
   IMAP connection and MIME parsing entirely, reached over IPC.
2. **A per-fetch subprocess** exec'd from inside the main process, spoken to
   over stdin/stdout.

A third option — the main process shelling out to `docker run` to launch an
ephemeral container per fetch — was rejected outright: this repository's
existing `Dockerfile` builds a `gcr.io/distroless/static-debian12:nonroot`
runtime image with no shell and no Docker client, so that approach would
require reintroducing a shell/Docker client into the runtime image (reversing
the distroless decision implicit in the existing `Dockerfile`), and it would
require handing the main container a Docker socket, which is a well-known
privilege-escalation vector (a container with access to the host's Docker
socket has an effective path to root on the host).

Between the two remaining options, a per-fetch subprocess is cheaper to wire
(no new Dockerfile, no compose file, no new IPC listener) but, once deployed
in a container, still shares the main container's filesystem, network, and
PID namespaces — none of the sandboxing a separate container would add
survives past "different OS process." A resident sidecar can instead run in
its own container with its own restricted capabilities, at the cost of a new
image, a new compose service, and a small IPC protocol.

## Decision

Isolate all IMAP protocol handling and MIME/HTML parsing in a new command,
`cmd/mailfetch`, run as a long-lived sidecar process (its own container in a
container deployment; a second systemd unit on a bare-host deployment).

- The main process (`cmd/server`) and `cmd/mailfetch` communicate over a
  Unix domain socket, one newline-delimited JSON request/response pair per
  IMAP fetch. A Unix socket is chosen over a TCP port so the channel is
  never network-reachable and access is controlled by filesystem
  permissions on the socket path (or, in a container deployment, by which
  containers share the socket's volume).
- `internal/ingest/imap` (the main process's side) is a thin RPC client: it
  knows the request/response JSON shape and nothing about IMAP or MIME. It
  never imports an IMAP or MIME library.
- `cmd/mailfetch` (and its supporting `internal/mailfetch` package) owns the
  IMAP4rev1 connection (`EXAMINE`, never `SELECT`; `UID FETCH` with
  `BODY.PEEK[...]`, never `BODY[...]`; no `STORE`/`COPY`/`MOVE`/`EXPUNGE`/
  `APPEND`), MIME structure walking, charset decoding, and HTML-to-plain-text
  sanitization. `github.com/emersion/go-imap` and `github.com/emersion/
  go-message` are imported only here, so Go's static linking guarantees they
  never appear in the `cmd/server` binary.
- Credentials (IMAP username/password) are passed to `cmd/mailfetch` only in
  the per-request JSON payload over the Unix socket, never as command-line
  arguments (visible via `ps`/`/proc`) or in an environment variable readable
  by another process on the host.
- `cmd/mailfetch` unreachable (not started, socket missing, connection
  refused) is a transport-level failure on the client side
  (`ingest.CategoryTransport`, retryable) and is handled by the existing job
  retry/backoff machinery exactly like a transient IMAP server outage. It
  never affects `notes/create`, another ingestion source, or the durable job
  worker.
- Deployment: a new `Dockerfile.mailfetch` (same distroless-nonroot pattern
  as the existing `Dockerfile`) and the repository's first `docker-compose.yml`
  (a `server` service and a `mailfetch` service sharing a volume that holds
  the socket directory; `mailfetch` publishes no port, runs `read_only: true`,
  and drops all capabilities). A bare-host deployment runs
  `bin/mailfetch` (added to `make build`) as its own process/systemd unit
  next to `bin/server`. `IMAP_ENABLED=false` (the default) means
  `cmd/mailfetch` need not run at all.

### Rejected alternative: per-fetch subprocess

`internal/ingest/imap.Adapter.Fetch` execs `/mailfetch` per call and talks
over its stdin/stdout, with the request/response JSON shape unchanged. This
achieves the same "different OS process, different address space, IMAP/MIME
libraries absent from the main binary" properties as the sidecar, at lower
implementation cost (no new Dockerfile, no compose file). It was not chosen
because, in a container deployment, the exec'd subprocess still runs inside
the main container's own filesystem, network, and PID namespaces: there is
no independent `cap_drop`, `read_only`, or network-egress boundary to place
around it. Given the user's stated goal was container-level separation, this
option's isolation strength was judged insufficient for the cost saved. It
remains a reasonable fallback if the sidecar's operational cost (a second
container to run, monitor, and keep in sync with the main image) proves not
worth it in practice.

## Consequences

- New operational surface: `cmd/mailfetch` must be running for IMAP
  ingestion to make progress. Its absence degrades to retried, logged
  fetch failures, never to a crash or a blocked user-facing request.
- New dependencies `github.com/emersion/go-imap` and
  `github.com/emersion/go-message` (plus their transitive charset-handling
  dependencies) are added, scoped entirely to `cmd/mailfetch`/
  `internal/mailfetch`.
- The repository gains its first `docker-compose.yml` and second Dockerfile;
  `README.md`/`docs/operations/configuration.md` document both the compose
  and bare-host ways to run `mailfetch` alongside `server`.
- A new IPC protocol (newline-delimited JSON over a Unix socket) is
  introduced and versioned informally by its own request/response struct
  shapes; it is internal to this deployment and never exposed externally.
