// Package media stores and serves photos: a write-once Storage (local disk or S3-compatible such as
// Cloudflare R2), upload validation, HMAC-signed photo URLs with a per-stage access policy, a display
// watermark, and the pure planning half of the retention job.
//
// The package has no database access. The core owns the photo/booking rows and plugs in through
// PhotoLookup (serving) and plain structs (policy, retention).
package media

import (
	"context"
	"errors"
	"strings"
)

// Storage is a write-once object store. The auth worker uses the same method set for encrypted KYC files.
type Storage interface {
	// Put stores data under key. It returns ErrExists if the key is already taken: objects are never
	// overwritten (FR-P4).
	Put(ctx context.Context, key string, data []byte, contentType string) error
	// Get returns the object and the content type it was stored with. Missing keys return ErrNotFound.
	Get(ctx context.Context, key string) (data []byte, contentType string, err error)
	// Delete removes the object. Deleting a missing key is not an error. Only the retention job deletes.
	Delete(ctx context.Context, key string) error
	// Exists reports whether key holds an object.
	Exists(ctx context.Context, key string) (bool, error)
}

var (
	// ErrExists is returned by Put when the key already holds an object.
	ErrExists = errors.New("media: object already exists")
	// ErrNotFound is returned by Get (and PhotoLookup implementations) for a missing object or photo.
	ErrNotFound = errors.New("media: object not found")
	// ErrInvalidKey is returned for keys that are empty, absolute, or try to escape the storage root.
	ErrInvalidKey = errors.New("media: invalid object key")
)

// MaxKeyLen bounds object keys (S3 allows 1024 bytes; we stay well below).
const MaxKeyLen = 512

// ValidateKey accepts keys made of "/"-separated segments of [A-Za-z0-9._-]. Segments may not be empty,
// "." or "..", or start with "." (hidden files are reserved for storage metadata). This rejects absolute
// paths, backslashes, NUL and every form of path traversal before any backend sees the key.
func ValidateKey(key string) error {
	if key == "" || len(key) > MaxKeyLen {
		return ErrInvalidKey
	}
	for seg := range strings.SplitSeq(key, "/") {
		if seg == "" || seg[0] == '.' {
			return ErrInvalidKey
		}
		for i := 0; i < len(seg); i++ {
			c := seg[i]
			if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '.' && c != '_' && c != '-' {
				return ErrInvalidKey
			}
		}
	}
	return nil
}

// PhotoKey is the object key for an original photo: "photos/<id><ext>", ext from Info.Ext.
func PhotoKey(photoID, ext string) string { return "photos/" + photoID + ext }

// WatermarkKey is the derived key that caches the watermarked display copy of a photo.
func WatermarkKey(photoID string) string { return "wm/" + photoID + ".jpg" }
