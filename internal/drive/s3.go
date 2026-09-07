package drive

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3Config configures an S3-compatible backend (DRIVE_BACKEND=s3compat):
// AWS S3 itself, or a self-hosted S3-compatible store such as MinIO.
// Credentials arrive as plain values here — internal/config resolves
// the configured secret_ref to its environment-variable value before
// building this struct, the same secret_ref indirection
// internal/openwebui/registry.go uses for its provider API key
// (ADR-0007 reuses that pattern rather than inventing a second one).
type S3Config struct {
	// Endpoint is the host[:port] the S3-compatible API is served from,
	// without a scheme (matching minio-go's own convention) — for
	// example "s3.us-east-1.amazonaws.com" or "minio.internal:9000".
	Endpoint string
	// Bucket is the single bucket every key is stored under. This
	// package never creates or configures the bucket itself; it must
	// already exist (the same "fail closed on missing prerequisite
	// infrastructure rather than silently create it" stance Local takes
	// on its root directory).
	Bucket string
	// AccessKeyID and SecretAccessKey authenticate against Endpoint.
	AccessKeyID     string
	SecretAccessKey string
	// UseSSL selects https (true) or http (false) against Endpoint.
	UseSSL bool
	// Region is passed to the client when non-empty; most S3-compatible
	// servers (MinIO included) do not require it.
	Region string
}

// S3 is a Storage backed by an S3-compatible object store, reached
// through minio-go — a client library whose primary purpose (unlike the
// full AWS SDK for Go v2) is broad S3-compatible-server support, not
// AWS-specific features this service never uses (ADR-0007).
type S3 struct {
	client *minio.Client
	bucket string
}

// NewS3 builds an S3 storage client. It does not verify the bucket
// exists or that the credentials are valid — those failures surface on
// first use (Put/Get/Delete), consistent with this package's other
// constructors never making a network call.
func NewS3(cfg S3Config) (*S3, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("drive: S3Config.Bucket must not be empty")
	}
	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		Secure: cfg.UseSSL,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("drive: build S3 client: %w", err)
	}
	return &S3{client: client, bucket: cfg.Bucket}, nil
}

func isS3NotFound(err error) bool {
	resp := minio.ToErrorResponse(err)
	return resp.Code == "NoSuchKey" || resp.Code == "NotFound"
}

func (s *S3) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	// S3's PUT has no "fail if exists" semantics this library exposes
	// portably across S3-compatible servers, so create-only is enforced
	// with a Stat-then-Put check. This is inherently racy (a concurrent
	// Put for the same key could interleave between the two calls); it
	// is accepted here because every caller of this package assigns
	// keys that are unique-by-construction (a UUID-derived path), so two
	// callers racing to Put the *same* key would itself be a caller bug
	// this check is a best-effort guard against, not a correctness
	// guarantee under adversarial concurrency.
	if _, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{}); err == nil {
		return fmt.Errorf("drive: put key %q: %w", key, ErrKeyExists)
	} else if !isS3NotFound(err) {
		return fmt.Errorf("drive: stat key %q before put: %w", key, err)
	}
	// SendContentMd5/DisableContentSha256 avoid minio-go's default
	// streaming-signed-payload upload encoding (chunked transfer with
	// per-chunk AWS signatures), which not every S3-compatible server
	// this deployment might point at is guaranteed to support — a plain
	// MD5-verified body is the more broadly compatible choice for a
	// "some S3-compatible store, not necessarily AWS S3 itself" backend.
	if _, err := s.client.PutObject(ctx, s.bucket, key, r, size, minio.PutObjectOptions{
		SendContentMd5:       true,
		DisableContentSha256: true,
	}); err != nil {
		return fmt.Errorf("drive: put key %q: %w", key, err)
	}
	return nil
}

func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		if isS3NotFound(err) {
			return nil, notFound(key)
		}
		return nil, fmt.Errorf("drive: get key %q: %w", key, err)
	}
	// minio-go's GetObject only issues the request lazily on first
	// Read/Stat, so a missing key is not yet reported here — Stat forces
	// it now, keeping Get's not-found contract identical to Local's
	// (an error before the caller ever reads a byte, not a mid-stream
	// failure).
	if _, err := obj.Stat(); err != nil {
		_ = obj.Close()
		if isS3NotFound(err) {
			return nil, notFound(key)
		}
		return nil, fmt.Errorf("drive: stat key %q: %w", key, err)
	}
	return obj, nil
}

// List returns every key currently in the bucket, paging through
// minio-go's ListObjects internally (Recursive: true — this backend
// never treats "/" as a folder delimiter the way Local's directory tree
// naturally does).
func (s *S3) List(ctx context.Context) ([]string, error) {
	var keys []string
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Recursive: true}) {
		if obj.Err != nil {
			return nil, fmt.Errorf("drive: list bucket %q: %w", s.bucket, obj.Err)
		}
		keys = append(keys, obj.Key)
	}
	return keys, nil
}

func (s *S3) Delete(ctx context.Context, key string) error {
	if err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		if isS3NotFound(err) {
			return nil
		}
		return fmt.Errorf("drive: delete key %q: %w", key, err)
	}
	return nil
}
