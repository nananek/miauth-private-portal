package drive

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"testing"
)

func encodePNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode PNG: %v", err)
	}
	return buf.Bytes()
}

func encodeJPEG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatalf("encode JPEG: %v", err)
	}
	return buf.Bytes()
}

func TestValidateImage_AcceptsPNG(t *testing.T) {
	data := encodePNG(t, 10, 20)
	info, err := ValidateImage(data, 100, 100)
	if err != nil {
		t.Fatalf("ValidateImage: %v", err)
	}
	if info.Format != "png" || info.Width != 10 || info.Height != 20 {
		t.Errorf("info = %+v, want format=png width=10 height=20", info)
	}
}

func TestValidateImage_AcceptsJPEG(t *testing.T) {
	data := encodeJPEG(t, 8, 8)
	info, err := ValidateImage(data, 100, 100)
	if err != nil {
		t.Fatalf("ValidateImage: %v", err)
	}
	if info.Format != "jpeg" || info.Width != 8 || info.Height != 8 {
		t.Errorf("info = %+v, want format=jpeg width=8 height=8", info)
	}
}

func TestValidateImage_RejectsSVG(t *testing.T) {
	svg := []byte(`<?xml version="1.0" encoding="UTF-8"?><svg xmlns="http://www.w3.org/2000/svg" width="10" height="10"></svg>`)
	_, err := ValidateImage(svg, 1000, 1000)
	if err == nil {
		t.Fatal("expected SVG to be rejected")
	}
	if !errors.Is(err, ErrInvalidImage) {
		t.Errorf("err = %v, want it to satisfy errors.Is(err, ErrInvalidImage)", err)
	}
}

func TestValidateImage_RejectsGarbage(t *testing.T) {
	_, err := ValidateImage([]byte("not an image, just some random bytes padded out"), 1000, 1000)
	if !errors.Is(err, ErrInvalidImage) {
		t.Errorf("err = %v, want it to satisfy errors.Is(err, ErrInvalidImage)", err)
	}
}

func TestValidateImage_RejectsGIFEvenThoughItDecodes(t *testing.T) {
	// image/gif is registered (for future-proofing, see validate.go's
	// import comment) but deliberately absent from AllowedImageFormats:
	// this confirms the allowlist check runs even when the underlying
	// decoder succeeds.
	var buf bytes.Buffer
	img := image.NewPaletted(image.Rect(0, 0, 4, 4), color.Palette{color.White, color.Black})
	if err := gif.Encode(&buf, img, nil); err != nil {
		t.Fatalf("encode GIF: %v", err)
	}
	_, err := ValidateImage(buf.Bytes(), 1000, 1000)
	if !errors.Is(err, ErrInvalidImage) {
		t.Errorf("err = %v, want GIF rejected via errors.Is(err, ErrInvalidImage)", err)
	}
}

func TestValidateImage_RejectsOversizedDimensions(t *testing.T) {
	data := encodePNG(t, 200, 50)
	_, err := ValidateImage(data, 100, 100)
	if !errors.Is(err, ErrInvalidImage) {
		t.Errorf("err = %v, want oversized image rejected via errors.Is(err, ErrInvalidImage)", err)
	}
}

// TestValidateImage_DimensionLimitIsInclusive pins the exact boundary
// comparison (cfg.Width > maxWidth, not >=): an image exactly at the
// configured limit must be accepted, and one pixel past it must not.
func TestValidateImage_DimensionLimitIsInclusive(t *testing.T) {
	atLimit := encodePNG(t, 100, 100)
	if _, err := ValidateImage(atLimit, 100, 100); err != nil {
		t.Errorf("ValidateImage at exactly the limit: %v, want accepted", err)
	}
	oneOver := encodePNG(t, 101, 100)
	if _, err := ValidateImage(oneOver, 100, 100); !errors.Is(err, ErrInvalidImage) {
		t.Errorf("ValidateImage one pixel over the width limit: err = %v, want ErrInvalidImage", err)
	}
}

func TestValidateImage_RejectsEmptyData(t *testing.T) {
	_, err := ValidateImage(nil, 1000, 1000)
	if !errors.Is(err, ErrInvalidImage) {
		t.Errorf("err = %v, want empty data rejected via errors.Is(err, ErrInvalidImage)", err)
	}
}

func TestAllowedImageFormats_IncludesWebP(t *testing.T) {
	// A full lossless/lossy WebP bitstream is impractical to construct
	// by hand in a unit test and this repository has no third-party WebP
	// fixture to depend on (AGENTS.md: check license/attribution before
	// copying third-party material) — golang.org/x/image/webp's own
	// decode correctness is that library's concern, not this package's.
	// This test instead pins the allowlist itself, so a future edit
	// cannot silently drop WebP support without failing a test.
	if !AllowedImageFormats["webp"] {
		t.Error(`AllowedImageFormats["webp"] = false, want true`)
	}
	if AllowedImageFormats["svg"] {
		t.Error(`AllowedImageFormats["svg"] = true, want false — SVG must never be allowed`)
	}
}
