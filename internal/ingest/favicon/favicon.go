// Package favicon implements Issue #77 PR5's per-RSS-source icon fetch
// (plan-77's requirement 2, folded into PR5 alongside actors.
// avatar_file_id — see docs/roadmap/media-drive.md's PR4/PR5 entries),
// extended by Issue #146 into a multi-step best-effort resolution chain:
// a well-known /favicon.ico on the feed's own host (Fetch), the feed's
// own <link>ed site page's <link rel="icon"> (FetchFromPage), and that
// site's own host's /favicon.ico (Resolve ties these together). Every
// step is a GET over the same SSRF-protected client
// (internal/ingest/safehttp) every other outbound fetch in this
// repository uses. This package knows nothing about internal/drive or
// storage — cmd/server is what validates and stores the returned bytes
// (via internal/drive.Service.CreateSystemFile) and sets an actor's
// avatar_file_id.
package favicon

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	// x/image/webp decodes but cannot encode; it is here only so
	// image.DecodeConfig recognizes a WebP candidate as "webp" in
	// decodeCandidate below, the same reason internal/drive/validate.go
	// blank-imports it.
	_ "golang.org/x/image/webp"

	"golang.org/x/net/html"

	"github.com/nananek/miauth-private-portal/internal/ingest/rss"
	"github.com/nananek/miauth-private-portal/internal/ingest/safehttp"
)

// userAgent identifies every request this package makes, the same
// convention internal/ingest/rss.Adapter uses for feed fetches.
const userAgent = "miauth-private-portal-favicon/1"

// maxPageBytes bounds how much of a fetched HTML page is read into
// memory while scanning for <link rel="icon"> candidates, independent
// of (and never larger than) the caller's own maxBytes — a page is text
// scanned for a handful of attributes, not an image, so it never needs
// DRIVE_MAX_FILE_BYTES' full allowance.
const maxPageBytes = 1 << 20

// maxIconCandidates bounds how many <link rel="icon">-family candidates
// FetchFromPage will actually GET, most-preferred first, so a page
// listing many sizes never turns into an unbounded number of outbound
// requests.
const maxIconCandidates = 3

// pngMagic is a PNG file's fixed 8-byte signature.
var pngMagic = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}

// icoMagic is an ICO container's fixed ICONDIR signature (reserved=0,
// type=1, little-endian).
var icoMagic = []byte{0, 0, 1, 0}

// decodableImageFormat names the image.DecodeConfig format strings this
// package accepts from a candidate URL that isn't an ICO container —
// exactly the raster formats internal/drive.ValidateImage itself
// accepts (see AllowedImageFormats), so nothing this package hands back
// can fail that later validation on format grounds alone.
var decodableImageFormat = map[string]bool{"png": true, "jpeg": true, "gif": true, "webp": true}

// Resolve is Issue #146's fallback chain for a single RSS source,
// bounded by maxBytes at every fetch: (1) the feed's own host's
// well-known /favicon.ico (Fetch, unchanged from pre-#146 behavior —
// this is the only step a source whose favicon.ico already works ever
// needs); (2) the feed document itself, read only to find its own
// <link> (internal/ingest/rss.SiteURLFromFeed) to the human-facing site
// its items belong to, then that site page's <link rel="icon"> family
// (FetchFromPage); (3) that site's own host's /favicon.ico, when it
// differs from the feed's host. Every step's failure is non-fatal and
// only advances to the next; Resolve itself returns an error, joining
// every step's own, only once all of them have failed. feedURL is the
// domain.ExternalSource's own URI, the same value
// internal/ingest/rss.HostFromFeedURL already derives feedHost from at
// registration time.
func Resolve(ctx context.Context, client *safehttp.Client, feedURL string, maxBytes int64) ([]byte, error) {
	feedHost, err := rss.HostFromFeedURL(feedURL)
	if err != nil {
		return nil, fmt.Errorf("favicon: resolve: %w", err)
	}

	var errs []error
	if data, ferr := Fetch(ctx, client, feedHost, maxBytes); ferr == nil {
		return data, nil
	} else {
		errs = append(errs, ferr)
	}

	siteURL, serr := fetchSiteURL(ctx, client, feedURL, maxBytes)
	if serr != nil {
		errs = append(errs, serr)
		return nil, fmt.Errorf("favicon: no resolution succeeded: %w", errors.Join(errs...))
	}

	if data, ferr := FetchFromPage(ctx, client, siteURL, maxBytes); ferr == nil {
		return data, nil
	} else {
		errs = append(errs, ferr)
	}

	if siteHost, herr := rss.HostFromFeedURL(siteURL); herr == nil && !strings.EqualFold(siteHost, feedHost) {
		if data, ferr := Fetch(ctx, client, siteHost, maxBytes); ferr == nil {
			return data, nil
		} else {
			errs = append(errs, ferr)
		}
	}

	return nil, fmt.Errorf("favicon: no resolution succeeded: %w", errors.Join(errs...))
}

// fetchSiteURL GETs feedURL itself (bounded by maxBytes, the same
// budget the caller applies to every other step) and extracts its own
// site link via internal/ingest/rss.SiteURLFromFeed.
func fetchSiteURL(ctx context.Context, client *safehttp.Client, feedURL string, maxBytes int64) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, feedURL, nil)
	if err != nil {
		return "", fmt.Errorf("favicon: build feed request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("favicon: fetch feed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("favicon: feed unexpected status %d", resp.StatusCode)
	}
	data, err := safehttp.ReadLimited(resp.Body, maxBytes)
	if err != nil {
		return "", fmt.Errorf("favicon: read feed: %w", err)
	}
	siteURL := rss.SiteURLFromFeed(data)
	if siteURL == "" {
		return "", errors.New("favicon: feed has no usable site URL")
	}
	return siteURL, nil
}

// Fetch fetches host's well-known /favicon.ico over client and decodes
// it (see decodeCandidate), bounded by maxBytes. It always tries https
// only — RSS_ALLOW_INSECURE_HTTP's http exception does not extend to
// favicon fetching, a deliberate simplification: favicon fetch is
// best-effort by design (every error here is meant to be swallowed by
// the caller, not surfaced as a registration failure), so there is no
// operator-visible cost to skipping an insecure fallback.
//
// Every returned error must be treated as non-fatal by the caller: a
// missing, unreachable, oversized, or undecodable favicon is an
// expected, common outcome, not a reason to fail whatever registered
// the source that triggered this fetch.
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
func fetchURL(ctx context.Context, client *safehttp.Client, rawURL string, maxBytes int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("favicon: build request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
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
	return decodeCandidate(data)
}

// decodeCandidate classifies one fetched blob — an ICO container (by
// its magic bytes) or a bare raster image, exactly as a URL named by a
// site's own <link rel="icon"> may point at either shape — and returns
// image bytes internal/drive.ValidateImage can decode directly, or an
// error if data is neither. An ICO is resolved via ExtractImageFromICO;
// anything else is accepted only if image.DecodeConfig recognizes it as
// one of decodableImageFormat (the same format set
// internal/drive.ValidateImage itself allows), returned unchanged
// (already a valid file of its own format, nothing to transcode).
func decodeCandidate(data []byte) ([]byte, error) {
	if bytes.HasPrefix(data, icoMagic) {
		return ExtractImageFromICO(data)
	}
	if _, format, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
		if decodableImageFormat[format] {
			return data, nil
		}
		return nil, fmt.Errorf("favicon: decoded format %q is not accepted", format)
	}
	return nil, errors.New("favicon: response is neither an ICO container nor a recognized raster image")
}

// ExtractImageFromICO parses an ICO container (an ICONDIR header
// followed by one ICONDIRENTRY per embedded image — the same format
// internal/httpserver/staticassets/gen's encodeICO produces, read here
// instead of written) and returns PNG-encoded bytes for its best
// decodable embedded image, trying entries largest-pixel-count first. A
// PNG-formatted entry (the shape nearly every current favicon generator,
// including this application's own, produces) is returned as-is; a
// legacy uncompressed-BMP entry (Issue #146) is decoded via
// decodeBMPEntry and re-encoded as PNG. An entry this package cannot
// decode (unsupported BMP compression/bit depth) is skipped in favor of
// the next-largest entry, not treated as a whole-ICO failure.
//
// data is untrusted input fetched from a third-party server (AGENTS.md:
// "treat ... remote API responses ... as untrusted data") — every
// offset and length the header claims is bounds-checked against data's
// actual length before any slice is taken, so a truncated or
// adversarially crafted ICO returns an error rather than panicking.
func ExtractImageFromICO(data []byte) ([]byte, error) {
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

	type icoEntry struct {
		pixels int
		data   []byte
	}
	var entries []icoEntry
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
		entries = append(entries, icoEntry{pixels: width * height, data: data[imageOffset : imageOffset+bytesInRes]})
	}
	if len(entries) == 0 {
		return nil, errors.New("favicon: ICO has no entries with valid bounds")
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].pixels > entries[j].pixels })

	var lastErr error
	for _, e := range entries {
		if bytes.HasPrefix(e.data, pngMagic) {
			return e.data, nil
		}
		img, err := decodeBMPEntry(e.data)
		if err != nil {
			lastErr = err
			continue
		}
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			lastErr = err
			continue
		}
		return buf.Bytes(), nil
	}
	if lastErr == nil {
		lastErr = errors.New("favicon: no entry could be decoded")
	}
	return nil, fmt.Errorf("favicon: no decodable entry found in ICO: %w", lastErr)
}

// bmpMinHeaderLen is sizeof(BITMAPINFOHEADER), the only DIB header shape
// this package decodes.
const bmpMinHeaderLen = 40

// maxBMPDimension bounds a decoded BMP-in-ICO entry's width/height:
// generous for any real favicon (which top out around 256px) while
// keeping a corrupt or adversarial width/height field
// (int32-attacker-controlled) from driving an unbounded allocation.
const maxBMPDimension = 1024

// bmpHeader is BITMAPINFOHEADER's fields this package needs, already
// validated against data's own length and this package's supported
// shapes (see parseBMPHeader).
type bmpHeader struct {
	headerSize int
	width      int
	height     int
	bitCount   int
	clrUsed    int
}

// parseBMPHeader reads and validates data's leading BITMAPINFOHEADER.
// ICO stores an uncompressed DIB whose declared height is always double
// the actual pixel height (the XOR and AND masks stacked as one image),
// which this function divides back out; anything compressed, or outside
// this package's supported 1/4/8/24/32 bit depths, is rejected rather
// than guessed at.
func parseBMPHeader(data []byte) (bmpHeader, error) {
	if len(data) < bmpMinHeaderLen {
		return bmpHeader{}, errors.New("favicon: BMP entry too short for BITMAPINFOHEADER")
	}
	size := int(binary.LittleEndian.Uint32(data[0:4]))
	if size < bmpMinHeaderLen || size > len(data) {
		return bmpHeader{}, fmt.Errorf("favicon: BMP header size %d invalid", size)
	}
	width := int(int32(binary.LittleEndian.Uint32(data[4:8])))
	rawHeight := int(int32(binary.LittleEndian.Uint32(data[8:12])))
	bitCount := int(binary.LittleEndian.Uint16(data[14:16]))
	compression := binary.LittleEndian.Uint32(data[16:20])
	clrUsed := int(binary.LittleEndian.Uint32(data[32:36]))

	if compression != 0 {
		return bmpHeader{}, fmt.Errorf("favicon: BMP compression %d not supported (only BI_RGB)", compression)
	}
	if width <= 0 || width > maxBMPDimension {
		return bmpHeader{}, fmt.Errorf("favicon: BMP width %d out of range", width)
	}
	if rawHeight <= 0 || rawHeight%2 != 0 {
		return bmpHeader{}, fmt.Errorf("favicon: BMP height %d is not a valid doubled ICO height", rawHeight)
	}
	height := rawHeight / 2
	if height <= 0 || height > maxBMPDimension {
		return bmpHeader{}, fmt.Errorf("favicon: BMP height %d out of range", height)
	}
	switch bitCount {
	case 1, 4, 8, 24, 32:
	default:
		return bmpHeader{}, fmt.Errorf("favicon: BMP bit depth %d not supported", bitCount)
	}
	return bmpHeader{headerSize: size, width: width, height: height, bitCount: bitCount, clrUsed: clrUsed}, nil
}

// readBMPPalette reads h's color table (present only for bitCount <=
// 8), immediately following the BITMAPINFOHEADER at h.headerSize, and
// returns it alongside the byte offset the pixel data starts at.
func readBMPPalette(data []byte, h bmpHeader) ([]color.NRGBA, int, error) {
	numColors := h.clrUsed
	if numColors <= 0 || numColors > 1<<uint(h.bitCount) {
		numColors = 1 << uint(h.bitCount)
	}
	end := h.headerSize + numColors*4
	if end > len(data) {
		return nil, 0, errors.New("favicon: BMP color table exceeds data length")
	}
	palette := make([]color.NRGBA, numColors)
	for i := 0; i < numColors; i++ {
		o := h.headerSize + i*4
		// BGRQuad order: Blue, Green, Red, Reserved.
		palette[i] = color.NRGBA{R: data[o+2], G: data[o+1], B: data[o], A: 255}
	}
	return palette, end, nil
}

// bmpRowBytes is a DIB row's byte width for the given pixel width and
// bit depth, padded up to the next 4-byte boundary (the DIB format's
// fixed row-alignment rule).
func bmpRowBytes(width, bitCount int) int {
	return ((width*bitCount + 31) / 32) * 4
}

// decodeBMPEntry decodes one legacy uncompressed-BMP ICO entry (Issue
// #146) into an in-memory image. Every accepted bit depth (1/4/8bpp
// palette, 24bpp BGR, 32bpp BGRA) is handled; anything else was already
// rejected by parseBMPHeader.
//
// A 32bpp entry's own alpha channel is trusted only if at least one
// pixel has a non-zero alpha byte — many real-world 32bpp favicons set
// alpha to all zero and rely entirely on the trailing 1bpp AND mask
// instead (a legacy authoring quirk this function must not render as
// fully transparent). In every other case (no real alpha channel), the
// AND mask, when present, marks fully transparent pixels; a mask bit of
// 1 means transparent, matching the DIB/ICO convention.
func decodeBMPEntry(data []byte) (image.Image, error) {
	h, err := parseBMPHeader(data)
	if err != nil {
		return nil, err
	}

	pixelOffset := h.headerSize
	var palette []color.NRGBA
	if h.bitCount <= 8 {
		palette, pixelOffset, err = readBMPPalette(data, h)
		if err != nil {
			return nil, err
		}
	}

	xorRowBytes := bmpRowBytes(h.width, h.bitCount)
	xorSize := xorRowBytes * h.height
	if pixelOffset+xorSize > len(data) {
		return nil, errors.New("favicon: BMP pixel data exceeds data length")
	}
	xorData := data[pixelOffset : pixelOffset+xorSize]

	hasRealAlpha := h.bitCount == 32 && bmpHasNonZeroAlpha(xorData, h.width, h.height, xorRowBytes)

	andRowBytes := bmpRowBytes(h.width, 1)
	andSize := andRowBytes * h.height
	andOffset := pixelOffset + xorSize
	var andData []byte
	if !hasRealAlpha && andOffset+andSize <= len(data) {
		andData = data[andOffset : andOffset+andSize]
	}

	img := image.NewNRGBA(image.Rect(0, 0, h.width, h.height))
	for y := 0; y < h.height; y++ {
		srcRow := h.height - 1 - y // DIB rows are stored bottom-up.
		rowStart := srcRow * xorRowBytes
		for x := 0; x < h.width; x++ {
			r, g, b, a := bmpPixelAt(xorData, palette, rowStart, x, h.bitCount, hasRealAlpha)
			if andData != nil && bmpMaskBit(andData, srcRow, x, andRowBytes) {
				a = 0
			}
			img.SetNRGBA(x, y, color.NRGBA{R: r, G: g, B: b, A: a})
		}
	}
	return img, nil
}

// bmpPixelAt reads one pixel from xorData at (row rowStart, column x),
// resolving a palette index (1/4/8bpp) against palette. hasAlpha gates
// whether a 32bpp pixel's own 4th byte is used as alpha (see
// decodeBMPEntry's doc comment); every other shape is fully opaque here
// (the AND mask, if any, is applied by the caller).
func bmpPixelAt(xorData []byte, palette []color.NRGBA, rowStart, x, bitCount int, hasAlpha bool) (r, g, b, a byte) {
	switch bitCount {
	case 32:
		o := rowStart + x*4
		b, g, r = xorData[o], xorData[o+1], xorData[o+2]
		if hasAlpha {
			a = xorData[o+3]
		} else {
			a = 255
		}
	case 24:
		o := rowStart + x*3
		b, g, r, a = xorData[o], xorData[o+1], xorData[o+2], 255
	default: // 1, 4, or 8bpp palette.
		idx := bmpPaletteIndex(xorData, rowStart, x, bitCount)
		a = 255
		if idx < len(palette) {
			c := palette[idx]
			r, g, b = c.R, c.G, c.B
		}
	}
	return r, g, b, a
}

// bmpPaletteIndex reads column x's palette index out of a packed
// 1/4/8bpp row starting at rowStart.
func bmpPaletteIndex(data []byte, rowStart, x, bitCount int) int {
	switch bitCount {
	case 8:
		return int(data[rowStart+x])
	case 4:
		v := data[rowStart+x/2]
		if x%2 == 0 {
			return int(v >> 4)
		}
		return int(v & 0x0F)
	default: // 1bpp
		v := data[rowStart+x/8]
		shift := uint(7 - x%8)
		return int((v >> shift) & 0x01)
	}
}

// bmpMaskBit reports whether the 1bpp AND mask marks (row, x) as
// transparent (bit set), out of a mask row rowBytes wide starting at
// row*rowBytes. An out-of-bounds read (which validated header/size
// fields should make impossible) is treated as "not transparent" rather
// than panicking.
func bmpMaskBit(andData []byte, row, x, rowBytes int) bool {
	o := row*rowBytes + x/8
	if o < 0 || o >= len(andData) {
		return false
	}
	shift := uint(7 - x%8)
	return (andData[o]>>shift)&0x01 == 1
}

// bmpHasNonZeroAlpha reports whether any pixel in a 32bpp XOR bitmap has
// a non-zero alpha byte (see decodeBMPEntry's doc comment).
func bmpHasNonZeroAlpha(xorData []byte, width, height, rowBytes int) bool {
	for y := 0; y < height; y++ {
		row := xorData[y*rowBytes : y*rowBytes+rowBytes]
		for x := 0; x < width; x++ {
			if row[x*4+3] != 0 {
				return true
			}
		}
	}
	return false
}

// FetchFromPage GETs pageURL (bounded to at most maxPageBytes of the
// page itself, regardless of maxBytes, since it is scanned as text —
// see maxPageBytes) and resolves it as HTML looking for a
// <link rel="icon"|"shortcut icon"|"apple-touch-icon"[-precomposed]>
// inside <head>. Candidates are tried most-preferred first ("icon" over
// "apple-touch-icon", larger declared `sizes` over smaller, then
// document order), up to maxIconCandidates GETs, each decoded via
// fetchURL/decodeCandidate (so a candidate may itself be an ICO or a
// bare raster image). The first that decodes successfully is returned.
// A candidate's own scheme is never special-cased here: client's own
// policy (https-only in production, see internal/ingest/safehttp)
// governs every fetchURL call exactly as it does for Fetch, so a
// same-scheme relative or protocol-relative href simply inherits
// pageURL's own already-validated scheme.
func FetchFromPage(ctx context.Context, client *safehttp.Client, pageURL string, maxBytes int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return nil, fmt.Errorf("favicon: build page request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("favicon: fetch page: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("favicon: page unexpected status %d", resp.StatusCode)
	}
	pageLimit := maxBytes
	if pageLimit > maxPageBytes {
		pageLimit = maxPageBytes
	}
	body, err := safehttp.ReadLimited(resp.Body, pageLimit)
	if err != nil {
		return nil, fmt.Errorf("favicon: read page: %w", err)
	}

	base := resp.Request.URL // the final URL after any redirects.
	candidates := parseIconLinks(body, base)
	if len(candidates) == 0 {
		return nil, errors.New("favicon: no <link rel=icon> candidate found")
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].priority != candidates[j].priority {
			return candidates[i].priority < candidates[j].priority
		}
		if candidates[i].sizePx != candidates[j].sizePx {
			return candidates[i].sizePx > candidates[j].sizePx
		}
		return candidates[i].order < candidates[j].order
	})
	if len(candidates) > maxIconCandidates {
		candidates = candidates[:maxIconCandidates]
	}

	var lastErr error
	for _, c := range candidates {
		data, err := fetchURL(ctx, client, c.url.String(), maxBytes)
		if err != nil {
			lastErr = err
			continue
		}
		return data, nil
	}
	if lastErr == nil {
		lastErr = errors.New("favicon: no candidate link resolved")
	}
	return nil, fmt.Errorf("favicon: no candidate link resolved to an image: %w", lastErr)
}

// iconCandidate is one <link> FetchFromPage's parseIconLinks admitted as
// a favicon candidate. priority is lower-is-better (1 = icon/shortcut
// icon, 2 = apple-touch-icon family); order is document order, used
// only to break ties within the same priority and sizePx.
type iconCandidate struct {
	priority int
	sizePx   int
	order    int
	url      *url.URL
}

// parseIconLinks tokenizes body as HTML and collects every <link>
// inside <head> that names an icon-family rel, resolved to an absolute
// URL against base. It stops at </head> or a <body> start tag, whichever
// comes first — real-world HTML sometimes omits a </head> close tag, so
// a <body> start tag is an equally valid stopping signal.
func parseIconLinks(body []byte, base *url.URL) []iconCandidate {
	tz := html.NewTokenizer(bytes.NewReader(body))
	var candidates []iconCandidate
	order := 0
	for {
		switch tz.Next() {
		case html.ErrorToken:
			return candidates
		case html.StartTagToken, html.SelfClosingTagToken:
			tok := tz.Token()
			if tok.Data == "body" {
				return candidates
			}
			if tok.Data != "link" {
				continue
			}
			if c, ok := parseLinkToken(tok, base, order); ok {
				candidates = append(candidates, c)
				order++
			}
		case html.EndTagToken:
			if tz.Token().Data == "head" {
				return candidates
			}
		}
	}
}

// parseLinkToken extracts an iconCandidate from one <link> tag's
// attributes, or reports ok=false when it does not name an icon-family
// rel, has no href, or is an SVG (drive's ValidateImage rejects vector
// formats — see internal/drive/validate.go — so there is no point ever
// fetching one). It does not itself reject a non-https href: FetchFromPage's
// eventual fetchURL call is where the client's own scheme policy applies
// (see FetchFromPage's doc comment).
func parseLinkToken(tok html.Token, base *url.URL, order int) (iconCandidate, bool) {
	var rel, href, sizes, typ string
	for _, a := range tok.Attr {
		switch strings.ToLower(a.Key) {
		case "rel":
			rel = strings.ToLower(a.Val)
		case "href":
			href = a.Val
		case "sizes":
			sizes = a.Val
		case "type":
			typ = strings.ToLower(a.Val)
		}
	}
	if href == "" {
		return iconCandidate{}, false
	}

	priority := 0
	for _, t := range strings.Fields(rel) {
		switch t {
		case "icon":
			priority = 1
		case "apple-touch-icon", "apple-touch-icon-precomposed":
			if priority == 0 {
				priority = 2
			}
		}
	}
	if priority == 0 {
		return iconCandidate{}, false
	}

	if typ == "image/svg+xml" || strings.HasSuffix(strings.ToLower(strings.SplitN(href, "?", 2)[0]), ".svg") {
		return iconCandidate{}, false
	}

	ref, err := url.Parse(href)
	if err != nil {
		return iconCandidate{}, false
	}
	resolved := base.ResolveReference(ref)

	return iconCandidate{priority: priority, sizePx: parseSizesAttr(sizes), order: order, url: resolved}, true
}

// parseSizesAttr returns the largest single dimension named by a
// `sizes` attribute (e.g. "32x32", "16x16 32x32", or "any"), or 0 if it
// names nothing this package can parse as larger-is-better.
func parseSizesAttr(sizes string) int {
	best := 0
	for _, tok := range strings.Fields(sizes) {
		w, h, ok := strings.Cut(strings.ToLower(tok), "x")
		if !ok {
			continue
		}
		wPx, errW := strconv.Atoi(w)
		hPx, errH := strconv.Atoi(h)
		if errW != nil || errH != nil {
			continue
		}
		if wPx > best {
			best = wPx
		}
		if hPx > best {
			best = hPx
		}
	}
	return best
}
