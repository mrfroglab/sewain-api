package media

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// FromEnv builds the Storage the application uses:
//
//	MEDIA_BACKEND=local (default)  UPLOAD_DIR (default ./data/uploads)
//	MEDIA_BACKEND=s3               S3_ENDPOINT | R2_ENDPOINT | R2_ACCOUNT_ID, S3_BUCKET | R2_BUCKET,
//	                               S3_ACCESS_KEY_ID | R2_ACCESS_KEY_ID, S3_SECRET_ACCESS_KEY | R2_SECRET_ACCESS_KEY,
//	                               S3_REGION (default auto), S3_USE_SSL (default true), S3_PREFIX
//
// The app credentials should not be allowed to delete (see docs/integration/media.md).
func FromEnv() (Storage, error) { return storageFromEnv(os.Getenv, false) }

// RetentionStorageFromEnv is FromEnv for the retention job. With the s3 backend it prefers
// S3_RETENTION_ACCESS_KEY_ID / S3_RETENTION_SECRET_ACCESS_KEY (the only credentials allowed to delete).
func RetentionStorageFromEnv() (Storage, error) { return storageFromEnv(os.Getenv, true) }

func storageFromEnv(getenv func(string) string, retention bool) (Storage, error) {
	first := func(keys ...string) string {
		for _, k := range keys {
			if v := strings.TrimSpace(getenv(k)); v != "" {
				return v
			}
		}
		return ""
	}
	switch backend := strings.ToLower(first("MEDIA_BACKEND")); backend {
	case "", "local":
		dir := first("UPLOAD_DIR")
		if dir == "" {
			dir = "./data/uploads"
		}
		return NewLocalStorage(dir)
	case "s3", "r2":
		cfg := S3Config{
			Endpoint:        first("S3_ENDPOINT", "R2_ENDPOINT"),
			Bucket:          first("S3_BUCKET", "R2_BUCKET"),
			Region:          first("S3_REGION", "R2_REGION"),
			AccessKeyID:     first("S3_ACCESS_KEY_ID", "R2_ACCESS_KEY_ID"),
			SecretAccessKey: first("S3_SECRET_ACCESS_KEY", "R2_SECRET_ACCESS_KEY"),
			UseSSL:          true,
			Prefix:          first("S3_PREFIX", "R2_PREFIX"),
		}
		if cfg.Endpoint == "" {
			if acct := first("R2_ACCOUNT_ID"); acct != "" {
				cfg.Endpoint = acct + ".r2.cloudflarestorage.com"
			}
		}
		if v := first("S3_USE_SSL"); v != "" {
			b, err := strconv.ParseBool(v)
			if err != nil {
				return nil, fmt.Errorf("media: S3_USE_SSL: %w", err)
			}
			cfg.UseSSL = b
		}
		if retention {
			id := first("S3_RETENTION_ACCESS_KEY_ID", "R2_RETENTION_ACCESS_KEY_ID")
			secret := first("S3_RETENTION_SECRET_ACCESS_KEY", "R2_RETENTION_SECRET_ACCESS_KEY")
			if id != "" && secret != "" {
				cfg.AccessKeyID, cfg.SecretAccessKey = id, secret
			}
		}
		return NewS3Storage(cfg)
	default:
		return nil, errors.New("media: MEDIA_BACKEND must be local or s3, got " + backend)
	}
}
