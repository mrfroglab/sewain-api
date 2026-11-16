package media

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
)

const (
	localFileMode = 0o640
	localDirMode  = 0o750
	// localMetaDir holds one small file per object with its content type. Its name starts with "." so it
	// can never collide with a valid key.
	localMetaDir = ".meta"
)

// LocalStorage keeps objects as files below one directory (demo/staging). All access goes through
// os.Root, so even a symlink planted inside the directory cannot make it read or write outside.
type LocalStorage struct {
	root *os.Root
}

var _ Storage = (*LocalStorage)(nil)

// NewLocalStorage opens (creating if needed) dir as the storage root.
func NewLocalStorage(dir string) (*LocalStorage, error) {
	if err := os.MkdirAll(dir, localDirMode); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &LocalStorage{root: root}, nil
}

// Close releases the root directory handle.
func (s *LocalStorage) Close() error { return s.root.Close() }

// Put writes data atomically and write-once: the bytes go to a temp file that is fsynced and then
// hard-linked to the final name. link(2) fails if the name exists, so concurrent Puts of one key cannot
// overwrite each other and readers never observe a partial file.
func (s *LocalStorage) Put(ctx context.Context, key string, data []byte, contentType string) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if dir := path.Dir(key); dir != "." {
		if err := s.root.MkdirAll(dir, localDirMode); err != nil {
			return err
		}
	}
	tmp, err := s.writeTemp(path.Dir(key), data)
	if err != nil {
		return err
	}
	defer func() { _ = s.root.Remove(tmp) }() // after a link the data lives on under key
	if err := s.root.Link(tmp, key); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return ErrExists
		}
		return err
	}
	// The winner of the link records the content type. A missing meta file falls back to sniffing.
	if contentType != "" {
		metaDir := path.Join(localMetaDir, path.Dir(key))
		if err := s.root.MkdirAll(metaDir, localDirMode); err == nil {
			if mt, err := s.writeTemp(metaDir, []byte(contentType)); err == nil {
				if err := s.root.Rename(mt, path.Join(localMetaDir, key)); err != nil {
					_ = s.root.Remove(mt)
				}
			}
		}
	}
	return nil
}

func (s *LocalStorage) writeTemp(dir string, data []byte) (string, error) {
	var rnd [8]byte
	_, _ = rand.Read(rnd[:])
	name := path.Join(dir, ".tmp-"+hex.EncodeToString(rnd[:]))
	f, err := s.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, localFileMode)
	if err != nil {
		return "", err
	}
	_, werr := f.Write(data)
	if werr == nil {
		werr = f.Chmod(localFileMode) // umask-independent
	}
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = s.root.Remove(name)
		return "", werr
	}
	return name, nil
}

func (s *LocalStorage) Get(ctx context.Context, key string) ([]byte, string, error) {
	if err := ValidateKey(key); err != nil {
		return nil, "", err
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	data, err := s.root.ReadFile(key)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", err
	}
	ct := ""
	if b, err := s.root.ReadFile(path.Join(localMetaDir, key)); err == nil {
		ct = strings.TrimSpace(string(b))
	}
	if ct == "" {
		ct = http.DetectContentType(data)
	}
	return data, ct, nil
}

func (s *LocalStorage) Delete(ctx context.Context, key string) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.root.Remove(key); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := s.root.Remove(path.Join(localMetaDir, key)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func (s *LocalStorage) Exists(ctx context.Context, key string) (bool, error) {
	if err := ValidateKey(key); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	fi, err := s.root.Stat(key)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return fi.Mode().IsRegular(), nil
}
