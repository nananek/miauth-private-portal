// Command gen deterministically renders the parent staticassets
// package's default favicon/OGP/PWA icon set (Issue #77 PR2) and writes
// the result into the current directory. `go generate` sets the working
// directory to wherever the invoking //go:generate directive lives
// (../doc.go, i.e. internal/httpserver/staticassets/), so every output
// path below is relative to that directory, not to this gen/ package;
// running `go run .` directly from within gen/ would write files into
// gen/ itself, which is not the intended use. This command never runs
// as part of the server binary — only its committed output (embedded
// via internal/httpserver/staticicons.go's //go:embed) does.
//
// Design: a plain "monogram" placeholder — a solid background square
// with a single white letter ("M", this service's name,
// miauth-private-portal) rendered in Go's own bundled Regular typeface
// (golang.org/x/image/font/gofont/goregular). golang.org/x/image is
// already a dependency (Issue #77 PR1 added it for WebP decoding), so
// this needs no new one. This repository has no real logo or brand
// asset — Issue #77 requirement 1 asks only that an unset icon fall
// back to something "ブランドに合う deterministic" — so the icon is
// generated from code rather than hand-drawn: "deterministic" is then a
// literally checkable property (this program takes no network or
// filesystem input, uses no randomness or timestamp, and always
// produces byte-identical output), and it is easy to regenerate if the
// color or letter ever changes. Treat this as a placeholder to be
// replaced by real design work later, not as final branding.
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// letter is the single character every icon variant renders.
const letter = "M"

// lightBG and darkBG are this placeholder's only two background
// colors — one accent hue, a lighter and a darker shade, so the dark
// variant reads as "the same mark," not an unrelated palette. white is
// the letter's only color in both variants: darkBG is dark enough that
// white stays legible on it.
var (
	lightBG = color.RGBA{R: 0x4f, G: 0x46, B: 0xe5, A: 0xff} // indigo-600
	darkBG  = color.RGBA{R: 0x31, G: 0x2e, B: 0x81, A: 0xff} // indigo-900
	white   = color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}
)

// glyphScale sets the letter's font size relative to the icon's shorter
// side, chosen to leave a visible margin around the glyph from a 16px
// favicon up through the 512px PWA icon and the 630px-tall OGP image.
const glyphScale = 0.62

type squareSpec struct {
	name string // output filename, written into the current directory
	size int    // square canvas: size x size pixels
	bg   color.RGBA
}

// squares is every fixed-size square icon this command renders. The
// *-dark.png entries have no consumer yet (this repository has no HTML
// page to put a `<link media="(prefers-color-scheme: dark)">` tag on —
// see ../doc.go); they are generated and served at their own path now
// so a future page can reference them without a further asset change.
var squares = []squareSpec{
	{"favicon-16x16.png", 16, lightBG},
	{"favicon-32x32.png", 32, lightBG},
	{"favicon-16x16-dark.png", 16, darkBG},
	{"favicon-32x32-dark.png", 32, darkBG},
	{"apple-touch-icon.png", 180, lightBG},
	{"icon-192.png", 192, lightBG},
	{"icon-512.png", 512, lightBG},
}

// ogWidth and ogHeight are Open Graph's recommended share-image size
// (1.91:1, the ratio most social/chat link previews crop to).
const (
	ogWidth  = 1200
	ogHeight = 630
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
}

func run() error {
	fnt, err := opentype.Parse(goregular.TTF)
	if err != nil {
		return fmt.Errorf("parse goregular font: %w", err)
	}

	rendered := make(map[string][]byte, len(squares))
	for _, sp := range squares {
		data, err := renderPNG(fnt, sp.size, sp.size, sp.bg)
		if err != nil {
			return fmt.Errorf("render %s: %w", sp.name, err)
		}
		if err := os.WriteFile(sp.name, data, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", sp.name, err)
		}
		rendered[sp.name] = data
	}

	og, err := renderPNG(fnt, ogWidth, ogHeight, lightBG)
	if err != nil {
		return fmt.Errorf("render og-image.png: %w", err)
	}
	if err := os.WriteFile("og-image.png", og, 0o644); err != nil {
		return fmt.Errorf("write og-image.png: %w", err)
	}

	ico, err := encodeICO(rendered["favicon-16x16.png"], rendered["favicon-32x32.png"])
	if err != nil {
		return fmt.Errorf("encode favicon.ico: %w", err)
	}
	if err := os.WriteFile("favicon.ico", ico, 0o644); err != nil {
		return fmt.Errorf("write favicon.ico: %w", err)
	}

	if err := os.WriteFile("site.webmanifest", manifestJSON(), 0o644); err != nil {
		return fmt.Errorf("write site.webmanifest: %w", err)
	}
	return nil
}

// renderPNG draws a w x h canvas filled solid with bg — no rounded
// corners: apple-touch-icon and PWA icons are rounded by the OS/browser
// chrome that displays them, and rounding is imperceptible at
// favicon.ico's 16-32px sizes, so baking corner rounding into the raster
// itself would only add complexity for no visible benefit — then the
// single monogram letter centered in white, sized off the canvas's
// shorter side, and PNG-encodes the result.
func renderPNG(fnt *opentype.Font, w, h int, bg color.RGBA) ([]byte, error) {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), image.NewUniform(bg), image.Point{}, draw.Src)

	basis := h
	if w < basis {
		basis = w
	}
	face, err := opentype.NewFace(fnt, &opentype.FaceOptions{
		Size:    float64(basis) * glyphScale,
		DPI:     72, // Size in points == size in pixels at 72 DPI.
		Hinting: font.HintingFull,
	})
	if err != nil {
		return nil, fmt.Errorf("build font face: %w", err)
	}
	defer face.Close()

	metrics := face.Metrics()
	advance := font.MeasureString(face, letter)
	dot := fixed.Point26_6{
		X: fixed.I(w)/2 - advance/2,
		Y: fixed.I(h)/2 + (metrics.Ascent-metrics.Descent)/2,
	}
	d := &font.Drawer{
		Dst:  img,
		Src:  image.NewUniform(white),
		Face: face,
		Dot:  dot,
	}
	d.DrawString(letter)

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// icoIconDir and icoIconDirEntry mirror the ICO file format's ICONDIR
// and ICONDIRENTRY records exactly (field order, name, and byte width):
// see encodeICO's doc comment.
type icoIconDir struct {
	Reserved, Type, Count uint16
}

type icoIconDirEntry struct {
	Width, Height           uint8
	ColorCount, Reserved    uint8
	Planes, BitCount        uint16
	BytesInRes, ImageOffset uint32
}

// encodeICO packs the given 16px and 32px PNG-encoded images into a
// single favicon.ico using the "PNG-in-ICO" container (an ICONDIR
// header plus one ICONDIRENTRY per image, each pointing at an
// already-PNG-compressed data block rather than a legacy uncompressed
// BMP one) — the format every current browser and OS accepts, and has
// since Windows Vista, so there is no reason to implement the older
// BMP-based encoding it superseded.
func encodeICO(png16, png32 []byte) ([]byte, error) {
	type image struct {
		widthPx int // fits in ICONDIRENTRY's one-byte Width/Height (0 would mean 256, unused here)
		data    []byte
	}
	images := []image{{16, png16}, {32, png32}}

	var buf bytes.Buffer
	if err := binary.Write(&buf, binary.LittleEndian, icoIconDir{
		Reserved: 0,
		Type:     1, // 1 = icon (.ico), 2 would be cursor (.cur)
		Count:    uint16(len(images)),
	}); err != nil {
		return nil, err
	}

	offset := uint32(6 + 16*len(images)) // ICONDIR + one ICONDIRENTRY per image
	for _, im := range images {
		if err := binary.Write(&buf, binary.LittleEndian, icoIconDirEntry{
			Width: uint8(im.widthPx), Height: uint8(im.widthPx),
			ColorCount: 0, Reserved: 0,
			Planes: 1, BitCount: 32, // BitCount is advisory for a PNG-in-ICO entry; the PNG's own header is authoritative.
			BytesInRes:  uint32(len(im.data)),
			ImageOffset: offset,
		}); err != nil {
			return nil, err
		}
		offset += uint32(len(im.data))
	}
	for _, im := range images {
		buf.Write(im.data)
	}
	return buf.Bytes(), nil
}

func manifestJSON() []byte {
	return []byte(`{
  "name": "miauth-private-portal",
  "short_name": "Portal",
  "icons": [
    { "src": "/icon-192.png", "sizes": "192x192", "type": "image/png" },
    { "src": "/icon-512.png", "sizes": "512x512", "type": "image/png" }
  ],
  "theme_color": "#4f46e5",
  "background_color": "#4f46e5",
  "display": "standalone"
}
`)
}
