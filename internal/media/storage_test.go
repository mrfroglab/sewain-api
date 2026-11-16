package media

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
)

// storageContract runs the behavior every Storage must have.
func storageContract(t *testing.T, st Storage) {
	ctx := context.Background()
	jpg := makeJPEG(t, 40, 40)

	t.Run("put get exists delete", func(t *testing.T) {
		key := "photos/p-roundtrip.jpg"
		if ok, err := st.Exists(ctx, key); err != nil || ok {
			t.Fatalf("Exists before put = %v, %v", ok, err)
		}
		if _, _, err := st.Get(ctx, key); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Get missing = %v, want ErrNotFound", err)
		}
		if err := st.Put(ctx, key, jpg, "image/jpeg"); err != nil {
			t.Fatal(err)
		}
		data, ct, err := st.Get(ctx, key)
		if err != nil || !bytes.Equal(data, jpg) || ct != "image/jpeg" {
			t.Fatalf("Get = %d bytes, %q, %v", len(data), ct, err)
		}
		if ok, err := st.Exists(ctx, key); err != nil || !ok {
			t.Fatalf("Exists after put = %v, %v", ok, err)
		}
		if err := st.Delete(ctx, key); err != nil {
			t.Fatal(err)
		}
		if ok, _ := st.Exists(ctx, key); ok {
			t.Fatal("still exists after delete")
		}
		if err := st.Delete(ctx, key); err != nil {
			t.Fatalf("delete of missing key: %v", err)
		}
	})

	t.Run("write once", func(t *testing.T) {
		key := "photos/p-once.jpg"
		if err := st.Put(ctx, key, jpg, "image/jpeg"); err != nil {
			t.Fatal(err)
		}
		if err := st.Put(ctx, key, []byte("replacement"), "text/plain"); !errors.Is(err, ErrExists) {
			t.Fatalf("second Put = %v, want ErrExists", err)
		}
		data, ct, _ := st.Get(ctx, key)
		if !bytes.Equal(data, jpg) || ct != "image/jpeg" {
			t.Fatal("original was overwritten")
		}
	})

	t.Run("concurrent put one winner", func(t *testing.T) {
		key := "kyc/u-1/ktp.bin"
		var wg sync.WaitGroup
		var mu sync.Mutex
		wins, exists := 0, 0
		for i := range 8 {
			wg.Go(func() {
				err := st.Put(ctx, key, []byte(fmt.Sprintf("writer-%d", i)), "application/octet-stream")
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					wins++
				case errors.Is(err, ErrExists):
					exists++
				default:
					t.Errorf("Put: %v", err)
				}
			})
		}
		wg.Wait()
		if wins != 1 || exists != 7 {
			t.Fatalf("wins=%d exists=%d", wins, exists)
		}
	})

	t.Run("invalid keys", func(t *testing.T) {
		bad := []string{"", "../etc/passwd", "photos/../../x", "/abs/path", "a//b", "a/./b", "./a",
			".meta/photos/x", "photos/.hidden", `a\..\b`, "a\x00b", "foto saya.jpg", "photos/", strings.Repeat("a", MaxKeyLen+1)}
		for _, k := range bad {
			if err := st.Put(ctx, k, jpg, "image/jpeg"); !errors.Is(err, ErrInvalidKey) {
				t.Errorf("Put(%q) = %v, want ErrInvalidKey", k, err)
			}
			if _, _, err := st.Get(ctx, k); !errors.Is(err, ErrInvalidKey) {
				t.Errorf("Get(%q) = %v, want ErrInvalidKey", k, err)
			}
			if _, err := st.Exists(ctx, k); !errors.Is(err, ErrInvalidKey) {
				t.Errorf("Exists(%q) = %v, want ErrInvalidKey", k, err)
			}
			if err := st.Delete(ctx, k); !errors.Is(err, ErrInvalidKey) {
				t.Errorf("Delete(%q) = %v, want ErrInvalidKey", k, err)
			}
		}
	})
}

func TestLocalStorage(t *testing.T) {
	dir := t.TempDir()
	st, err := NewLocalStorage(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	storageContract(t, st)

	ctx := context.Background()
	if err := st.Put(ctx, "photos/p-perm.jpg", makeJPEG(t, 40, 40), "image/jpeg"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "photos", "p-perm.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %v, want 0640", fi.Mode().Perm())
	}
	// No temp files left behind.
	entries, _ := os.ReadDir(filepath.Join(dir, "photos"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Fatalf("temp file left: %s", e.Name())
		}
	}
}

func TestLocalStorageSymlinkEscape(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("rahasia"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "evil")); err != nil {
		t.Skip("symlinks unsupported:", err)
	}
	st, err := NewLocalStorage(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if data, _, err := st.Get(ctx, "evil/secret.txt"); err == nil {
		t.Fatalf("read outside the root: %q", data)
	}
	if err := st.Put(ctx, "evil/planted.txt", []byte("x"), "text/plain"); err == nil {
		t.Fatal("wrote outside the root")
	}
	if _, err := os.Stat(filepath.Join(outside, "planted.txt")); err == nil {
		t.Fatal("file planted outside the root")
	}
	if err := st.Delete(ctx, "evil/secret.txt"); err == nil {
		if _, statErr := os.Stat(secret); statErr != nil {
			t.Fatal("deleted a file outside the root")
		}
	}
}

func TestFromEnv(t *testing.T) {
	dir := t.TempDir()
	env := map[string]string{"MEDIA_BACKEND": "local", "UPLOAD_DIR": dir}
	st, err := storageFromEnv(func(k string) string { return env[k] }, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.(*LocalStorage); !ok {
		t.Fatalf("got %T", st)
	}
	env = map[string]string{"MEDIA_BACKEND": "s3", "R2_ACCOUNT_ID": "abc", "R2_BUCKET": "b", "R2_ACCESS_KEY_ID": "k", "R2_SECRET_ACCESS_KEY": "s"}
	st, err = storageFromEnv(func(k string) string { return env[k] }, false)
	if err != nil {
		t.Fatal(err)
	}
	if s3, ok := st.(*S3Storage); !ok || s3.client.EndpointURL().Host != "abc.r2.cloudflarestorage.com" || s3.client.EndpointURL().Scheme != "https" {
		t.Fatalf("got %T %v", st, st)
	}
	env["MEDIA_BACKEND"] = "s3"
	delete(env, "R2_BUCKET")
	if _, err := storageFromEnv(func(k string) string { return env[k] }, false); err == nil {
		t.Fatal("expected error without bucket")
	}
	env = map[string]string{"MEDIA_BACKEND": "ftp"}
	if _, err := storageFromEnv(func(k string) string { return env[k] }, false); err == nil {
		t.Fatal("expected error for unknown backend")
	}
}

// --- S3 against a throw-away MinIO -------------------------------------------------------------------

const defaultMinioImage = "pgsty/minio:RELEASE.2026-08-04T00-00-00Z"

// startMinio runs a disposable MinIO (name sewain-test-minio-<rand>, random port on 127.0.0.1) and
// removes it when the test ends. Skips when Docker is unavailable.
func startMinio(t *testing.T) S3Config {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available; S3 test skipped")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker daemon not reachable; S3 test skipped")
	}
	image := os.Getenv("MEDIA_TEST_MINIO_IMAGE")
	if image == "" {
		image = defaultMinioImage
	}
	var rnd [4]byte
	_, _ = rand.Read(rnd[:])
	name := "sewain-test-minio-" + hex.EncodeToString(rnd[:])
	user, pass := "sewaintest", "sewaintest-"+hex.EncodeToString(rnd[:])
	out, err := exec.Command("docker", "run", "-d", "--rm", "--name", name,
		"-p", "127.0.0.1::9000", "--memory", "512m", "--label", "sewain.test=1",
		"-e", "MINIO_ROOT_USER="+user, "-e", "MINIO_ROOT_PASSWORD="+pass,
		image, "server", "/data").CombinedOutput()
	if err != nil {
		t.Fatalf("docker run minio: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })

	portOut, err := exec.Command("docker", "port", name, "9000/tcp").Output()
	if err != nil {
		t.Fatalf("docker port: %v", err)
	}
	addr := strings.TrimSpace(strings.Split(string(portOut), "\n")[0])
	deadline := time.Now().Add(60 * time.Second)
	for {
		res, err := http.Get("http://" + addr + "/minio/health/ready")
		if err == nil {
			res.Body.Close()
			if res.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			logs, _ := exec.Command("docker", "logs", name).CombinedOutput()
			t.Fatalf("minio not ready: %v\n%s", err, logs)
		}
		time.Sleep(300 * time.Millisecond)
	}
	return S3Config{Endpoint: addr, Bucket: "sewain-test", Region: "auto", AccessKeyID: user, SecretAccessKey: pass}
}

func TestS3StorageMinio(t *testing.T) {
	cfg := startMinio(t)
	st, err := NewS3Storage(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := st.client.MakeBucket(ctx, cfg.Bucket, minio.MakeBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := st.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	storageContract(t, st)

	// The conditional PUT alone (no Exists pre-check) must also refuse an overwrite.
	if err := st.Put(ctx, "photos/p-cond.jpg", []byte("a"), "image/jpeg"); err != nil {
		t.Fatal(err)
	}
	raw := &S3Storage{client: st.client, bucket: st.bucket}
	if err := raw.putConditional(ctx, "photos/p-cond.jpg", []byte("b"), "image/jpeg"); !errors.Is(err, ErrExists) {
		t.Fatalf("conditional put = %v, want ErrExists", err)
	}

	// Prefix keeps environments apart in one bucket.
	cfg.Prefix = "staging"
	pst, err := NewS3Storage(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := pst.Exists(ctx, "photos/p-cond.jpg"); ok {
		t.Fatal("prefix not applied")
	}
}
