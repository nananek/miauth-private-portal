package rpc

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// MaxFrameBytes bounds one JSON request/response line, on both sides of
// the socket: internal/ingest/imap reading cmd/mailfetch's Response, and
// cmd/mailfetch reading the main process's Request. json.Marshal always
// escapes control characters (including newlines) inside JSON strings, so
// a single marshaled frame never itself contains an unescaped newline
// byte: newline-delimited framing is safe here without a length prefix.
const MaxFrameBytes = 16 << 20 // 16 MiB

// WriteFrame encodes v as one newline-terminated JSON line and writes it
// to w.
func WriteFrame(w io.Writer, v interface{}) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("mailfetch/rpc: encode frame: %w", err)
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
}

// ReadFrame reads one newline-terminated JSON line from r, bounded by
// MaxFrameBytes, and decodes it into v.
func ReadFrame(r io.Reader, v interface{}) error {
	reader := bufio.NewReader(io.LimitReader(r, MaxFrameBytes+1))
	line, readErr := reader.ReadBytes('\n')
	if len(line) > MaxFrameBytes {
		return fmt.Errorf("mailfetch/rpc: frame exceeds %d bytes", MaxFrameBytes)
	}
	// A read error is fatal unless it is exactly "the peer closed the
	// connection right after writing its final newline-less frame" (EOF
	// with data already read): both this package's own WriteFrame always
	// appends '\n', but tolerating its absence costs nothing and avoids a
	// spurious failure on a peer that closes promptly after writing.
	if readErr != nil && !(errors.Is(readErr, io.EOF) && len(line) > 0) {
		return fmt.Errorf("mailfetch/rpc: read frame: %w", readErr)
	}
	if err := json.Unmarshal(line, v); err != nil {
		return fmt.Errorf("mailfetch/rpc: decode frame: %w", err)
	}
	return nil
}
