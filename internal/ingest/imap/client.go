package imap

import (
	"context"
	"fmt"
	"net"

	"github.com/nananek/miauth-private-portal/internal/ingest"
	"github.com/nananek/miauth-private-portal/internal/mailfetch/rpc"
)

// call opens one connection to a.cfg.SocketPath, sends req, and reads
// back one rpc.Response, classifying every failure per ADR-0003 and this
// ticket's plan: unreachable cmd/mailfetch (dial or write failure) is
// CategoryTransport, retryable exactly like a transient IMAP server
// outage; a response that cannot be read/decoded at all (including one
// exceeding rpc.MaxFrameBytes) is CategoryMalformed, since both sides of
// this socket are this deployment's own code and a bad frame signals a
// version mismatch or bug, not a condition retrying can fix.
func (a *Adapter) call(ctx context.Context, req rpc.Request) (rpc.Response, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", a.cfg.SocketPath)
	if err != nil {
		return rpc.Response{}, ingest.NewFetchError(ingest.CategoryTransport, fmt.Errorf("dial mailfetch socket: %w", err))
	}
	defer conn.Close()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	if err := rpc.WriteFrame(conn, req); err != nil {
		return rpc.Response{}, ingest.NewFetchError(ingest.CategoryTransport, fmt.Errorf("write mailfetch request: %w", err))
	}

	var resp rpc.Response
	if err := rpc.ReadFrame(conn, &resp); err != nil {
		return rpc.Response{}, ingest.NewFetchError(ingest.CategoryMalformed, fmt.Errorf("read mailfetch response: %w", err))
	}
	return resp, nil
}
