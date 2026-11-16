package media

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3Config configures an S3-compatible bucket (Cloudflare R2 in production, MinIO in tests).
type S3Config struct {
	Endpoint        string // host[:port] without scheme, e.g. "<account>.r2.cloudflarestorage.com"
	Bucket          string
	Region          string // "auto" for R2
	AccessKeyID     string
	SecretAccessKey string
	UseSSL          bool
	Prefix          string // optional key prefix inside the bucket, e.g. "staging/"
}

// S3Storage stores objects in an S3-compatible bucket.
type S3Storage struct {
	client *minio.Client
	bucket string
	prefix string
}

var _ Storage = (*S3Storage)(nil)

// NewS3Storage builds a client. It does not touch the network; call Ping to check credentials.
func NewS3Storage(cfg S3Config) (*S3Storage, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" || cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" {
		return nil, errors.New("media: S3 endpoint, bucket, access key id and secret are required")
	}
	if cfg.Region == "" {
		cfg.Region = "auto"
	}
	endpoint := strings.TrimPrefix(strings.TrimPrefix(cfg.Endpoint, "https://"), "http://")
	endpoint = strings.TrimSuffix(endpoint, "/")
	client, err := minio.New(endpoint, &minio.Options{
		Creds:        credentials.NewStaticV4(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		Secure:       cfg.UseSSL,
		Region:       cfg.Region,
		BucketLookup: minio.BucketLookupPath,
	})
	if err != nil {
		return nil, err
	}
	prefix := cfg.Prefix
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	if prefix != "" {
		if err := ValidateKey(strings.TrimSuffix(prefix, "/")); err != nil {
			return nil, err
		}
	}
	return &S3Storage{client: client, bucket: cfg.Bucket, prefix: prefix}, nil
}

// Ping checks that the bucket exists and the credentials can see it.
func (s *S3Storage) Ping(ctx context.Context) error {
	ok, err := s.client.BucketExists(ctx, s.bucket)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("media: bucket " + s.bucket + " does not exist")
	}
	return nil
}

func (s *S3Storage) objectName(key string) (string, error) {
	if err := ValidateKey(key); err != nil {
		return "", err
	}
	return s.prefix + key, nil
}

// Put uploads with "If-None-Match: *", so the bucket itself refuses to overwrite (R2 and MinIO honor
// conditional writes). The Exists pre-check only gives a cheaper error on the common path.
func (s *S3Storage) Put(ctx context.Context, key string, data []byte, contentType string) error {
	name, err := s.objectName(key)
	if err != nil {
		return err
	}
	if ok, err := s.Exists(ctx, key); err != nil {
		return err
	} else if ok {
		return ErrExists
	}
	return s.putConditional(ctx, name, data, contentType)
}

func (s *S3Storage) putConditional(ctx context.Context, name string, data []byte, contentType string) error {
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	opts := minio.PutObjectOptions{ContentType: contentType, DisableMultipart: true, SendContentMd5: true}
	opts.SetMatchETagExcept("*")
	_, err := s.client.PutObject(ctx, s.bucket, name, bytes.NewReader(data), int64(len(data)), opts)
	if err != nil {
		if r := minio.ToErrorResponse(err); r.StatusCode == http.StatusPreconditionFailed || r.Code == "PreconditionFailed" {
			return ErrExists
		}
		return err
	}
	return nil
}

func (s *S3Storage) Get(ctx context.Context, key string) ([]byte, string, error) {
	name, err := s.objectName(key)
	if err != nil {
		return nil, "", err
	}
	obj, err := s.client.GetObject(ctx, s.bucket, name, minio.GetObjectOptions{})
	if err != nil {
		return nil, "", mapS3Err(err)
	}
	defer obj.Close()
	data, err := io.ReadAll(obj)
	if err != nil {
		return nil, "", mapS3Err(err)
	}
	info, err := obj.Stat()
	if err != nil {
		return nil, "", mapS3Err(err)
	}
	return data, info.ContentType, nil
}

func (s *S3Storage) Delete(ctx context.Context, key string) error {
	name, err := s.objectName(key)
	if err != nil {
		return err
	}
	// S3 DeleteObject is idempotent: a missing key is not an error.
	return s.client.RemoveObject(ctx, s.bucket, name, minio.RemoveObjectOptions{})
}

func (s *S3Storage) Exists(ctx context.Context, key string) (bool, error) {
	name, err := s.objectName(key)
	if err != nil {
		return false, err
	}
	_, err = s.client.StatObject(ctx, s.bucket, name, minio.StatObjectOptions{})
	if err == nil {
		return true, nil
	}
	if errors.Is(mapS3Err(err), ErrNotFound) {
		return false, nil
	}
	return false, err
}

func mapS3Err(err error) error {
	r := minio.ToErrorResponse(err)
	if r.StatusCode == http.StatusNotFound || r.Code == "NoSuchKey" {
		return ErrNotFound
	}
	return err
}
