package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// OTPMessage is what a Sender delivers.
type OTPMessage struct {
	Phone string // E.164
	Code  string
}

// Sender delivers OTP codes. Implementations: DemoSender, EchoSender, ZenzivaSender.
type Sender interface {
	Name() string
	Send(ctx context.Context, msg OTPMessage) error
}

// FixedCoder is implemented by senders that require a fixed code instead of a random one (demo).
type FixedCoder interface{ FixedCode() string }

// CodeEchoer is implemented by senders whose code is returned in the API response (staging).
type CodeEchoer interface{ EchoCode() bool }

// DemoSender sends nothing; the code is always DemoOTPCode. Allowed only with DEMO_MODE.
type DemoSender struct{}

func (DemoSender) Name() string                           { return ProviderDemo }
func (DemoSender) Send(context.Context, OTPMessage) error { return nil }
func (DemoSender) FixedCode() string                      { return DemoOTPCode }

// EchoSender sends nothing; the random code is returned to the client as devCode. Staging only.
type EchoSender struct{}

func (EchoSender) Name() string                           { return ProviderEcho }
func (EchoSender) Send(context.Context, OTPMessage) error { return nil }
func (EchoSender) EchoCode() bool                         { return true }

// ZenzivaConfig configures a generic HTTP gateway adapter. Nothing about the real Zenziva API is assumed
// beyond "HTTP POST with credentials, a destination and a text"; every name is configurable and must be
// verified against a real account before go-live (ROADMAP §9 #2).
type ZenzivaConfig struct {
	WAURL        string            // ZENZIVA_WA_URL (required): WhatsApp endpoint
	SMSURL       string            // ZENZIVA_SMS_URL (optional): SMS fallback endpoint
	UserKey      string            // ZENZIVA_USERKEY
	PassKey      string            // ZENZIVA_PASSKEY
	Auth         string            // ZENZIVA_AUTH: body (default) | basic | bearer | none
	Body         string            // ZENZIVA_BODY: form (default) | json
	FieldUserKey string            // ZENZIVA_FIELD_USERKEY (default "userkey")
	FieldPassKey string            // ZENZIVA_FIELD_PASSKEY (default "passkey")
	FieldTo      string            // ZENZIVA_FIELD_TO (default "to")
	FieldMessage string            // ZENZIVA_FIELD_MESSAGE (default "message")
	PhoneFormat  string            // ZENZIVA_PHONE_FORMAT: local "08…" (default) | 62 "628…" | e164 "+628…"
	Message      string            // ZENZIVA_MESSAGE, "{code}" is replaced
	SuccessField string            // ZENZIVA_SUCCESS_FIELD: JSON field that must equal SuccessValue (optional)
	SuccessValue string            // ZENZIVA_SUCCESS_VALUE
	Extra        map[string]string // ZENZIVA_EXTRA "k=v;k2=v2": fixed extra fields (e.g. a sender/brand id)
	Timeout      time.Duration
}

const defaultOTPMessage = "Kode masuk Sewain: {code}. Berlaku 5 menit. Jangan berikan kode ini ke siapa pun, termasuk yang mengaku dari Sewain."

func zenzivaConfigFromEnv(get func(k, def string) string) (ZenzivaConfig, error) {
	z := ZenzivaConfig{
		WAURL:        get("ZENZIVA_WA_URL", ""),
		SMSURL:       get("ZENZIVA_SMS_URL", ""),
		UserKey:      get("ZENZIVA_USERKEY", ""),
		PassKey:      get("ZENZIVA_PASSKEY", ""),
		Auth:         get("ZENZIVA_AUTH", "body"),
		Body:         get("ZENZIVA_BODY", "form"),
		FieldUserKey: get("ZENZIVA_FIELD_USERKEY", "userkey"),
		FieldPassKey: get("ZENZIVA_FIELD_PASSKEY", "passkey"),
		FieldTo:      get("ZENZIVA_FIELD_TO", "to"),
		FieldMessage: get("ZENZIVA_FIELD_MESSAGE", "message"),
		PhoneFormat:  get("ZENZIVA_PHONE_FORMAT", "local"),
		Message:      get("ZENZIVA_MESSAGE", defaultOTPMessage),
		SuccessField: get("ZENZIVA_SUCCESS_FIELD", ""),
		SuccessValue: get("ZENZIVA_SUCCESS_VALUE", ""),
		Timeout:      10 * time.Second,
	}
	if extra := get("ZENZIVA_EXTRA", ""); extra != "" {
		z.Extra = map[string]string{}
		for _, kv := range strings.Split(extra, ";") {
			k, v, ok := strings.Cut(kv, "=")
			if !ok || strings.TrimSpace(k) == "" {
				return z, fmt.Errorf("auth: ZENZIVA_EXTRA entry %q must be key=value", kv)
			}
			z.Extra[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return z, z.validate()
}

func (z ZenzivaConfig) validate() error {
	if z.WAURL == "" {
		return errors.New("auth: ZENZIVA_WA_URL is required for OTP_PROVIDER=zenziva")
	}
	for _, u := range []string{z.WAURL, z.SMSURL} {
		if u == "" {
			continue
		}
		p, err := url.Parse(u)
		if err != nil || (p.Scheme != "https" && p.Scheme != "http") || p.Host == "" {
			return fmt.Errorf("auth: invalid Zenziva URL %q", u)
		}
	}
	switch z.Auth {
	case "body", "basic", "bearer", "none":
	default:
		return fmt.Errorf("auth: ZENZIVA_AUTH must be body|basic|bearer|none, got %q", z.Auth)
	}
	switch z.Body {
	case "form", "json":
	default:
		return fmt.Errorf("auth: ZENZIVA_BODY must be form|json, got %q", z.Body)
	}
	switch z.PhoneFormat {
	case "local", "62", "e164":
	default:
		return fmt.Errorf("auth: ZENZIVA_PHONE_FORMAT must be local|62|e164, got %q", z.PhoneFormat)
	}
	if z.Auth != "none" && z.UserKey == "" {
		return errors.New("auth: ZENZIVA_USERKEY is required")
	}
	if !strings.Contains(z.Message, "{code}") {
		return errors.New("auth: ZENZIVA_MESSAGE must contain {code}")
	}
	return nil
}

// ZenzivaSender posts the OTP to the WhatsApp endpoint and falls back to SMS when WhatsApp fails.
type ZenzivaSender struct {
	cfg    ZenzivaConfig
	client *http.Client
}

// NewZenzivaSender validates cfg. client may be nil (a client with cfg.Timeout is used).
func NewZenzivaSender(cfg ZenzivaConfig, client *http.Client) (*ZenzivaSender, error) {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.Message == "" {
		cfg.Message = defaultOTPMessage
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if client == nil {
		client = &http.Client{Timeout: cfg.Timeout}
	}
	return &ZenzivaSender{cfg: cfg, client: client}, nil
}

func (*ZenzivaSender) Name() string { return ProviderZenziva }

func (z *ZenzivaSender) Send(ctx context.Context, msg OTPMessage) error {
	waErr := z.post(ctx, z.cfg.WAURL, msg)
	if waErr == nil {
		return nil
	}
	if z.cfg.SMSURL == "" {
		return fmt.Errorf("zenziva wa: %w", waErr)
	}
	if smsErr := z.post(ctx, z.cfg.SMSURL, msg); smsErr != nil {
		return fmt.Errorf("zenziva wa: %v; sms: %w", waErr, smsErr)
	}
	return nil
}

func (z *ZenzivaSender) phone(e164 string) string {
	switch z.cfg.PhoneFormat {
	case "e164":
		return e164
	case "62":
		return strings.TrimPrefix(e164, "+")
	default:
		return "0" + strings.TrimPrefix(e164, "+62")
	}
}

func (z *ZenzivaSender) post(ctx context.Context, endpoint string, msg OTPMessage) error {
	fields := map[string]string{}
	for k, v := range z.cfg.Extra {
		fields[k] = v
	}
	fields[z.cfg.FieldTo] = z.phone(msg.Phone)
	fields[z.cfg.FieldMessage] = strings.ReplaceAll(z.cfg.Message, "{code}", msg.Code)
	if z.cfg.Auth == "body" {
		fields[z.cfg.FieldUserKey] = z.cfg.UserKey
		if z.cfg.PassKey != "" {
			fields[z.cfg.FieldPassKey] = z.cfg.PassKey
		}
	}
	var body io.Reader
	var ctype string
	if z.cfg.Body == "json" {
		b, _ := json.Marshal(fields)
		body, ctype = bytes.NewReader(b), "application/json"
	} else {
		form := url.Values{}
		for k, v := range fields {
			form.Set(k, v)
		}
		body, ctype = strings.NewReader(form.Encode()), "application/x-www-form-urlencoded"
	}
	// endpoint is ZENZIVA_WA_URL/ZENZIVA_SMS_URL from the operator's environment (validated http(s) URLs),
	// never from a request.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body) // #nosec G704 -- operator-configured URL
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", ctype)
	req.Header.Set("Accept", "application/json")
	switch z.cfg.Auth {
	case "basic":
		req.SetBasicAuth(z.cfg.UserKey, z.cfg.PassKey)
	case "bearer":
		req.Header.Set("Authorization", "Bearer "+z.cfg.UserKey)
	}
	resp, err := z.client.Do(req) // #nosec G704 -- operator-configured URL, see above
	if err != nil {
		return err // url.Error carries the endpoint but never the credentials (they are in the body/header)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("http %d", resp.StatusCode)
	}
	if z.cfg.SuccessField == "" {
		return nil
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return fmt.Errorf("unexpected response (not JSON)")
	}
	if got := fmt.Sprint(parsed[z.cfg.SuccessField]); got != z.cfg.SuccessValue {
		return fmt.Errorf("gateway reported %s=%q", z.cfg.SuccessField, got)
	}
	return nil
}
