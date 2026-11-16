package notify

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSMTP is a tiny SMTP server: EHLO, STARTTLS, AUTH PLAIN, MAIL, RCPT, DATA, QUIT.
// Recipients starting with "bounce" are rejected with 550.
type fakeSMTP struct {
	t       *testing.T
	ln      net.Listener
	tlsConf *tls.Config
	pool    *x509.CertPool
	user    string
	pass    string

	mu       sync.Mutex
	messages []smtpMessage
}

type smtpMessage struct {
	from, to string
	usedTLS  bool
	authed   bool
	data     string
}

func newFakeSMTP(t *testing.T) *fakeSMTP {
	t.Helper()
	// Borrow httptest's self-signed certificate (valid for 127.0.0.1).
	hs := httptest.NewUnstartedServer(http.NotFoundHandler())
	hs.StartTLS()
	cert := hs.TLS.Certificates[0]
	pool := x509.NewCertPool()
	pool.AddCert(hs.Certificate())
	hs.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{t: t, ln: ln, tlsConf: &tls.Config{Certificates: []tls.Certificate{cert}}, pool: pool, user: "sewain", pass: "rahasia"}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.session(c)
		}
	}()
	return f
}

func (f *fakeSMTP) msgs() []smtpMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]smtpMessage(nil), f.messages...)
}

func (f *fakeSMTP) port() string { return strings.Split(f.ln.Addr().String(), ":")[1] }

func (f *fakeSMTP) session(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	r, w := bufio.NewReader(c), bufio.NewWriter(c)
	say := func(s string) { _, _ = w.WriteString(s + "\r\n"); _ = w.Flush() }
	var m smtpMessage
	say("220 fake.smtp ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		cmd := strings.ToUpper(strings.SplitN(line, " ", 2)[0])
		switch cmd {
		case "EHLO", "HELO":
			if m.usedTLS {
				say("250-fake.smtp\r\n250 AUTH PLAIN")
			} else {
				say("250-fake.smtp\r\n250 STARTTLS")
			}
		case "STARTTLS":
			say("220 go ahead")
			tc := tls.Server(c, f.tlsConf)
			if err := tc.Handshake(); err != nil {
				return
			}
			c = tc
			r, w = bufio.NewReader(tc), bufio.NewWriter(tc)
			m.usedTLS = true
		case "AUTH":
			parts := strings.Fields(line)
			raw, _ := base64.StdEncoding.DecodeString(parts[len(parts)-1])
			if string(raw) == "\x00"+f.user+"\x00"+f.pass {
				m.authed = true
				say("235 ok")
			} else {
				say("535 bad credentials")
			}
		case "MAIL":
			m.from = strings.Trim(strings.TrimPrefix(line, "MAIL FROM:"), "<>")
			say("250 ok")
		case "RCPT":
			to := strings.Trim(strings.TrimPrefix(line, "RCPT TO:"), "<>")
			if strings.HasPrefix(to, "bounce") {
				say("550 no such user")
				continue
			}
			m.to = to
			say("250 ok")
		case "DATA":
			say("354 end with .")
			var sb strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				sb.WriteString(strings.TrimPrefix(l, "."))
			}
			m.data = sb.String()
			f.mu.Lock()
			f.messages = append(f.messages, m)
			f.mu.Unlock()
			say("250 queued")
		case "QUIT":
			say("221 bye")
			return
		default:
			say("250 ok")
		}
	}
}

// parseEmail returns the decoded subject, text and HTML parts of a raw message.
func parseEmail(t *testing.T, raw string) (hdr mail.Header, text, html string) {
	t.Helper()
	msg, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	_, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	mr := multipart.NewReader(msg.Body, params["boundary"])
	for {
		p, err := mr.NextPart() // decodes quoted-printable
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(p)
		if strings.HasPrefix(p.Header.Get("Content-Type"), "text/html") {
			html = string(b)
		} else {
			text = string(b)
		}
	}
	return msg.Header, text, html
}

var emailPayload = Payload{
	Title: "Sengketa naik ke mediasi", Body: "Sengketa booking SW-1042 belum selesai dalam 48 jam.\nAdmin akan membantu.",
	Href: "/sewaan/b-1042", URL: "https://sewain.test/sewaan/b-1042", Kind: "sengketa", Category: CategorySengketa, To: "Raka <raka@contoh.id>",
}

func TestSMTPMailerWithStartTLS(t *testing.T) {
	f := newFakeSMTP(t)
	m := &SMTPMailer{Host: "127.0.0.1", Port: f.port(), Username: f.user, Password: f.pass, From: "Sewain <noreply@sewain.test>",
		TLSConfig: &tls.Config{RootCAs: f.pool}}
	s := &EmailSender{Mailer: m, PublicURL: "https://sewain.test"}
	if err := s.Send(context.Background(), Job{ID: "nob-1", Payload: emailPayload}); err != nil {
		t.Fatal(err)
	}
	if len(f.msgs()) != 1 {
		t.Fatalf("got %d messages", len(f.msgs()))
	}
	got := f.msgs()[0]
	if !got.usedTLS || !got.authed || got.from != "noreply@sewain.test" || got.to != "raka@contoh.id" {
		t.Fatalf("session = %+v", got)
	}
	hdr, text, html := parseEmail(t, got.data)
	subject, _ := new(mime.WordDecoder).DecodeHeader(hdr.Get("Subject"))
	if subject != "Sengketa naik ke mediasi" {
		t.Errorf("subject %q", subject)
	}
	if !strings.Contains(hdr.Get("Message-ID"), "nob-1@sewain.test") {
		t.Errorf("Message-ID %q", hdr.Get("Message-ID"))
	}
	for _, want := range []string{"Sengketa booking SW-1042", "https://sewain.test/sewaan/b-1042", "https://sewain.test/profil/notifikasi"} {
		if !strings.Contains(text, want) {
			t.Errorf("text part lacks %q:\n%s", want, text)
		}
	}
	for _, want := range []string{`href="https://sewain.test/sewaan/b-1042"`, "Buka di Sewain", `href="https://sewain.test/profil/notifikasi"`, "Atur notifikasi", `lang="id"`} {
		if !strings.Contains(html, want) {
			t.Errorf("html part lacks %q", want)
		}
	}

	// Rejected recipient → permanent.
	p := emailPayload
	p.To = "bounce@contoh.id"
	if err := s.Send(context.Background(), Job{ID: "nob-2", Payload: p}); !IsPermanent(err) {
		t.Errorf("550 recipient: err=%v, want permanent", err)
	}
}

func TestSMTPMailerRequiresStartTLS(t *testing.T) {
	f := newFakeSMTP(t)
	m := &SMTPMailer{Host: "127.0.0.1", Port: f.port(), From: "noreply@sewain.test", TLSConfig: &tls.Config{RootCAs: x509.NewCertPool()}}
	err := m.Send(context.Background(), Message{To: "a@contoh.id", Subject: "x", Text: "x", HTML: "x"})
	if err == nil || IsPermanent(err) {
		t.Fatalf("untrusted certificate: err=%v, want retryable failure", err)
	}
	if len(f.msgs()) != 0 {
		t.Fatal("message sent over an unverified connection")
	}
}

func TestResendMailer(t *testing.T) {
	var mu sync.Mutex
	var got []map[string]any
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/emails" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer re_test_key" || r.Header.Get("Idempotency-Key") != "nob-7" {
			t.Errorf("headers %v", r.Header)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		got = append(got, body)
		mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"id":"49a3999c-0ce1-4ea6-ab68-afcd6dc2e794"}`))
	}))
	defer srv.Close()
	m := &ResendMailer{APIKey: "re_test_key", From: "Sewain <noreply@sewain.test>", BaseURL: srv.URL}
	s := &EmailSender{Mailer: m, PublicURL: "https://sewain.test"}
	job := Job{ID: "nob-7", Payload: emailPayload}
	if err := s.Send(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	b := got[0]
	if b["from"] != "Sewain <noreply@sewain.test>" || b["subject"] != "Sengketa naik ke mediasi" {
		t.Errorf("body %v", b)
	}
	if to, _ := b["to"].([]any); len(to) != 1 || to[0] != "Raka <raka@contoh.id>" {
		t.Errorf("to %v", b["to"])
	}
	if !strings.Contains(b["html"].(string), "https://sewain.test/sewaan/b-1042") || !strings.Contains(b["text"].(string), "Atur notifikasi") {
		t.Errorf("content %v", b)
	}

	status = http.StatusInternalServerError
	if err := s.Send(context.Background(), job); err == nil || IsPermanent(err) {
		t.Errorf("500: err=%v, want retryable", err)
	}
	status = http.StatusUnprocessableEntity
	if err := s.Send(context.Background(), job); !IsPermanent(err) {
		t.Errorf("422: err=%v, want permanent", err)
	}
}

func TestRenderEmailEscapesAndDefaults(t *testing.T) {
	e := RenderEmail(Payload{Title: "Halo\r\nBcc: x@y.z", Body: `<script>alert(1)</script>`, Href: "javascript:alert(1)"}, "https://sewain.test", "")
	if strings.ContainsAny(e.Subject, "\r\n") {
		t.Errorf("subject keeps newlines: %q", e.Subject)
	}
	if strings.Contains(e.HTML, "<script>") {
		t.Error("body not escaped")
	}
	if strings.Contains(e.HTML, `href="javascript:`) {
		t.Error("unsafe href kept")
	}
}
