package drive

import (
	"bytes"
	"errors"
	"fmt"
	"image"

	// Registers the PNG, JPEG, and GIF decoders with image.DecodeConfig
	// below. GIF is registered but never listed in AllowedImageFormats
	// (see its doc comment) — it stays imported so a future decision to
	// allow it needs no new import, and because image/gif is part of the
	// standard library "free" cost AGENTS.md's dependency rule already
	// accepts.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	// x/image/webp decodes but cannot encode; it is here only so
	// DecodeConfig recognizes an uploaded WebP as WebP.
	_ "golang.org/x/image/webp"
)

// ErrInvalidImage reports that an upload claiming to be an image could
// not be decoded as one of AllowedImageFormats. Wraps no domain.Err*
// sentinel: this is a client-input validation failure (Issue #23's
// UNSUPPORTED_FEATURE-shaped explicit rejection, applied here to a
// malformed upload rather than an unimplemented endpoint), not a
// storage or lookup failure.
var ErrInvalidImage = errors.New("drive: not a valid raster image")

// AllowedImageFormats are the only image.DecodeConfig format names this
// package accepts. SVG has no entry here — and never will — because it
// is a vector/XML format image.DecodeConfig cannot decode as any raster
// format, so it is rejected by DecodeImage's ordinary
// fails-to-decode path (ADR-0006 D2) rather than needing a dedicated
// SVG/XML parser to detect and reject it by name.
var AllowedImageFormats = map[string]bool{
	"png":  true,
	"jpeg": true,
	"webp": true,
}

// ImageInfo is what ValidateImage confirms about an upload claiming to
// be an image.
type ImageInfo struct {
	Format string
	Width  int
	Height int
}

// ValidateImage decodes data's image structure to confirm it is really
// one of AllowedImageFormats, ignoring whatever Content-Type or file
// extension a client claimed — AGENTS.md's "treat ... remote API
// responses ... as untrusted data," applied here to an upload's declared
// MIME type. It never decodes full pixel data (image.DecodeConfig reads
// only the header), so this is cheap even for a large file, and it
// naturally rejects SVG: an SVG document is XML, not any of these
// formats' binary header, so it always fails to decode here rather than
// needing a dedicated rejection rule.
//
// maxWidth/maxHeight bound the *claimed* dimensions from the header
// alone — this happens before any full decode, so a pathological
// "1 byte file claiming to be 50000x50000" cannot force a caller into
// allocating a full decode buffer for it.
func ValidateImage(data []byte, maxWidth, maxHeight int) (ImageInfo, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return ImageInfo{}, fmt.Errorf("%w: %v", ErrInvalidImage, err)
	}
	if !AllowedImageFormats[format] {
		return ImageInfo{}, fmt.Errorf("%w: decoded format %q is not allowed", ErrInvalidImage, format)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return ImageInfo{}, fmt.Errorf("%w: non-positive dimensions %dx%d", ErrInvalidImage, cfg.Width, cfg.Height)
	}
	if cfg.Width > maxWidth || cfg.Height > maxHeight {
		return ImageInfo{}, fmt.Errorf("%w: %dx%d exceeds the %dx%d limit", ErrInvalidImage, cfg.Width, cfg.Height, maxWidth, maxHeight)
	}
	return ImageInfo{Format: format, Width: cfg.Width, Height: cfg.Height}, nil
}
