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

// buildTestICO assembles a minimal PNG-in-ICO container from the given
// (widthPx, pngData) entries, mirroring internal/httpserver/staticassets/
// gen's own encodeICO — duplicated here rather than imported (that
// function lives in an unrelated package main, not a reusable library).
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

func TestExtractPNGFromICO_SingleEntry(t *testing.T) {
	png16 := encodeTestPNG(t, 16)
	ico := buildTestICO(t, struct {
		widthPx int
		data    []byte
	}{16, png16})

	got, err := ExtractPNGFromICO(ico)
	if err != nil {
		t.Fatalf("ExtractPNGFromICO: %v", err)
	}
	if !bytes.Equal(got, png16) {
		t.Error("ExtractPNGFromICO did not return the embedded PNG bytes")
	}
}

func TestExtractPNGFromICO_PicksLargestEntry(t *testing.T) {
	png16 := encodeTestPNG(t, 16)
	png32 := encodeTestPNG(t, 32)
	ico := buildTestICO(t,
		struct {
			widthPx int
			data    []byte
		}{16, png16},
		struct {
			widthPx int
			data    []byte
		}{32, png32},
	)

	got, err := ExtractPNGFromICO(ico)
	if err != nil {
		t.Fatalf("ExtractPNGFromICO: %v", err)
	}
	if !bytes.Equal(got, png32) {
		t.Error("ExtractPNGFromICO did not pick the largest (32x32) entry")
	}
}

func TestExtractPNGFromICO_RejectsLegacyBMPEntry(t *testing.T) {
	fakeBMPData := []byte{0x28, 0x00, 0x00, 0x00, 0xAA, 0xBB, 0xCC, 0xDD} // not a PNG signature
	ico := buildTestICO(t, struct {
		widthPx int
		data    []byte
	}{16, fakeBMPData})

	if _, err := ExtractPNGFromICO(ico); err == nil {
		t.Error("expected an error for a legacy BMP-in-ICO entry")
	}
}

func TestExtractPNGFromICO_RejectsTruncatedData(t *testing.T) {
	cases := map[string][]byte{
		"empty":                nil,
		"too short for header": {0, 0, 1, 0},
		"not an icon (type 2)": func() []byte {
			b := buildTestICO(t, struct {
				widthPx int
				data    []byte
			}{16, encodeTestPNG(t, 16)})
			binary.LittleEndian.PutUint16(b[2:4], 2) // corrupt type to "cursor"
			return b
		}(),
		"directory exceeds data": {0, 0, 1, 0, 5, 0}, // count=5 but no entries follow
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ExtractPNGFromICO(data); err == nil {
				t.Errorf("expected an error for %s", name)
			}
		})
	}
}

func TestExtractPNGFromICO_SkipsEntryWithOutOfBoundsOffset(t *testing.T) {
	// A well-formed directory naming an offset/size that runs past the
	// actual data must be skipped, not panic.
	ico := buildTestICO(t, struct {
		widthPx int
		data    []byte
	}{16, encodeTestPNG(t, 16)})
	// Corrupt the one entry's BytesInRes (ICONDIRENTRY byte 8, i.e. data
	// offset 6+8=14: ICONDIR is 6 bytes, then Width/Height/ColorCount/
	// Reserved/Planes/BitCount occupy the entry's first 8 bytes) to claim
	// far more data than exists.
	binary.LittleEndian.PutUint32(ico[14:18], 1<<20)

	if _, err := ExtractPNGFromICO(ico); err == nil {
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
	ico := buildTestICO(t, struct {
		widthPx int
		data    []byte
	}{32, encodeTestPNG(t, 32)})
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
