package notify

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// browserKeys is the client side of a push subscription (what PushManager.subscribe creates).
type browserKeys struct {
	priv *ecdh.PrivateKey
	auth []byte
}

func newBrowserKeys(t *testing.T) browserKeys {
	t.Helper()
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	_, _ = rand.Read(auth)
	return browserKeys{priv: priv, auth: auth}
}

func (k browserKeys) input(endpoint string) SubscriptionInput {
	var in SubscriptionInput
	in.Endpoint = endpoint
	in.Keys.P256dh = base64.RawURLEncoding.EncodeToString(k.priv.PublicKey().Bytes())
	in.Keys.Auth = base64.RawURLEncoding.EncodeToString(k.auth)
	return in
}

// decrypt reverses RFC 8291 (aes128gcm content encoding) as a browser would.
func (k browserKeys) decrypt(body []byte) ([]byte, error) {
	if len(body) < 21 {
		return nil, fmt.Errorf("short body")
	}
	salt := body[:16]
	idlen := int(body[20])
	asPub := body[21 : 21+idlen]
	ct := body[21+idlen:]
	peer, err := ecdh.P256().NewPublicKey(asPub)
	if err != nil {
		return nil, err
	}
	secret, err := k.priv.ECDH(peer)
	if err != nil {
		return nil, err
	}
	prkKey, _ := hkdf.Extract(sha256.New, secret, k.auth)
	keyInfo := "WebPush: info\x00" + string(k.priv.PublicKey().Bytes()) + string(asPub)
	ikm, _ := hkdf.Expand(sha256.New, prkKey, keyInfo, 32)
	prk, _ := hkdf.Extract(sha256.New, ikm, salt)
	cek, _ := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	nonce, _ := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	block, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(block)
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, err
	}
	i := bytes.LastIndexByte(plain, 2) // padding delimiter
	if i < 0 {
		return nil, fmt.Errorf("no padding delimiter")
	}
	return plain[:i], nil
}

// verifyVAPID checks "vapid t=<jwt>, k=<key>": key matches, ES256 signature valid, aud/sub/exp sane.
func verifyVAPID(header, wantKey, wantAud, wantSub string) error {
	rest, ok := strings.CutPrefix(header, "vapid ")
	if !ok {
		return fmt.Errorf("scheme: %q", header)
	}
	var tok, key string
	for _, part := range strings.Split(rest, ",") {
		part = strings.TrimSpace(part)
		if v, ok := strings.CutPrefix(part, "t="); ok {
			tok = v
		} else if v, ok := strings.CutPrefix(part, "k="); ok {
			key = v
		}
	}
	if key != wantKey {
		return fmt.Errorf("k=%q, want server key", key)
	}
	pubBytes, err := base64.RawURLEncoding.DecodeString(key)
	if err != nil {
		return err
	}
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), pubBytes)
	if err != nil {
		return err
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return fmt.Errorf("jwt parts: %d", len(parts))
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		return fmt.Errorf("signature encoding")
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(pub, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		return fmt.Errorf("bad signature")
	}
	hdr, _ := base64.RawURLEncoding.DecodeString(parts[0])
	if !strings.Contains(string(hdr), `"ES256"`) {
		return fmt.Errorf("alg: %s", hdr)
	}
	claimsJSON, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims struct {
		Aud string `json:"aud"`
		Sub string `json:"sub"`
		Exp int64  `json:"exp"`
	}
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		return err
	}
	if claims.Aud != wantAud || claims.Sub != wantSub {
		return fmt.Errorf("claims %+v", claims)
	}
	if exp := time.Unix(claims.Exp, 0); exp.Before(time.Now()) || exp.After(time.Now().Add(24*time.Hour)) {
		return fmt.Errorf("exp %v out of range", exp)
	}
	return nil
}

// fakePushService records deliveries; the path decides the response (/ok, /gone, /missing, /busy).
type fakePushService struct {
	t       *testing.T
	srv     *httptest.Server
	vapid   VAPID
	keys    map[string]browserKeys // path → subscriber keys
	mu      sync.Mutex
	got     map[string][]PushMessage
	headers map[string]http.Header
}

func newFakePushService(t *testing.T, vapid VAPID) *fakePushService {
	f := &fakePushService{t: t, vapid: vapid, keys: map[string]browserKeys{}, got: map[string][]PushMessage{}, headers: map[string]http.Header{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakePushService) serve(w http.ResponseWriter, r *http.Request) {
	if err := verifyVAPID(r.Header.Get("Authorization"), f.vapid.PublicKey, f.srv.URL, f.vapid.Subject); err != nil {
		f.t.Errorf("VAPID: %v", err)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if r.Header.Get("Content-Encoding") != "aes128gcm" || r.Header.Get("TTL") == "" {
		f.t.Errorf("headers: %v", r.Header)
	}
	body, _ := io.ReadAll(r.Body)
	switch {
	case strings.HasPrefix(r.URL.Path, "/gone"):
		w.WriteHeader(http.StatusGone)
		return
	case strings.HasPrefix(r.URL.Path, "/missing"):
		w.WriteHeader(http.StatusNotFound)
		return
	case strings.HasPrefix(r.URL.Path, "/busy"):
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	plain, err := f.keys[r.URL.Path].decrypt(body)
	if err != nil {
		f.t.Errorf("decrypt: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var m PushMessage
	if err := json.Unmarshal(plain, &m); err != nil {
		f.t.Errorf("payload %q: %v", plain, err)
	}
	f.mu.Lock()
	f.got[r.URL.Path] = append(f.got[r.URL.Path], m)
	f.headers[r.URL.Path] = r.Header.Clone()
	f.mu.Unlock()
	w.WriteHeader(http.StatusCreated)
}

func (f *fakePushService) subscribe(t *testing.T, db DB, userID, path string) {
	t.Helper()
	k := newBrowserKeys(t)
	f.keys[path] = k
	if err := SaveSubscription(context.Background(), db, userID, k.input(f.srv.URL+path), "test"); err != nil {
		t.Fatal(err)
	}
}

func testVAPID(t *testing.T) VAPID {
	t.Helper()
	pub, priv, err := GenerateVAPIDKeys()
	if err != nil {
		t.Fatal(err)
	}
	return VAPID{PublicKey: pub, PrivateKey: priv, Subject: "mailto:ops@sewain.test"}
}

func subscriptionCount(t *testing.T, db DB, userID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(context.Background(), `SELECT count(*) FROM push_subscriptions WHERE user_id = $1`, userID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPushSenderDeliversAndPrunesGoneSubscriptions(t *testing.T) {
	db := testPool(t)
	ctx := context.Background()
	vapid := testVAPID(t)
	f := newFakePushService(t, vapid)
	addUser(t, db, "u-a", "")
	f.subscribe(t, db, "u-a", "/ok/phone")
	f.subscribe(t, db, "u-a", "/gone/old-laptop")
	f.subscribe(t, db, "u-a", "/missing/old-tab")

	s := &PushSender{DB: db, VAPID: vapid}
	job := Job{ID: "nob-1", UserID: "u-a", Channel: ChannelWebPush, Payload: Payload{
		Title: "Request sewa baru", Body: "Raka mau sewa Kamera Fujifilm X-T200 tanggal 21–23 Nov.", Href: "/sewaan/b-1", Tag: "n-1", Important: true,
	}}
	if err := s.Send(ctx, job); err != nil {
		t.Fatalf("Send: %v", err)
	}
	got := f.got["/ok/phone"]
	want := PushMessage{Title: "Request sewa baru", Body: job.Payload.Body, Href: "/sewaan/b-1", Tag: "n-1"}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("delivered %+v, want %+v", got, want)
	}
	if u := f.headers["/ok/phone"].Get("Urgency"); u != "high" {
		t.Errorf("Urgency = %q, want high for important", u)
	}
	if n := subscriptionCount(t, db, "u-a"); n != 1 {
		t.Fatalf("subscriptions left = %d, want 1 (404/410 removed)", n)
	}
	var last *time.Time
	_ = db.QueryRow(ctx, `SELECT last_success_at FROM push_subscriptions WHERE user_id = 'u-a'`).Scan(&last)
	if last == nil {
		t.Error("last_success_at not set")
	}
}

func TestPushSenderFailureModes(t *testing.T) {
	db := testPool(t)
	ctx := context.Background()
	vapid := testVAPID(t)
	f := newFakePushService(t, vapid)
	addUser(t, db, "u-busy", "")
	addUser(t, db, "u-gone", "")
	addUser(t, db, "u-none", "")
	f.subscribe(t, db, "u-busy", "/busy/1")
	f.subscribe(t, db, "u-gone", "/gone/1")
	s := &PushSender{DB: db, VAPID: vapid}
	job := func(u string) Job { return Job{ID: "x", UserID: u, Payload: Payload{Title: "t", Body: "b"}} }

	if err := s.Send(ctx, job("u-busy")); err == nil || IsPermanent(err) {
		t.Errorf("503: err=%v, want retryable", err)
	}
	if err := s.Send(ctx, job("u-gone")); !IsPermanent(err) {
		t.Errorf("410 only: err=%v, want permanent", err)
	}
	if n := subscriptionCount(t, db, "u-gone"); n != 0 {
		t.Errorf("gone subscription kept")
	}
	if err := s.Send(ctx, job("u-none")); !IsPermanent(err) {
		t.Errorf("no subscriptions: err=%v, want permanent", err)
	}
}

func TestPushThroughOutboxEndToEnd(t *testing.T) {
	db := testPool(t)
	ctx := context.Background()
	vapid := testVAPID(t)
	f := newFakePushService(t, vapid)
	addUser(t, db, "u-a", "")
	f.subscribe(t, db, "u-a", "/ok/a")
	cfg := Config{VAPID: vapid, MailProvider: MailNone, PublicURL: "https://sewain.test"}
	e := &Enqueuer{Channels: cfg.Channels(), PublicURL: cfg.PublicURL, Now: func() time.Time { return at(12, 0).AddDate(-1, 0, 0) }} // past noon: due, outside quiet hours
	tx, _ := db.Begin(ctx)
	if _, err := e.Enqueue(ctx, tx, Notification{ID: "n-1", UserID: "u-a", Kind: "booking", Title: "Request disetujui", Body: "Yuk bayar.", Href: "/sewaan/b-9"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	d := &Dispatcher{DB: db, Senders: cfg.Senders(db, nil)}
	if n, err := d.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if got := f.got["/ok/a"]; len(got) != 1 || got[0].Href != "/sewaan/b-9" || got[0].Title != "Request disetujui" {
		t.Fatalf("delivered %+v", got)
	}
	if r := outboxRows(t, db, "n-1")[ChannelWebPush]; r.Status != "sent" {
		t.Fatalf("row %+v", r)
	}
}

func TestEncodePushMessageFitsLimit(t *testing.T) {
	long := strings.Repeat("Barang dikembalikan dalam kondisi baik. ", 400)
	b := EncodePushMessage(Payload{Title: strings.Repeat("Judul ", 60), Body: long, Tag: "n-1"})
	if len(b) > MaxPushPayload {
		t.Fatalf("payload %d bytes", len(b))
	}
	var m PushMessage
	if err := json.Unmarshal(b, &m); err != nil || m.Href != "/notifikasi" || !strings.HasSuffix(m.Body, "…") {
		t.Fatalf("message %+v err %v", m, err)
	}
}
