package drive

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"
)

// fakeS3Server is a minimal S3-compatible HTTP server covering exactly
// the PUT/GET/HEAD/DELETE object operations S3 exercises, so S3's tests
// run against real HTTP round trips through minio-go without any real
// network access or credentials (AGENTS.md: "normal tests must not
// require real credentials or network access"). It does not verify
// AWS Signature V4 authorization — minio-go still sends it, this server
// simply never checks it, since request authenticity is minio-go's and
// the real backend's concern, not this package's.
type fakeS3Server struct {
	mu      sync.Mutex
	objects map[string][]byte // "/bucket/key" -> content
}

func newFakeS3Server(t *testing.T) *httptest.Server {
	t.Helper()
	fake := &fakeS3Server{objects: map[string][]byte{}}
	srv := httptest.NewServer(http.HandlerFunc(fake.handle))
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeS3Server) handle(w http.ResponseWriter, r *http.Request) {
	// minio-go's PutObject caches the bucket's region by first sending
	// GET /<bucket>/?location — a real S3-compatible server answers this
	// regardless of whether the bucket itself has any objects, so the
	// fake must special-case it before falling into the ordinary
	// per-object GET handling below (which would otherwise 404 it as a
	// missing "/<bucket>/" key).
	if r.Method == http.MethodGet {
		if _, ok := r.URL.Query()["location"]; ok {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>` +
				`<LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/"></LocationConstraint>`))
			return
		}
	}

	path, err := url.PathUnescape(r.URL.Path)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	switch r.Method {
	case http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		f.objects[path] = body
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		body, ok := f.objects[path]
		if !ok {
			writeS3NotFound(w, path)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		w.Header().Set("ETag", `"fake-etag"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	case http.MethodHead:
		body, ok := f.objects[path]
		if !ok {
			writeS3NotFound(w, path)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		w.Header().Set("ETag", `"fake-etag"`)
		w.WriteHeader(http.StatusOK)
	case http.MethodDelete:
		// Real S3 DELETE is unconditionally successful whether or not
		// the key existed, matching Storage.Delete's own idempotency
		// contract.
		delete(f.objects, path)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func writeS3NotFound(w http.ResponseWriter, key string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusNotFound)
	if key == "" {
		key = "/"
	}
	// HEAD responses in real net/http never send this body (net/http
	// strips it), which is fine: minio-go maps a bare 404 status on a
	// HEAD response to NoSuchKey without needing the XML body — see
	// httpRespToErrorResponse in minio-go's api-error-response.go.
	_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>` +
		`<Error><Code>NoSuchKey</Code><Message>The specified key does not exist.</Message>` +
		`<Key>` + key + `</Key><RequestId>test</RequestId></Error>`))
}

func newTestS3(t *testing.T) Storage {
	t.Helper()
	srv := newFakeS3Server(t)
	endpoint := srv.URL[len("http://"):]
	s, err := NewS3(S3Config{
		Endpoint:        endpoint,
		Bucket:          "test-bucket",
		AccessKeyID:     "test-access-key",
		SecretAccessKey: "test-secret-key",
		UseSSL:          false,
	})
	if err != nil {
		t.Fatalf("NewS3: %v", err)
	}
	return s
}

func TestS3_StorageContract(t *testing.T) {
	testStorageContract(t, newTestS3)
}

func TestS3_RejectsEmptyBucket(t *testing.T) {
	_, err := NewS3(S3Config{Endpoint: "example.com", Bucket: ""})
	if err == nil {
		t.Fatal("expected an error for an empty bucket")
	}
}

func TestS3_ContextCancellation(t *testing.T) {
	s := newTestS3(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Put(ctx, "k", nil, 0); err == nil {
		t.Error("expected Put to fail with a cancelled context")
	}
}
