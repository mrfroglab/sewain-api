package payments

import (
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Payment modes (ROADMAP D12, F6.7).
const (
	ModeSimulated = "simulated" // demo: "lunas" is a button
	ModeOffline   = "offline"   // pilot: paid directly to the provider, outside Sewain
	ModeGateway   = "gateway"   // Phase 8: this package
)

// EnvConfig is everything read from the environment.
type EnvConfig struct {
	Mode    string
	Service Config
	Xendit  XenditConfig
	// FakeToken enables the fake provider (local sandbox demo only; refused in production).
	FakeToken string
}

// ConfigFromEnv reads PAYMENT_* and XENDIT_* variables. Outside gateway mode only Mode is meaningful.
func ConfigFromEnv(getenv func(string) string) (EnvConfig, error) {
	c := EnvConfig{Mode: getenv("PAYMENT_MODE")}
	if c.Mode == "" {
		c.Mode = ModeOffline
	}
	switch c.Mode {
	case ModeSimulated, ModeOffline:
		return c, nil
	case ModeGateway:
	default:
		return c, fmt.Errorf("payments: PAYMENT_MODE %q (simulated|offline|gateway)", c.Mode)
	}
	c.Service.Provider = getenv("PAYMENT_PROVIDER")
	if c.Service.Provider == "" {
		c.Service.Provider = "xendit"
	}
	c.Service.IntentTTL = 30 * time.Minute
	if v := getenv("PAYMENT_INTENT_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Minute {
			return c, fmt.Errorf("payments: PAYMENT_INTENT_TTL %q", v)
		}
		c.Service.IntentTTL = d
	}
	c.Service.MDRBasisPoints = 70
	if v := getenv("PAYMENT_MDR_BP"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 || n > 1000 {
			return c, fmt.Errorf("payments: PAYMENT_MDR_BP %q", v)
		}
		c.Service.MDRBasisPoints = n
	}
	c.Service.CommissionActive = getenv("PAYMENT_COMMISSION_ACTIVE") == "true"
	switch c.Service.Provider {
	case "xendit":
		c.Xendit = XenditConfig{
			BaseURL: getenv("XENDIT_BASE_URL"), SecretKey: getenv("XENDIT_SECRET_KEY"), CallbackToken: getenv("XENDIT_CALLBACK_TOKEN"),
			APIVersion: getenv("XENDIT_API_VERSION"), QRISChannelCode: getenv("XENDIT_QRIS_CHANNEL_CODE"),
			StatusPath: getenv("XENDIT_STATUS_PATH"), RefundPath: getenv("XENDIT_REFUND_PATH"), PayoutPath: getenv("XENDIT_PAYOUT_PATH"),
		}
		if c.Xendit.SecretKey == "" || c.Xendit.CallbackToken == "" {
			return c, errors.New("payments: XENDIT_SECRET_KEY and XENDIT_CALLBACK_TOKEN are required with PAYMENT_PROVIDER=xendit")
		}
	case "fake":
		if getenv("APP_ENV") == "production" {
			return c, errors.New("payments: PAYMENT_PROVIDER=fake is refused in production")
		}
		c.FakeToken = getenv("PAYMENT_FAKE_TOKEN")
		if c.FakeToken == "" {
			return c, errors.New("payments: PAYMENT_FAKE_TOKEN is required with PAYMENT_PROVIDER=fake")
		}
	default:
		return c, fmt.Errorf("payments: PAYMENT_PROVIDER %q (xendit|fake)", c.Service.Provider)
	}
	return c, nil
}

// Providers builds the configured provider.
func (c EnvConfig) Providers() ([]Provider, error) {
	switch c.Service.Provider {
	case "xendit":
		x, err := NewXendit(c.Xendit)
		if err != nil {
			return nil, err
		}
		return []Provider{x}, nil
	case "fake":
		return []Provider{NewFakeProvider(c.FakeToken)}, nil
	}
	return nil, fmt.Errorf("payments: no provider for mode %s", c.Mode)
}
