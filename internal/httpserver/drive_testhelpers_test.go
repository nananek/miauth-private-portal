package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/nananek/miauth-private-portal/internal/domain"
	"github.com/nananek/miauth-private-portal/internal/drive"
	"github.com/nananek/miauth-private-portal/internal/health"
	"github.com/nananek/miauth-private-portal/internal/ingest/safehttp"
	"github.com/nananek/miauth-private-portal/internal/logging"
	"github.com/nananek/miauth-private-portal/internal/miauth"
	"github.com/nananek/miauth-private-portal/internal/storage/sqlite"
	"github.com/nananek/miauth-private-portal/internal/timeline"
)

const driveTestMaxFileBytes = 10 << 20

// driveTestServer bundles a real Server with MiAuthService and a real
// drive.Service (backed by a fake in-memory Storage — see
// internal/drive's own fakeStorage; this package writes an equivalent
// one below since fakeStorage is unexported to internal/drive) wired.
// tokenRead/tokenWrite are already-issued API tokens carrying exactly
// read:drive and write:drive respectively, mirroring
// docs/compat/aria-v1.5.11.md's Drive API scope split.
type driveTestServer struct {
	*Server
	db         *sqlite.DB
	tokenRead  string
	tokenWrite string
	ownerID    string
}

// driveFakeStorage is internal/httpserver's own copy of internal/drive's
// test-only in-memory Storage (unexported there, so not reusable across
// the package boundary) — see that package's fakeStorage doc comment for
// why handler-level tests should not need real disk or network I/O.
type driveFakeStorage struct {
	objects map[string][]byte
}

func newDriveFakeStorage() *driveFakeStorage { return &driveFakeStorage{objects: map[string][]byte{}} }

func (f *driveFakeStorage) Put(_ context.Context, key string, r io.Reader, _ int64) error {
	if _, exists := f.objects[key]; exists {
		return drive.ErrKeyExists
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.objects[key] = data
	return nil
}

func (f *driveFakeStorage) Get(_ context.Context, key string) (io.ReadCloser, error) {
	data, ok := f.objects[key]
	if !ok {
		return nil, fmt.Errorf("drive: key %q not found: %w", key, domain.ErrNotFound)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (f *driveFakeStorage) Delete(_ context.Context, key string) error {
	delete(f.objects, key)
	return nil
}

func (f *driveFakeStorage) List(_ context.Context) ([]string, error) {
	keys := make([]string, 0, len(f.objects))
	for k := range f.objects {
		keys = append(keys, k)
	}
	return keys, nil
}

func newDriveTestServer(t *testing.T) *driveTestServer {
	t.Helper()
	db, err := sqlite.Open(t.Context(), sqlite.Config{Path: filepath.Join(t.TempDir(), "test.db"), BusyTimeout: 5 * time.Second, MaxOpenConns: 4})
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(t.Context()); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	if err := db.Actors.EnsureReservedActors(t.Context()); err != nil {
		t.Fatalf("ensure reserved actors: %v", err)
	}

	miauthCfg := defaultMiAuthTestConfig()
	miauthSvc := miauth.NewService(db, db.Repos, miauthCfg)

	driveSvc := drive.NewService(
		newDriveFakeStorage(),
		safehttp.NewClient(safehttp.Config{MaxRedirects: 3, AllowInsecureHTTP: true, AllowIPForTesting: func(net.IP) bool { return true }}),
		db.Repos,
		drive.Config{MaxFileBytes: driveTestMaxFileBytes, MaxImageWidth: 8000, MaxImageHeight: 8000, CapacityBytes: 1 << 30},
	)

	// TimelineService is wired too, matching cmd/server's own shape
	// (Drive and the note timeline are never constructed independently
	// in production — see cmd/server/main.go): POST /api/endpoints only
	// registers when both are present (NewServer), and this package's
	// existing implementedEndpoints contract test needs it reachable.
	timelineSvc := timeline.NewService(db, db.Repos, timeline.Config{})

	logger := logging.New(&bytes.Buffer{}, logging.Config{Format: "json", Level: "info"})
	reg := health.NewRegistry()
	srv := NewServer(logger, reg, Options{
		MiAuthService:     miauthSvc,
		TimelineService:   timelineSvc,
		LocalOrigin:       testLocalOrigin,
		Drive:             driveSvc,
		DriveMaxFileBytes: driveTestMaxFileBytes,
	})

	ts := &driveTestServer{Server: srv, db: db}
	ts.tokenRead, ts.ownerID = mustIssueToken(t, ts.Server, "drive-setup-read", "read:drive")
	ts.tokenWrite, _ = mustIssueToken(t, ts.Server, "drive-setup-write", "write:drive")
	return ts
}

// post sends a JSON POST to path using token for the "i" field.
func (ts *driveTestServer) post(t *testing.T, path, token string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	if body == nil {
		body = map[string]any{}
	}
	if _, ok := body["i"]; !ok {
		body["i"] = token
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, req)
	return rec
}

// testPNG returns a small valid PNG's bytes, for tests that need a raster
// image accepted by internal/drive.ValidateImage.
func testPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.RGBA{R: 200, G: 100, B: 50, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode test PNG: %v", err)
	}
	return buf.Bytes()
}
