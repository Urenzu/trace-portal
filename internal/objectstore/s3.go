package objectstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3Config describes an S3-compatible bucket.
//
// One config covers MinIO, S3 and R2 because they speak the same protocol.
// That is the whole reason development runs against MinIO: the thing exercised
// on a laptop is the same code path that will run against R2, rather than a
// local shim that agrees with whatever the code assumed.
type S3Config struct {
	// Endpoint is host[:port], without a scheme — "localhost:9000" for MinIO,
	// "<account>.r2.cloudflarestorage.com" for R2. A full URL is accepted and
	// reduced, because that is what people paste.
	Endpoint string

	Bucket    string
	AccessKey string
	SecretKey string

	// Region matters to S3 and is ignored by MinIO. R2 wants "auto".
	Region string

	// UseSSL is on for anything real and off for MinIO on a laptop.
	UseSSL bool
}

// S3 is an object store backed by an S3-compatible bucket.
type S3 struct {
	client *minio.Client
	bucket string
}

// NewS3 connects to a bucket, creating it if it does not exist.
//
// Creating it is right for MinIO in development, where nothing has provisioned
// anything yet. Against R2 or S3 the credentials usually cannot create buckets,
// and the attempt fails harmlessly — the bucket is already there, so the
// existence check short-circuits before any create is tried.
func NewS3(ctx context.Context, cfg S3Config) (*S3, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("s3 object store needs a bucket")
	}
	endpoint, secure, err := normalizeEndpoint(cfg.Endpoint, cfg.UseSSL)
	if err != nil {
		return nil, err
	}

	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: secure,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("connect to object storage: %w", err)
	}

	exists, err := client.BucketExists(ctx, cfg.Bucket)
	if err != nil {
		return nil, fmt.Errorf("reach bucket %s: %w", cfg.Bucket, err)
	}
	if !exists {
		if err := client.MakeBucket(ctx, cfg.Bucket, minio.MakeBucketOptions{Region: cfg.Region}); err != nil {
			return nil, fmt.Errorf("create bucket %s: %w", cfg.Bucket, err)
		}
	}
	return &S3{client: client, bucket: cfg.Bucket}, nil
}

// normalizeEndpoint accepts either a bare host:port or a full URL.
//
// A URL is what someone pastes out of a dashboard, and minio.New wants a host.
// Accepting both and deriving the scheme from the URL when one is given avoids
// the failure where a correct-looking endpoint produces a connection refused
// because the scheme was silently dropped.
func normalizeEndpoint(endpoint string, useSSL bool) (string, bool, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return "", false, errors.New("s3 object store needs an endpoint")
	}
	if !strings.Contains(endpoint, "://") {
		return strings.TrimSuffix(endpoint, "/"), useSSL, nil
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", false, fmt.Errorf("parse endpoint %q: %w", endpoint, err)
	}
	if u.Host == "" {
		return "", false, fmt.Errorf("endpoint %q has no host", endpoint)
	}
	return u.Host, u.Scheme == "https", nil
}

// Get implements Store.
func (s *S3) Get(ctx context.Context, key string) ([]byte, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, s.translate(err, key)
	}
	defer obj.Close()

	data, err := io.ReadAll(obj)
	if err != nil {
		// A missing object surfaces here rather than at GetObject: the client
		// is lazy and does not talk to the server until the body is read.
		// Checking only the GetObject error would report every absent key as a
		// read failure instead of a miss.
		return nil, s.translate(err, key)
	}
	return data, nil
}

// Put implements Store.
func (s *S3) Put(ctx context.Context, key string, data []byte) error {
	_, err := s.client.PutObject(ctx, s.bucket, key,
		bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{ContentType: contentType(key)})
	if err != nil {
		return fmt.Errorf("put %s: %w", key, err)
	}
	return nil
}

// Exists implements Store.
func (s *S3) Exists(ctx context.Context, key string) (bool, error) {
	_, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err == nil {
		return true, nil
	}
	if errors.Is(s.translate(err, key), ErrNotFound) {
		return false, nil
	}
	return false, err
}

// List implements Store.
func (s *S3) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	// Recursive: the store has no directories, so a non-recursive listing would
	// return synthetic prefixes rather than the objects a caller asked for.
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{
		Prefix: prefix, Recursive: true,
	}) {
		if obj.Err != nil {
			return nil, fmt.Errorf("list %s: %w", prefix, obj.Err)
		}
		keys = append(keys, obj.Key)
	}
	sort.Strings(keys)
	return keys, nil
}

// Delete implements Store.
func (s *S3) Delete(ctx context.Context, key string) error {
	err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
	if err != nil && !errors.Is(s.translate(err, key), ErrNotFound) {
		return fmt.Errorf("delete %s: %w", key, err)
	}
	return nil
}

// translate turns an S3 error into ErrNotFound where that is what it means, so
// callers can use one check regardless of which store is behind the interface.
func (s *S3) translate(err error, key string) error {
	if err == nil {
		return nil
	}
	resp := minio.ToErrorResponse(err)
	switch resp.Code {
	case "NoSuchKey", "NoSuchBucket", "NotFound":
		return ErrNotFound
	}
	return fmt.Errorf("object %s: %w", key, err)
}

// contentType labels objects so a bucket browser shows something sensible.
// Nothing in this system reads it back.
func contentType(key string) string {
	switch {
	case strings.HasSuffix(key, ".parquet"):
		return "application/vnd.apache.parquet"
	case strings.HasSuffix(key, ".json"):
		return "application/json"
	case strings.HasSuffix(key, ".json.gz"):
		return "application/gzip"
	default:
		return "application/octet-stream"
	}
}
