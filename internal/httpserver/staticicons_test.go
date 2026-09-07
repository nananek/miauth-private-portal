package httpserver

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"image/png"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStaticIcons_EveryRouteServesOKWithExpectedContentType(t *testing.T) {
	srv, _ := newTestServer()

	for _, route := range staticIconRoutes {
		t.Run(route.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, route.path, nil)
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s = %d, want %d", route.path, rec.Code, http.StatusOK)
			}
			if got := rec.Header().Get("Content-Type"); got != route.contentType {
				t.Errorf("GET %s Content-Type = %q, want %q", route.path, got, route.contentType)
			}
			if rec.Body.Len() == 0 {
				t.Errorf("GET %s returned an empty body", route.path)
			}
			if got := rec.Header().Get("Cache-Control"); got == "" {
				t.Errorf("GET %s has no Cache-Control header", route.path)
			}
		})
	}
}

func TestStaticIcons_UnknownIconPathIs404(t *testing.T) {
	srv, _ := newTestServer()

	req := httptest.NewRequest(http.MethodGet, "/favicon-does-not-exist.png", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /favicon-does-not-exist.png = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

// TestStaticIcons_PNGFilesDecodeAtTheirDeclaredSize backs the
// staticassets/gen generator's contract: every *.png route's bytes must
// actually decode as a PNG at the pixel dimensions its filename/route
// promises (16, 32, 180, 192, or 512), not just be a nonempty blob.
func TestStaticIcons_PNGFilesDecodeAtTheirDeclaredSize(t *testing.T) {
	cases := []struct {
		path          string
		width, height int
	}{
		{"/favicon-16x16.png", 16, 16},
		{"/favicon-32x32.png", 32, 32},
		{"/favicon-16x16-dark.png", 16, 16},
		{"/favicon-32x32-dark.png", 32, 32},
		{"/apple-touch-icon.png", 180, 180},
		{"/icon-192.png", 192, 192},
		{"/icon-512.png", 512, 512},
		{"/og-image.png", 1200, 630},
	}

	srv, _ := newTestServer()
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)

			cfg, err := png.DecodeConfig(rec.Body)
			if err != nil {
				t.Fatalf("DecodeConfig: %v", err)
			}
			if cfg.Width != tc.width || cfg.Height != tc.height {
				t.Errorf("%s decoded as %dx%d, want %dx%d", tc.path, cfg.Width, cfg.Height, tc.width, tc.height)
			}
		})
	}
}

// TestStaticIcons_FaviconICOContainsBothPNGEntries backs
// staticassets/gen's encodeICO: favicon.ico must be a well-formed
// "PNG-in-ICO" container (ICONDIR type=1, exactly two ICONDIRENTRY
// records) whose two embedded images are themselves valid PNGs at 16x16
// and 32x32, not raw legacy BMP data.
func TestStaticIcons_FaviconICOContainsBothPNGEntries(t *testing.T) {
	srv, _ := newTestServer()

	req := httptest.NewRequest(http.MethodGet, "/favicon.ico", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	data := rec.Body.Bytes()
	if len(data) < 6 {
		t.Fatalf("favicon.ico body too short: %d bytes", len(data))
	}

	var dir struct{ Reserved, Type, Count uint16 }
	if err := binary.Read(bytes.NewReader(data[:6]), binary.LittleEndian, &dir); err != nil {
		t.Fatalf("read ICONDIR: %v", err)
	}
	if dir.Reserved != 0 || dir.Type != 1 {
		t.Fatalf("ICONDIR = %+v, want Reserved=0 Type=1", dir)
	}
	if dir.Count != 2 {
		t.Fatalf("ICONDIR.Count = %d, want 2", dir.Count)
	}

	wantSizes := []int{16, 32}
	for i := range int(dir.Count) {
		entryOff := 6 + i*16
		var entry struct {
			Width, Height           uint8
			ColorCount, Reserved    uint8
			Planes, BitCount        uint16
			BytesInRes, ImageOffset uint32
		}
		if err := binary.Read(bytes.NewReader(data[entryOff:entryOff+16]), binary.LittleEndian, &entry); err != nil {
			t.Fatalf("read ICONDIRENTRY[%d]: %v", i, err)
		}
		if int(entry.Width) != wantSizes[i] || int(entry.Height) != wantSizes[i] {
			t.Errorf("ICONDIRENTRY[%d] size = %dx%d, want %dx%d", i, entry.Width, entry.Height, wantSizes[i], wantSizes[i])
		}

		imgData := data[entry.ImageOffset : entry.ImageOffset+entry.BytesInRes]
		cfg, err := png.DecodeConfig(bytes.NewReader(imgData))
		if err != nil {
			t.Fatalf("ICONDIRENTRY[%d] image data is not a valid PNG: %v", i, err)
		}
		if cfg.Width != wantSizes[i] || cfg.Height != wantSizes[i] {
			t.Errorf("ICONDIRENTRY[%d] embedded PNG decoded as %dx%d, want %dx%d", i, cfg.Width, cfg.Height, wantSizes[i], wantSizes[i])
		}
	}
}

func TestStaticIcons_WebManifestIsValidAndReferencesIcons(t *testing.T) {
	srv, _ := newTestServer()

	req := httptest.NewRequest(http.MethodGet, "/site.webmanifest", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	var manifest struct {
		Name       string `json:"name"`
		ShortName  string `json:"short_name"`
		ThemeColor string `json:"theme_color"`
		Icons      []struct {
			Src   string `json:"src"`
			Sizes string `json:"sizes"`
		} `json:"icons"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &manifest); err != nil {
		t.Fatalf("site.webmanifest is not valid JSON: %v", err)
	}
	if manifest.Name == "" || manifest.ShortName == "" || manifest.ThemeColor == "" {
		t.Errorf("manifest = %+v, want name/short_name/theme_color all set", manifest)
	}
	if len(manifest.Icons) < 2 {
		t.Fatalf("manifest.Icons has %d entries, want at least 2 (192 and 512)", len(manifest.Icons))
	}
	for _, icon := range manifest.Icons {
		req := httptest.NewRequest(http.MethodGet, icon.Src, nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("manifest icon %s (%s) = %d, want %d", icon.Src, icon.Sizes, rec.Code, http.StatusOK)
		}
	}
}
