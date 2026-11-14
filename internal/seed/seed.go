// Package seed provides the demo dataset (PRD §7.2: ±40 listings around UGM, demo accounts, one booking in every
// status). seed.json is exported from the web app's seed (sewain-web/src/lib/data/seed.ts) at a fixed base time;
// Build shifts every timestamp and date so the scenario is "live" relative to now (24h expiry, overdue, etc.).
package seed

import (
	_ "embed"
	"encoding/json"
	"math"
	"time"

	"github.com/mrfroglab/sewain-api/internal/domain"
)

//go:embed seed.json
var raw []byte

var timeKeys = map[string]bool{
	"createdAt": true, "expiresAt": true, "acceptedAt": true, "declinedAt": true, "cancelledAt": true, "activeAt": true,
	"returnedAt": true, "completedAt": true, "resolvedAt": true, "renterAgreedAt": true, "providerAgreedAt": true, "paidAt": true,
	"submittedAt": true, "at": true, "serverTimestamp": true, "verifiedAt": true, "reviewedAt": true, "joinedAt": true,
	"updatedAt": true, "l1Deadline": true, "providerReturnedAt": true, "renterConfirmedAt": true, "autoCompleteAt": true,
}

var dateKeys = map[string]bool{"startDate": true, "endDate": true, "neededFrom": true, "neededUntil": true}

func shift(v any, ms int64, days int) any {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			switch {
			case timeKeys[k]:
				if f, ok := val.(float64); ok {
					x[k] = f + float64(ms)
					continue
				}
			case dateKeys[k]:
				if s, ok := val.(string); ok {
					x[k] = domain.AddDays(s, days)
					continue
				}
			}
			x[k] = shift(val, ms, days)
		}
	case []any:
		for i := range x {
			x[i] = shift(x[i], ms, days)
		}
	}
	return v
}

// Build returns a fresh demo dataset anchored at now (epoch ms).
func Build(now int64) (*domain.Data, error) {
	var file struct {
		Base int64          `json:"base"`
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, err
	}
	day := func(ms int64) time.Time {
		t := time.UnixMilli(ms).In(domain.WIB)
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, domain.WIB)
	}
	days := int(math.Round(day(now).Sub(day(file.Base)).Hours() / 24))
	ms := now - file.Base
	shift(file.Data, ms, days)
	b, _ := json.Marshal(file.Data)
	var d domain.Data
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, err
	}
	d.Seeded, d.SeedBase, d.ClockOffset = true, now, 0
	return &d, nil
}
