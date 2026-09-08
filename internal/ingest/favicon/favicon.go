// Package favicon implements Issue #77 PR5's per-RSS-source icon fetch
// (plan-77's requirement 2, folded into PR5 alongside actors.
// avatar_file_id — see docs/roadmap/media-drive.md's PR4/PR5 entries):
// a best-effort GET of an ActorExternalSource's own host's well-known
// /favicon.ico, over the same SSRF-protected client
// (internal/ingest/safehttp) every other outbound fetch in this
// repository uses. It knows nothing about internal/drive or storage —
// cmd/server is what validates and stores the returned bytes (via
// internal/drive.Service.CreateSystemFile) and sets an actor's
// avatar_file_id.
package favicon

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"

	"github.com/nananek/miauth-private-portal/internal/ingest/safehttp"
)

// pngMagic is a PNG file's fixed 8-byte signature.
var pngMagic = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}

// Fetch fetches host's well-known /favicon.ico over client and returns
// the largest PNG-formatted image embedded in it (see
// ExtractPNGFromICO), bounded by maxBytes. It always tries https only —
// RSS_ALLOW_INSECURE_HTTP's http exception does not extend to favicon
// fetching, a deliberate simplification: favicon fetch is best-effort by
// design (every error here is meant to be swallowed by the caller, not
// surfaced as a registration failure), so there is no operator-visible
// cost to skipping an insecure fallback.
//
// Every returned error must be treated as non-fatal by the caller: a
// missing, unreachable, oversized, or non-ICO/non-PNG-embedding favicon
// is an expected, common outcome (most sites either have no favicon.ico
// at all, or serve a legacy BMP-in-ICO this package does not decode —
// see ExtractPNGFromICO), not a reason to fail whatever registered the
// source that triggered this fetch.
func Fetch(ctx context.Context, client *safehttp.Client, host string, maxBytes int64) ([]byte, error) {
	return fetchURL(ctx, client, "https://"+host+"/favicon.ico", maxBytes)
}

// fetchURL is Fetch's scheme-agnostic core, split out so tests can drive
// it directly against a plain-http httptest.Server (paired with a
// safehttp.Client configured with AllowInsecureHTTP for the test only —
// the same pattern internal/drive's own tests use), without needing a
// TLS certificate the client would trust. Fetch itself always calls this
// with an https:// URL; nothing in this package ever builds an http://
// one outside of tests.
func fetchURL(ctx context.Context, client *safehttp.Client, url string, maxBytes int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("favicon: build request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("favicon: fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("favicon: unexpected status %d", resp.StatusCode)
	}
	data, err := safehttp.ReadLimited(resp.Body, maxBytes)
	if err != nil {
		return nil, fmt.Errorf("favicon: read response: %w", err)
	}
	return ExtractPNGFromICO(data)
}

// ExtractPNGFromICO parses an ICO container (an ICONDIR header followed
// by one ICONDIRENTRY per embedded image — the same format
// internal/httpserver/staticassets/gen's encodeICO produces, read here
// instead of written) and returns the bytes of its largest PNG-
// formatted embedded image. A legacy ICO whose entries are all
// uncompressed BMP data (the pre-Vista ICO shape, no PNG entries) is
// reported as an error: this function supports only the modern
// "PNG-in-ICO" shape nearly every current favicon generator (including
// this application's own, see encodeICO) produces.
//
// data is untrusted input fetched from a third-party server (AGENTS.md:
// "treat ... remote API responses ... as untrusted data") — every
// offset and length the header claims is bounds-checked against data's
// actual length before any slice is taken, so a truncated or
// adversarially crafted ICO returns an error rather than panicking.
func ExtractPNGFromICO(data []byte) ([]byte, error) {
	if len(data) < 6 {
		return nil, errors.New("favicon: ICO data too short for ICONDIR")
	}
	reserved := binary.LittleEndian.Uint16(data[0:2])
	iconType := binary.LittleEndian.Uint16(data[2:4])
	count := int(binary.LittleEndian.Uint16(data[4:6]))
	if reserved != 0 || iconType != 1 {
		return nil, fmt.Errorf("favicon: not an ICO icon container (reserved=%d type=%d)", reserved, iconType)
	}
	if count == 0 {
		return nil, errors.New("favicon: ICO has no entries")
	}
	headerEnd := 6 + 16*count
	if headerEnd > len(data) {
		return nil, errors.New("favicon: ICO directory entries exceed data length")
	}

	var best []byte
	bestPixels := -1
	for i := range count {
		entry := data[6+16*i : 6+16*i+16]
		// A zero width/height byte means 256 (ICO's own convention for
		// "too large to fit in one byte").
		width, height := int(entry[0]), int(entry[1])
		if width == 0 {
			width = 256
		}
		if height == 0 {
			height = 256
		}
		bytesInRes := int(binary.LittleEndian.Uint32(entry[8:12]))
		imageOffset := int(binary.LittleEndian.Uint32(entry[12:16]))
		if bytesInRes <= 0 || imageOffset < 0 || imageOffset > len(data) || bytesInRes > len(data)-imageOffset {
			continue // malformed entry: skip it rather than fail the whole ICO
		}
		imgData := data[imageOffset : imageOffset+bytesInRes]
		if !bytes.HasPrefix(imgData, pngMagic) {
			continue // legacy BMP-in-ICO entry; not supported, see doc comment
		}
		if pixels := width * height; pixels > bestPixels {
			best, bestPixels = imgData, pixels
		}
	}
	if best == nil {
		return nil, errors.New("favicon: no PNG-formatted entry found in ICO (legacy BMP-in-ICO is not supported)")
	}
	return best, nil
}
