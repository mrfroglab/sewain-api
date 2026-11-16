package notify

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strings"
	"time"
)

// Message is one email.
type Message struct {
	ID      string // stable id (outbox row id), used for Message-ID and provider idempotency
	To      string
	Subject string
	Text    string
	HTML    string
}

// Mailer sends email.
type Mailer interface {
	Send(ctx context.Context, m Message) error
}

/* --------------------------------------------------------------- SMTP */

// SMTP security modes.
const (
	SMTPStartTLS = "starttls" // plain connection upgraded with STARTTLS (port 587); refuses servers without it
	SMTPTLS      = "tls"      // implicit TLS (port 465)
	SMTPNone     = "none"     // no TLS: local relays/dev only; credentials are then refused by net/smtp
)

// SMTPMailer sends through an SMTP relay with net/smtp.
type SMTPMailer struct {
	Host     string
	Port     string // default 587
	Username string // empty: no AUTH
	Password string
	From     string // "Sewain <noreply@sewain.me>"
	Security string // starttls (default) | tls | none
	// TLSConfig overrides the TLS settings (tests inject a private CA). ServerName defaults to Host.
	TLSConfig *tls.Config
	Timeout   time.Duration // whole session (default 30s)
	HelloName string        // EHLO name (default "localhost")
}

func (m *SMTPMailer) tlsConfig() *tls.Config {
	c := &tls.Config{MinVersion: tls.VersionTLS12}
	if m.TLSConfig != nil {
		c = m.TLSConfig.Clone()
	}
	if c.ServerName == "" {
		c.ServerName = m.Host
	}
	return c
}

func (m *SMTPMailer) Send(ctx context.Context, msg Message) error {
	from, err := mail.ParseAddress(m.From)
	if err != nil {
		return Permanent(fmt.Errorf("MAIL_FROM: %w", err))
	}
	to, err := mail.ParseAddress(msg.To)
	if err != nil {
		return Permanent(fmt.Errorf("recipient: %w", err))
	}
	raw, err := buildMIME(from, to, msg)
	if err != nil {
		return Permanent(err)
	}
	port := m.Port
	if port == "" {
		port = "587"
	}
	timeout := m.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	dialer := &net.Dialer{Deadline: deadline}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(m.Host, port))
	if err != nil {
		return err
	}
	if m.Security == SMTPTLS {
		conn = tls.Client(conn, m.tlsConfig())
	}
	_ = conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()

	c, err := smtp.NewClient(conn, m.Host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	hello := m.HelloName
	if hello == "" {
		hello = "localhost"
	}
	if err := c.Hello(hello); err != nil {
		return err
	}
	if m.Security == "" || m.Security == SMTPStartTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errors.New("smtp: server does not offer STARTTLS")
		}
		if err := c.StartTLS(m.tlsConfig()); err != nil {
			return err
		}
	}
	if m.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", m.Username, m.Password, m.Host)); err != nil {
			return err
		}
	}
	if err := c.Mail(from.Address); err != nil {
		return err
	}
	if err := c.Rcpt(to.Address); err != nil {
		var te *textproto.Error
		if errors.As(err, &te) && te.Code >= 500 {
			return Permanent(err) // mailbox does not exist / rejected
		}
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(raw); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

func randomBoundary() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "sewain-" + hex.EncodeToString(b)
}

// buildMIME renders a multipart/alternative message (text + HTML), UTF-8, quoted-printable.
func buildMIME(from, to *mail.Address, msg Message) ([]byte, error) {
	var buf bytes.Buffer
	h := func(k, v string) { fmt.Fprintf(&buf, "%s: %s\r\n", k, v) }
	h("From", from.String())
	h("To", to.String())
	h("Subject", mime.QEncoding.Encode("utf-8", msg.Subject))
	h("Date", time.Now().Format(time.RFC1123Z))
	domain := "sewain.local"
	if i := strings.LastIndex(from.Address, "@"); i >= 0 {
		domain = from.Address[i+1:]
	}
	id := msg.ID
	if id == "" {
		id = randomBoundary()
	}
	h("Message-ID", "<"+id+"@"+domain+">")
	h("MIME-Version", "1.0")
	mw := multipart.NewWriter(&buf)
	if err := mw.SetBoundary(randomBoundary()); err != nil {
		return nil, err
	}
	h("Content-Type", "multipart/alternative; boundary="+mw.Boundary())
	buf.WriteString("\r\n")
	for _, part := range []struct{ ct, body string }{{"text/plain; charset=utf-8", msg.Text}, {"text/html; charset=utf-8", msg.HTML}} {
		pw, err := mw.CreatePart(textproto.MIMEHeader{"Content-Type": {part.ct}, "Content-Transfer-Encoding": {"quoted-printable"}})
		if err != nil {
			return nil, err
		}
		qp := quotedprintable.NewWriter(pw)
		if _, err := qp.Write([]byte(part.body)); err != nil {
			return nil, err
		}
		if err := qp.Close(); err != nil {
			return nil, err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

/* ------------------------------------------------------------- Resend */

// ResendMailer sends through the Resend HTTP API (POST /emails).
type ResendMailer struct {
	APIKey     string
	From       string
	BaseURL    string // default https://api.resend.com
	HTTPClient *http.Client
}

func (m *ResendMailer) Send(ctx context.Context, msg Message) error {
	base := m.BaseURL
	if base == "" {
		base = "https://api.resend.com"
	}
	body, _ := json.Marshal(map[string]any{
		"from": m.From, "to": []string{msg.To}, "subject": msg.Subject, "html": msg.HTML, "text": msg.Text,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/emails", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+m.APIKey)
	req.Header.Set("Content-Type", "application/json")
	if msg.ID != "" {
		req.Header.Set("Idempotency-Key", msg.ID) // a retried outbox row is not sent twice
	}
	client := m.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return fmt.Errorf("resend: %d %s", resp.StatusCode, strings.TrimSpace(string(rb)))
	default:
		return Permanent(fmt.Errorf("resend: %d %s", resp.StatusCode, strings.TrimSpace(string(rb))))
	}
}

/* ---------------------------------------------------------------- log */

// LogMailer only logs (staging without a real provider). It never fails.
type LogMailer struct{ Logger *slog.Logger }

func (m LogMailer) Send(_ context.Context, msg Message) error {
	if m.Logger != nil {
		m.Logger.Info("notify: email (log only)", "id", msg.ID, "subject", msg.Subject)
	}
	return nil
}

/* -------------------------------------------------------------- sender */

// EmailSender renders a job with the email template and hands it to a Mailer.
type EmailSender struct {
	Mailer    Mailer
	PublicURL string
	PrefsPath string // web route of the preferences page (default DefaultPrefsPath)
}

func (s *EmailSender) Send(ctx context.Context, job Job) error {
	if job.Payload.To == "" {
		return Permanent(errors.New("no email address"))
	}
	e := RenderEmail(job.Payload, s.PublicURL, s.PrefsPath)
	return s.Mailer.Send(ctx, Message{ID: job.ID, To: job.Payload.To, Subject: e.Subject, Text: e.Text, HTML: e.HTML})
}

/* ------------------------------------------------------------ whatsapp */

// ErrWhatsAppDisabled is returned by the WhatsApp stub. Real delivery needs a paid BSP account (ROADMAP §9 #9).
var ErrWhatsAppDisabled = errors.New("whatsapp channel is not available")

// WhatsAppStub is the placeholder sender for the whatsapp channel. Enqueue never queues WhatsApp unless the
// channel is enabled in Config.Channels, which nothing does yet.
type WhatsAppStub struct{}

func (WhatsAppStub) Send(context.Context, Job) error { return Permanent(ErrWhatsAppDisabled) }
