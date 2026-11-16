package notify

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
)

// Mail providers (MAIL_PROVIDER).
const (
	MailNone   = "none"
	MailSMTP   = "smtp"
	MailResend = "resend"
	MailLog    = "log"
)

// Config is the notify part of the environment. See docs/integration/notify.md.
type Config struct {
	VAPID        VAPID
	MailProvider string // none (default) | smtp | resend | log
	MailFrom     string
	SMTP         SMTPMailer
	ResendAPIKey string
	PublicURL    string
	PrefsPath    string
}

// ConfigFromEnv reads the notify settings. Pass os.Getenv.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	get := func(k string) string { return strings.TrimSpace(getenv(k)) }
	c := Config{
		VAPID:        VAPID{PublicKey: get("VAPID_PUBLIC_KEY"), PrivateKey: get("VAPID_PRIVATE_KEY"), Subject: get("VAPID_SUBJECT")},
		MailProvider: strings.ToLower(get("MAIL_PROVIDER")),
		MailFrom:     get("MAIL_FROM"),
		ResendAPIKey: get("RESEND_API_KEY"),
		PublicURL:    strings.TrimRight(get("PUBLIC_URL"), "/"),
		PrefsPath:    get("NOTIFY_PREFS_PATH"),
		SMTP: SMTPMailer{
			Host: get("SMTP_HOST"), Port: get("SMTP_PORT"), Username: get("SMTP_USERNAME"), Password: getenv("SMTP_PASSWORD"),
			Security: strings.ToLower(get("SMTP_SECURITY")),
		},
	}
	if c.MailProvider == "" {
		c.MailProvider = MailNone
	}
	c.SMTP.From = c.MailFrom
	var errs []error
	v := c.VAPID
	if (v.PublicKey != "" || v.PrivateKey != "" || v.Subject != "") && !v.Enabled() {
		errs = append(errs, errors.New("VAPID_PUBLIC_KEY, VAPID_PRIVATE_KEY and VAPID_SUBJECT must be set together"))
	}
	if v.Subject != "" && !strings.HasPrefix(v.Subject, "mailto:") && !strings.HasPrefix(v.Subject, "https://") {
		errs = append(errs, errors.New("VAPID_SUBJECT must start with mailto: or https://"))
	}
	if v.PublicKey != "" {
		if b, err := decodeKey(v.PublicKey); err != nil || len(b) != 65 {
			errs = append(errs, errors.New("VAPID_PUBLIC_KEY is not a base64url P-256 public key"))
		}
	}
	if c.PublicURL != "" {
		if u, err := url.Parse(c.PublicURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			errs = append(errs, errors.New("PUBLIC_URL must be an absolute http(s) URL"))
		}
	}
	switch c.MailProvider {
	case MailNone, MailLog:
	case MailSMTP:
		if c.SMTP.Host == "" {
			errs = append(errs, errors.New("SMTP_HOST is required when MAIL_PROVIDER=smtp"))
		}
		switch c.SMTP.Security {
		case "", SMTPStartTLS, SMTPTLS, SMTPNone:
		default:
			errs = append(errs, errors.New("SMTP_SECURITY must be starttls, tls or none"))
		}
	case MailResend:
		if c.ResendAPIKey == "" {
			errs = append(errs, errors.New("RESEND_API_KEY is required when MAIL_PROVIDER=resend"))
		}
	default:
		errs = append(errs, fmt.Errorf("MAIL_PROVIDER %q: use none, smtp, resend or log", c.MailProvider))
	}
	if c.MailProvider != MailNone {
		if c.MailFrom == "" {
			errs = append(errs, errors.New("MAIL_FROM is required when email is enabled"))
		}
		if c.PublicURL == "" {
			errs = append(errs, errors.New("PUBLIC_URL is required when email is enabled (links in emails)"))
		}
	}
	return c, errors.Join(errs...)
}

// Channels reports which outbox channels this server can deliver. WhatsApp stays off (paid provider needed).
func (c Config) Channels() map[Channel]bool {
	return map[Channel]bool{
		ChannelWebPush:  c.VAPID.Enabled(),
		ChannelEmail:    c.MailProvider != MailNone,
		ChannelWhatsApp: false,
	}
}

// Mailer builds the configured mailer, or nil when email is off.
func (c Config) Mailer(log *slog.Logger) Mailer {
	switch c.MailProvider {
	case MailSMTP:
		m := c.SMTP
		return &m
	case MailResend:
		return &ResendMailer{APIKey: c.ResendAPIKey, From: c.MailFrom}
	case MailLog:
		return LogMailer{Logger: log}
	}
	return nil
}

// Senders builds the dispatcher senders for the configured channels.
func (c Config) Senders(db DB, log *slog.Logger) map[Channel]Sender {
	s := map[Channel]Sender{ChannelWhatsApp: WhatsAppStub{}}
	if c.VAPID.Enabled() {
		s[ChannelWebPush] = &PushSender{DB: db, VAPID: c.VAPID}
	}
	if m := c.Mailer(log); m != nil {
		s[ChannelEmail] = &EmailSender{Mailer: m, PublicURL: c.PublicURL, PrefsPath: c.PrefsPath}
	}
	return s
}
