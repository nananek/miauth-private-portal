package favicon

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/nananek/miauth-private-portal/internal/ingest/safehttp"
)

func encodeTestPNG(t *testing.T, size int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			img.Set(x, y, color.RGBA{R: 10, G: 20, B: 30, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode PNG: %v", err)
	}
	return buf.Bytes()
}

// buildTestICO assembles a minimal ICO container from the given
// (widthPx, data) entries, mirroring internal/httpserver/staticassets/
// gen's own encodeICO — duplicated here rather than imported (that
// function lives in an unrelated package main, not a reusable library).
// data may be a PNG-formatted image or a legacy BMP entry (see
// buildTestBMPEntry); ExtractImageFromICO tells them apart by content,
// not by this helper's own (unused-by-decode) BitCount field.
func buildTestICO(t *testing.T, entries ...struct {
	widthPx int
	data    []byte
}) []byte {
	t.Helper()
	var buf bytes.Buffer
	dir := struct{ Reserved, Type, Count uint16 }{0, 1, uint16(len(entries))}
	if err := binary.Write(&buf, binary.LittleEndian, dir); err != nil {
		t.Fatalf("write ICONDIR: %v", err)
	}
	offset := uint32(6 + 16*len(entries))
	for _, e := range entries {
		entry := struct {
			Width, Height           uint8
			ColorCount, Reserved    uint8
			Planes, BitCount        uint16
			BytesInRes, ImageOffset uint32
		}{
			Width: uint8(e.widthPx), Height: uint8(e.widthPx),
			Planes: 1, BitCount: 32,
			BytesInRes:  uint32(len(e.data)),
			ImageOffset: offset,
		}
		if err := binary.Write(&buf, binary.LittleEndian, entry); err != nil {
			t.Fatalf("write ICONDIRENTRY: %v", err)
		}
		offset += uint32(len(e.data))
	}
	for _, e := range entries {
		buf.Write(e.data)
	}
	return buf.Bytes()
}

func icoEntry(widthPx int, data []byte) struct {
	widthPx int
	data    []byte
} {
	return struct {
		widthPx int
		data    []byte
	}{widthPx, data}
}

// buildTestBMPEntry assembles one legacy uncompressed-BMP ICO entry
// (BITMAPINFOHEADER + bottom-up XOR data + optional 1bpp AND mask) for
// exercising decodeBMPEntry's 24/32bpp paths. pixels[0] is the image's
// top row; this function reverses it into the file's bottom-up storage
// order. withMask, when true, appends an AND mask marking each pixel
// transparent (mask bit 1) wherever its alpha is 0.
func buildTestBMPEntry(t *testing.T, width, height, bitCount int, pixels [][]color.NRGBA, withMask bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	header := struct {
		Size                  uint32
		Width, Height         int32
		Planes, BitCount      uint16
		Compression, SizeImg  uint32
		XPPM, YPPM            int32
		ClrUsed, ClrImportant uint32
	}{
		Size: 40, Width: int32(width), Height: int32(height * 2),
		Planes: 1, BitCount: uint16(bitCount),
	}
	if err := binary.Write(&buf, binary.LittleEndian, header); err != nil {
		t.Fatalf("write BITMAPINFOHEADER: %v", err)
	}

	rowBytes := ((width*bitCount + 31) / 32) * 4
	for y := height - 1; y >= 0; y-- { // bottom-up
		row := make([]byte, rowBytes)
		for x := 0; x < width; x++ {
			c := pixels[y][x]
			switch bitCount {
			case 32:
				row[x*4], row[x*4+1], row[x*4+2], row[x*4+3] = c.B, c.G, c.R, c.A
			case 24:
				row[x*3], row[x*3+1], row[x*3+2] = c.B, c.G, c.R
			default:
				t.Fatalf("buildTestBMPEntry: unsupported bitCount %d", bitCount)
			}
		}
		buf.Write(row)
	}

	if withMask {
		maskRowBytes := ((width + 31) / 32) * 4
		for y := height - 1; y >= 0; y-- {
			row := make([]byte, maskRowBytes)
			for x := 0; x < width; x++ {
				if pixels[y][x].A == 0 {
					row[x/8] |= 1 << uint(7-x%8)
				}
			}
			buf.Write(row)
		}
	}
	return buf.Bytes()
}

func solidPixels(width, height int, c color.NRGBA) [][]color.NRGBA {
	rows := make([][]color.NRGBA, height)
	for y := range rows {
		row := make([]color.NRGBA, width)
		for x := range row {
			row[x] = c
		}
		rows[y] = row
	}
	return rows
}

func decodeTestPNG(t *testing.T, data []byte) image.Image {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode result as PNG: %v", err)
	}
	return img
}

func TestExtractImageFromICO_SingleEntry(t *testing.T) {
	png16 := encodeTestPNG(t, 16)
	ico := buildTestICO(t, icoEntry(16, png16))

	got, err := ExtractImageFromICO(ico)
	if err != nil {
		t.Fatalf("ExtractImageFromICO: %v", err)
	}
	if !bytes.Equal(got, png16) {
		t.Error("ExtractImageFromICO did not return the embedded PNG bytes")
	}
}

func TestExtractImageFromICO_PicksLargestPNGEntry(t *testing.T) {
	png16 := encodeTestPNG(t, 16)
	png32 := encodeTestPNG(t, 32)
	ico := buildTestICO(t, icoEntry(16, png16), icoEntry(32, png32))

	got, err := ExtractImageFromICO(ico)
	if err != nil {
		t.Fatalf("ExtractImageFromICO: %v", err)
	}
	if !bytes.Equal(got, png32) {
		t.Error("ExtractImageFromICO did not pick the largest (32x32) entry")
	}
}

func TestExtractImageFromICO_DecodesLegacy32bppBMPEntry(t *testing.T) {
	red := color.NRGBA{R: 200, G: 10, B: 10, A: 255}
	bmp := buildTestBMPEntry(t, 8, 8, 32, solidPixels(8, 8, red), false)
	ico := buildTestICO(t, icoEntry(8, bmp))

	got, err := ExtractImageFromICO(ico)
	if err != nil {
		t.Fatalf("ExtractImageFromICO: %v", err)
	}
	img := decodeTestPNG(t, got)
	if b := img.Bounds(); b.Dx() != 8 || b.Dy() != 8 {
		t.Fatalf("decoded bounds = %v, want 8x8", b)
	}
	r, g, b, a := img.At(0, 0).RGBA()
	if r>>8 != 200 || g>>8 != 10 || b>>8 != 10 || a>>8 != 255 {
		t.Errorf("pixel (0,0) = (%d,%d,%d,%d), want (200,10,10,255)", r>>8, g>>8, b>>8, a>>8)
	}
}

func TestExtractImageFromICO_PicksLargestAcrossPNGAndBMP(t *testing.T) {
	png16 := encodeTestPNG(t, 16)
	bmp32 := buildTestBMPEntry(t, 32, 32, 24, solidPixels(32, 32, color.NRGBA{R: 1, G: 2, B: 3, A: 255}), false)
	ico := buildTestICO(t, icoEntry(16, png16), icoEntry(32, bmp32))

	got, err := ExtractImageFromICO(ico)
	if err != nil {
		t.Fatalf("ExtractImageFromICO: %v", err)
	}
	img := decodeTestPNG(t, got)
	if b := img.Bounds(); b.Dx() != 32 || b.Dy() != 32 {
		t.Fatalf("decoded bounds = %v, want the larger 32x32 BMP entry, got %v", b, b)
	}
}

func TestExtractImageFromICO_32bppAllZeroAlphaFallsBackToANDMask(t *testing.T) {
	// A legacy authoring quirk (see decodeBMPEntry's doc comment): every
	// XOR-data alpha byte is 0, so transparency must come entirely from
	// the AND mask instead — column 0 opaque (mask bit 0), column 1
	// transparent (mask bit 1). buildTestBMPEntry derives its mask from
	// each pixel's own alpha, which can't express "same RGB, different
	// mask bit" when every alpha byte is already 0, so this test builds
	// the entry by hand instead.
	bmp := buildManualMaskBMPEntry(t, color.NRGBA{R: 50, G: 60, B: 70})

	ico := buildTestICO(t, icoEntry(2, bmp))
	got, err := ExtractImageFromICO(ico)
	if err != nil {
		t.Fatalf("ExtractImageFromICO: %v", err)
	}
	img := decodeTestPNG(t, got)
	_, _, _, a0 := img.At(0, 0).RGBA()
	_, _, _, a1 := img.At(1, 0).RGBA()
	if a0>>8 != 255 {
		t.Errorf("column 0 alpha = %d, want 255 (opaque per AND mask)", a0>>8)
	}
	if a1>>8 != 0 {
		t.Errorf("column 1 alpha = %d, want 0 (transparent per AND mask)", a1>>8)
	}
}

// buildManualMaskBMPEntry builds a 2x1, 32bpp, all-zero-alpha BMP entry
// with an explicit AND mask: column 0 opaque (mask bit 0), column 1
// transparent (mask bit 1).
func buildManualMaskBMPEntry(t *testing.T, c color.NRGBA) []byte {
	t.Helper()
	var buf bytes.Buffer
	header := struct {
		Size                  uint32
		Width, Height         int32
		Planes, BitCount      uint16
		Compression, SizeImg  uint32
		XPPM, YPPM            int32
		ClrUsed, ClrImportant uint32
	}{Size: 40, Width: 2, Height: 2, Planes: 1, BitCount: 32} // Height=2 == 2 * actual height (1)
	if err := binary.Write(&buf, binary.LittleEndian, header); err != nil {
		t.Fatalf("write BITMAPINFOHEADER: %v", err)
	}
	// One XOR row (4-byte-aligned already at 2*4=8 bytes): both pixels
	// BGRA with alpha 0.
	buf.Write([]byte{c.B, c.G, c.R, 0, c.B, c.G, c.R, 0})
	// One AND mask row, padded to 4 bytes: bit0 (MSB of first byte) =
	// column 0 = 0 (opaque), bit1 = column 1 = 1 (transparent).
	buf.Write([]byte{0b01000000, 0, 0, 0})
	return buf.Bytes()
}

func TestExtractImageFromICO_24bppUsesANDMask(t *testing.T) {
	blue := color.NRGBA{R: 5, G: 6, B: 7, A: 255}
	pixels := [][]color.NRGBA{{blue, blue}}
	pixels[0][1] = color.NRGBA{R: 5, G: 6, B: 7, A: 0} // marks column 1 transparent in the mask
	bmp := buildTestBMPEntry(t, 2, 1, 24, pixels, true)
	ico := buildTestICO(t, icoEntry(2, bmp))

	got, err := ExtractImageFromICO(ico)
	if err != nil {
		t.Fatalf("ExtractImageFromICO: %v", err)
	}
	img := decodeTestPNG(t, got)
	_, _, _, a0 := img.At(0, 0).RGBA()
	_, _, _, a1 := img.At(1, 0).RGBA()
	if a0>>8 != 255 {
		t.Errorf("column 0 alpha = %d, want 255 (24bpp has no alpha channel of its own)", a0>>8)
	}
	if a1>>8 != 0 {
		t.Errorf("column 1 alpha = %d, want 0 (transparent per AND mask)", a1>>8)
	}
}

func TestExtractImageFromICO_RejectsCompressedBMPEntry(t *testing.T) {
	bmp := buildTestBMPEntry(t, 4, 4, 24, solidPixels(4, 4, color.NRGBA{A: 255}), false)
	binary.LittleEndian.PutUint32(bmp[16:20], 1) // BI_RLE8
	ico := buildTestICO(t, icoEntry(4, bmp))

	if _, err := ExtractImageFromICO(ico); err == nil {
		t.Error("expected an error for a compressed BMP entry")
	}
}

func TestExtractImageFromICO_RejectsUnsupportedBitDepth(t *testing.T) {
	bmp := buildTestBMPEntry(t, 4, 4, 24, solidPixels(4, 4, color.NRGBA{A: 255}), false)
	binary.LittleEndian.PutUint16(bmp[14:16], 16) // 16bpp is not one of this package's supported depths
	ico := buildTestICO(t, icoEntry(4, bmp))

	if _, err := ExtractImageFromICO(ico); err == nil {
		t.Error("expected an error for an unsupported bit depth")
	}
}

func TestExtractImageFromICO_RejectsTruncatedData(t *testing.T) {
	cases := map[string][]byte{
		"empty":                nil,
		"too short for header": {0, 0, 1, 0},
		"not an icon (type 2)": func() []byte {
			b := buildTestICO(t, icoEntry(16, encodeTestPNG(t, 16)))
			binary.LittleEndian.PutUint16(b[2:4], 2) // corrupt type to "cursor"
			return b
		}(),
		"directory exceeds data": {0, 0, 1, 0, 5, 0}, // count=5 but no entries follow
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ExtractImageFromICO(data); err == nil {
				t.Errorf("expected an error for %s", name)
			}
		})
	}
}

func TestExtractImageFromICO_SkipsEntryWithOutOfBoundsOffset(t *testing.T) {
	// A well-formed directory naming an offset/size that runs past the
	// actual data must be skipped, not panic.
	ico := buildTestICO(t, icoEntry(16, encodeTestPNG(t, 16)))
	// Corrupt the one entry's BytesInRes (ICONDIRENTRY byte 8, i.e. data
	// offset 6+8=14: ICONDIR is 6 bytes, then Width/Height/ColorCount/
	// Reserved/Planes/BitCount occupy the entry's first 8 bytes) to claim
	// far more data than exists.
	binary.LittleEndian.PutUint32(ico[14:18], 1<<20)

	if _, err := ExtractImageFromICO(ico); err == nil {
		t.Error("expected an error when the only entry's bounds are invalid")
	}
}

func newFaviconTestClient() *safehttp.Client {
	return safehttp.NewClient(safehttp.Config{
		MaxRedirects:      3,
		AllowInsecureHTTP: true,
		AllowIPForTesting: func(net.IP) bool { return true },
	})
}

// Fetch itself always builds an https:// URL (see favicon.go's doc
// comment), so these tests drive fetchURL directly against a plain-http
// httptest.Server rather than Fetch — a real TLS handshake the client
// would trust isn't available here, matching how internal/drive's own
// safehttp-based tests avoid TLS entirely (AllowInsecureHTTP + a plain
// httptest.Server).

func TestFetchURL_Success(t *testing.T) {
	ico := buildTestICO(t, icoEntry(32, encodeTestPNG(t, 32)))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/favicon.ico" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(ico)
	}))
	defer srv.Close()

	got, err := fetchURL(t.Context(), newFaviconTestClient(), srv.URL+"/favicon.ico", 1<<20)
	if err != nil {
		t.Fatalf("fetchURL: %v", err)
	}
	if len(got) == 0 {
		t.Error("fetchURL returned no data")
	}
}

func TestFetchURL_BarePNGResponseIsReturnedAsIs(t *testing.T) {
	// Some hosts serve /favicon.ico as a literal PNG file, not an ICO
	// container (Issue #146's decodeCandidate).
	png32 := encodeTestPNG(t, 32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(png32)
	}))
	defer srv.Close()

	got, err := fetchURL(t.Context(), newFaviconTestClient(), srv.URL+"/favicon.ico", 1<<20)
	if err != nil {
		t.Fatalf("fetchURL: %v", err)
	}
	if !bytes.Equal(got, png32) {
		t.Error("fetchURL did not return the bare PNG response unchanged")
	}
}

func TestFetchURL_UnrecognizedResponseIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>not an icon</html>"))
	}))
	defer srv.Close()

	if _, err := fetchURL(t.Context(), newFaviconTestClient(), srv.URL+"/favicon.ico", 1<<20); err == nil {
		t.Error("expected an error for a response that is neither an ICO nor a recognized image")
	}
}

func TestFetchURL_NonOKStatusIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	if _, err := fetchURL(t.Context(), newFaviconTestClient(), srv.URL+"/favicon.ico", 1<<20); err == nil {
		t.Error("expected an error for a 404 response")
	}
}

func TestFetchURL_OversizedResponseIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(make([]byte, 100))
	}))
	defer srv.Close()

	if _, err := fetchURL(t.Context(), newFaviconTestClient(), srv.URL+"/favicon.ico", 10); err == nil {
		t.Error("expected an error for a response exceeding maxBytes")
	}
}

func TestFetch_AlwaysUsesHTTPSScheme(t *testing.T) {
	// A plain-http test server dialed via Fetch's hardcoded https://
	// prefix must fail the TLS handshake rather than silently falling
	// back to http, proving Fetch never honors AllowInsecureHTTP for its
	// own request scheme.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should never be reached over a downgraded http connection")
	}))
	defer srv.Close()

	if _, err := Fetch(t.Context(), newFaviconTestClient(), srv.Listener.Addr().String(), 1<<20); err == nil {
		t.Error("expected an error when Fetch's https request hits a plain-http server")
	}
}

// --- FetchFromPage / <link rel=icon> resolution (Issue #146) ---

func TestFetchFromPage_PicksIconOverAppleTouchIcon(t *testing.T) {
	png32 := encodeTestPNG(t, 32)
	var requested []string
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html><head>
			<link rel="apple-touch-icon" href="/apple.png" sizes="180x180">
			<link rel="icon" href="/icon.png" sizes="32x32">
			</head><body></body></html>`))
	})
	mux.HandleFunc("/icon.png", func(w http.ResponseWriter, r *http.Request) {
		requested = append(requested, r.URL.Path)
		_, _ = w.Write(png32)
	})
	mux.HandleFunc("/apple.png", func(w http.ResponseWriter, r *http.Request) {
		requested = append(requested, r.URL.Path)
		_, _ = w.Write(png32)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	got, err := FetchFromPage(t.Context(), newFaviconTestClient(), srv.URL+"/", 1<<20)
	if err != nil {
		t.Fatalf("FetchFromPage: %v", err)
	}
	if !bytes.Equal(got, png32) {
		t.Error("FetchFromPage did not return the icon's bytes")
	}
	if len(requested) != 1 || requested[0] != "/icon.png" {
		t.Errorf("requested = %v, want exactly [/icon.png] (rel=icon must be tried before apple-touch-icon)", requested)
	}
}

func TestFetchFromPage_PicksLargestDeclaredSize(t *testing.T) {
	small := encodeTestPNG(t, 16)
	large := encodeTestPNG(t, 64)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html><head>
			<link rel="icon" href="/small.png" sizes="16x16">
			<link rel="icon" href="/large.png" sizes="64x64">
			</head></html>`))
	})
	mux.HandleFunc("/small.png", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(small) })
	mux.HandleFunc("/large.png", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(large) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	got, err := FetchFromPage(t.Context(), newFaviconTestClient(), srv.URL+"/", 1<<20)
	if err != nil {
		t.Fatalf("FetchFromPage: %v", err)
	}
	if !bytes.Equal(got, large) {
		t.Error("FetchFromPage did not pick the largest declared sizes= candidate")
	}
}

func TestFetchFromPage_ResolvesRelativeAndProtocolRelativeHref(t *testing.T) {
	png32 := encodeTestPNG(t, 32)
	mux := http.NewServeMux()
	mux.HandleFunc("/dir/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html><head><link rel="icon" href="../relative.png"></head></html>`))
	})
	mux.HandleFunc("/relative.png", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(png32) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	got, err := FetchFromPage(t.Context(), newFaviconTestClient(), srv.URL+"/dir/", 1<<20)
	if err != nil {
		t.Fatalf("FetchFromPage: %v", err)
	}
	if !bytes.Equal(got, png32) {
		t.Error("FetchFromPage did not resolve the relative href against the page's own URL")
	}
}

func TestFetchFromPage_SkipsSVGCandidate(t *testing.T) {
	png32 := encodeTestPNG(t, 32)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html><head>
			<link rel="icon" type="image/svg+xml" href="/icon.svg">
			<link rel="icon" href="/icon.png">
			</head></html>`))
	})
	mux.HandleFunc("/icon.svg", func(w http.ResponseWriter, r *http.Request) {
		t.Error("an SVG candidate must never be fetched")
	})
	mux.HandleFunc("/icon.png", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(png32) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	got, err := FetchFromPage(t.Context(), newFaviconTestClient(), srv.URL+"/", 1<<20)
	if err != nil {
		t.Fatalf("FetchFromPage: %v", err)
	}
	if !bytes.Equal(got, png32) {
		t.Error("FetchFromPage did not fall through to the non-SVG candidate")
	}
}

func TestFetchFromPage_StopsScanningAtBodyTag(t *testing.T) {
	png32 := encodeTestPNG(t, 32)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html><head></head><body>
			<link rel="icon" href="/after-body.png">
			</body></html>`))
	})
	mux.HandleFunc("/after-body.png", func(w http.ResponseWriter, r *http.Request) {
		t.Error("a <link> appearing after <body> must never be treated as a candidate")
		_, _ = w.Write(png32)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	if _, err := FetchFromPage(t.Context(), newFaviconTestClient(), srv.URL+"/", 1<<20); err == nil {
		t.Error("expected an error: no candidate before </head>/<body>")
	}
}

func TestFetchFromPage_LimitsCandidateFetchesToThree(t *testing.T) {
	var mu sync.Mutex
	fetchCount := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html><head>
			<link rel="icon" href="/1.png" sizes="10x10">
			<link rel="icon" href="/2.png" sizes="9x9">
			<link rel="icon" href="/3.png" sizes="8x8">
			<link rel="icon" href="/4.png" sizes="7x7">
			</head></html>`))
	})
	fail := func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		fetchCount++
		mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
	}
	mux.HandleFunc("/1.png", fail)
	mux.HandleFunc("/2.png", fail)
	mux.HandleFunc("/3.png", fail)
	mux.HandleFunc("/4.png", fail)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	if _, err := FetchFromPage(t.Context(), newFaviconTestClient(), srv.URL+"/", 1<<20); err == nil {
		t.Fatal("expected an error: every candidate 404s")
	}
	mu.Lock()
	defer mu.Unlock()
	if fetchCount != maxIconCandidates {
		t.Errorf("fetchCount = %d, want exactly maxIconCandidates (%d)", fetchCount, maxIconCandidates)
	}
}

func TestFetchFromPage_NoCandidatesIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html><head><title>no icons here</title></head></html>`))
	}))
	defer srv.Close()

	if _, err := FetchFromPage(t.Context(), newFaviconTestClient(), srv.URL+"/", 1<<20); err == nil {
		t.Error("expected an error when the page names no icon candidate")
	}
}

func TestParseSizesAttr(t *testing.T) {
	cases := map[string]int{
		"32x32":       32,
		"16x16 32x32": 32,
		"any":         0,
		"":            0,
		"not-a-size":  0,
		"48X48":       48,
	}
	for in, want := range cases {
		if got := parseSizesAttr(in); got != want {
			t.Errorf("parseSizesAttr(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestDecodeCandidate_UnsupportedFormatIsError(t *testing.T) {
	if _, err := decodeCandidate([]byte("plain text, not an image")); err == nil {
		t.Error("expected an error for a non-image, non-ICO response")
	}
}

func TestDecodeCandidate_BarePNGReturnedUnchanged(t *testing.T) {
	png32 := encodeTestPNG(t, 32)
	got, err := decodeCandidate(png32)
	if err != nil {
		t.Fatalf("decodeCandidate: %v", err)
	}
	if !bytes.Equal(got, png32) {
		t.Error("decodeCandidate did not return the bare PNG unchanged")
	}
}

// --- Resolve (Issue #146) ---
//
// Resolve's own host-based steps (Fetch) always build an https:// URL
// (see Fetch's doc comment), so — like TestFetch_AlwaysUsesHTTPSScheme
// above — a real success path can't be driven against a plain-http (or
// self-signed-TLS) test server here. These tests instead exercise
// Resolve's fail-fast and full-failure control flow using the
// production (non-test) IP policy, which rejects a loopback host before
// any network I/O is attempted at all — fast and fully deterministic,
// the same trick cmd/server/main_test.go's own fixture relies on.

func TestResolve_InvalidFeedURLFailsFast(t *testing.T) {
	if _, err := Resolve(t.Context(), newFaviconTestClient(), "://not-a-url", 1<<20); err == nil {
		t.Error("expected an error for an unparseable feed URL")
	}
}

func TestResolve_AllStepsFailReturnsJoinedError(t *testing.T) {
	client := safehttp.NewClient(safehttp.Config{MaxRedirects: 3, AllowInsecureHTTP: false})
	_, err := Resolve(t.Context(), client, "https://localhost/feed.xml", 1<<20)
	if err == nil {
		t.Fatal("expected an error when every resolution step fails")
	}
	if !strings.Contains(err.Error(), "no resolution succeeded") {
		t.Errorf("error = %q, want it to name the overall failure", err.Error())
	}
}
