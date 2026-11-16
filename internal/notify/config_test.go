package notify

import (
	"strings"
	"testing"
)

func envMap(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestConfigFromEnv(t *testing.T) {
	pub, priv, err := GenerateVAPIDKeys()
	if err != nil {
		t.Fatal(err)
	}
	c, err := ConfigFromEnv(envMap(map[string]string{}))
	if err != nil {
		t.Fatalf("empty env should be valid (everything off): %v", err)
	}
	if ch := c.Channels(); ch[ChannelWebPush] || ch[ChannelEmail] || ch[ChannelWhatsApp] {
		t.Fatalf("channels on with empty env: %v", ch)
	}

	c, err = ConfigFromEnv(envMap(map[string]string{
		"VAPID_PUBLIC_KEY": pub, "VAPID_PRIVATE_KEY": priv, "VAPID_SUBJECT": "mailto:ops@sewain.test",
		"MAIL_PROVIDER": "smtp", "SMTP_HOST": "smtp.contoh.id", "SMTP_PORT": "587", "MAIL_FROM": "Sewain <noreply@sewain.test>",
		"PUBLIC_URL": "https://sewain.test/",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if ch := c.Channels(); !ch[ChannelWebPush] || !ch[ChannelEmail] || ch[ChannelWhatsApp] {
		t.Fatalf("channels: %v", ch)
	}
	if c.PublicURL != "https://sewain.test" {
		t.Errorf("PUBLIC_URL not trimmed: %q", c.PublicURL)
	}
	s := c.Senders(nil, nil)
	if _, ok := s[ChannelEmail].(*EmailSender); !ok {
		t.Error("email sender missing")
	}
	if _, ok := s[ChannelWebPush].(*PushSender); !ok {
		t.Error("push sender missing")
	}

	for name, env := range map[string]map[string]string{
		"half vapid":       {"VAPID_PUBLIC_KEY": pub},
		"bad subject":      {"VAPID_PUBLIC_KEY": pub, "VAPID_PRIVATE_KEY": priv, "VAPID_SUBJECT": "ops@sewain.test"},
		"bad public key":   {"VAPID_PUBLIC_KEY": "abc", "VAPID_PRIVATE_KEY": priv, "VAPID_SUBJECT": "mailto:a@b.c"},
		"smtp no host":     {"MAIL_PROVIDER": "smtp", "MAIL_FROM": "a@b.c", "PUBLIC_URL": "https://x.y"},
		"resend no key":    {"MAIL_PROVIDER": "resend", "MAIL_FROM": "a@b.c", "PUBLIC_URL": "https://x.y"},
		"mail no from":     {"MAIL_PROVIDER": "log", "PUBLIC_URL": "https://x.y"},
		"mail no url":      {"MAIL_PROVIDER": "log", "MAIL_FROM": "a@b.c"},
		"unknown provider": {"MAIL_PROVIDER": "sendgrid"},
		"bad security":     {"MAIL_PROVIDER": "smtp", "SMTP_HOST": "h", "SMTP_SECURITY": "ssl", "MAIL_FROM": "a@b.c", "PUBLIC_URL": "https://x.y"},
		"relative url":     {"PUBLIC_URL": "sewain.test"},
	} {
		if _, err := ConfigFromEnv(envMap(env)); err == nil {
			t.Errorf("%s: accepted", name)
		} else if strings.Contains(err.Error(), "\n\n") {
			t.Errorf("%s: odd error %q", name, err)
		}
	}
}
