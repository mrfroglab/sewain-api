package app_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/mrfroglab/sewain-api/internal/app/apptest"
	"github.com/mrfroglab/sewain-api/internal/auth"
)

// newStudents creates n accounts with bearer sessions directly (signing 300 people up through OTP from one IP is
// exactly what the auth profile is there to slow down).
func newStudents(t *testing.T, a *apptest.API, n int) []*apptest.Session {
	t.Helper()
	ctx := context.Background()
	out := make([]*apptest.Session, n)
	for i := range n {
		tx, err := a.App.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		u, _, err := a.App.Svc.CreateUser(ctx, tx, fmt.Sprintf("+6281277%06d", i), fmt.Sprintf("Mahasiswa %d", i))
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		tok, _, err := a.App.Auth.CreateSession(ctx, u.ID, auth.RequestMeta{IP: "127.0.0.1", UserAgent: "nat-test"})
		if err != nil {
			t.Fatal(err)
		}
		out[i] = &apptest.Session{Token: tok, UserID: u.ID}
	}
	return out
}

// TestCampusNATDefaults: 300 students behind one campus IP, each with a normal minute of use (snapshot, 5
// actions, 3 photo uploads), all fired at once against the default rate limits. Every request succeeds.
func TestCampusNATDefaults(t *testing.T) {
	a := apptest.New(t, apptest.Options{RealRateLimits: true})
	students := newStudents(t, a, 300)

	var mu sync.Mutex
	var limited []string
	work := make(chan *apptest.Session)
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for s := range work {
				resps := []apptest.Resp{a.Get(s, "/api/v1/state")}
				for range 5 {
					resps = append(resps, a.Act(s, "markNotificationsRead", s.UserID, []string{}))
				}
				for i := range 3 {
					resps = append(resps, a.Upload(s, "referensi", "", apptest.PNG(t, uint8(i))))
				}
				for i, r := range resps {
					if r.Status != http.StatusOK {
						mu.Lock()
						limited = append(limited, fmt.Sprintf("%s request %d: %d %s", s.UserID, i, r.Status, r.Raw))
						mu.Unlock()
					}
				}
			}
		}()
	}
	for _, s := range students {
		work <- s
	}
	close(work)
	wg.Wait()
	if len(limited) > 0 {
		t.Fatalf("%d requests failed behind the campus NAT, first: %s", len(limited), limited[0])
	}
}

// TestSingleIPFloodStillLimited: one guest IP hammering the API is cut off at the configured public limit.
func TestSingleIPFloodStillLimited(t *testing.T) {
	a := apptest.New(t, apptest.Options{RealRateLimits: true, Env: map[string]string{"RATE_PUBLIC_PER_MIN": "60", "RATE_PUBLIC_BURST": "30"}})
	for i := range 30 {
		if r := a.Get(nil, "/api/v1/config"); r.Status != 200 {
			t.Fatalf("request %d: %d", i, r.Status)
		}
	}
	r := a.Get(nil, "/api/v1/config")
	if r.Status != http.StatusTooManyRequests || r.Header.Get("Retry-After") == "" || r.Header.Get("X-RateLimit-Limit") != "30" {
		t.Fatalf("request past the burst: %d %v", r.Status, r.Header)
	}
	// A signed-in student behind the same IP has their own bucket.
	s := newStudents(t, a, 1)[0]
	if r := a.Get(s, "/api/v1/state"); r.Status != 200 {
		t.Fatalf("signed-in user behind the flooding IP: %d %s", r.Status, r.Raw)
	}
}

// TestDemoLoginNotThrottled: DEMO_MODE persona logins use the generous demo profile, not auth: 50 logins a minute
// from one IP all succeed and leave the OTP (auth) bucket untouched.
func TestDemoLoginNotThrottled(t *testing.T) {
	a := apptest.New(t, apptest.Options{Demo: true, RealRateLimits: true})
	for i := range 50 {
		r := a.Do(nil, apptest.Req{Method: "POST", Path: "/api/v1/auth/demo-login", JSON: map[string]any{"userId": "u-raka"}})
		if r.Status != 200 {
			t.Fatalf("demo login %d: %d %s", i, r.Status, r.Raw)
		}
	}
	r := a.Do(nil, apptest.Req{Method: "POST", Path: "/api/v1/auth/otp/request", JSON: map[string]any{"phone": "081277700001"}})
	if r.Status != 200 || r.Header.Get("X-RateLimit-Remaining") != "19" {
		t.Fatalf("otp after demo logins: %d remaining %q %s", r.Status, r.Header.Get("X-RateLimit-Remaining"), r.Raw)
	}
}

// TestOTPDailyCapEndToEnd: OTP_MAX_SENDS_PER_DAY refuses further codes politely and the admins get exactly one
// in-app notification for the day.
func TestOTPDailyCapEndToEnd(t *testing.T) {
	a := apptest.New(t, apptest.Options{Env: map[string]string{"OTP_MAX_SENDS_PER_DAY": "3"}})
	ctx := context.Background()
	admin := a.OTPLogin("081277799900") // send 1
	if _, err := a.App.Pool.Exec(ctx, `UPDATE users SET role = 'admin' WHERE id = $1`, admin.UserID); err != nil {
		t.Fatal(err)
	}
	for i := range 2 { // sends 2 and 3
		if r := a.Do(nil, apptest.Req{Method: "POST", Path: "/api/v1/auth/otp/request", JSON: map[string]any{"phone": fmt.Sprintf("08127779000%d", i)}}); r.Status != 200 {
			t.Fatalf("otp %d: %d %s", i, r.Status, r.Raw)
		}
	}
	for i := range 3 {
		r := a.Do(nil, apptest.Req{Method: "POST", Path: "/api/v1/auth/otp/request", JSON: map[string]any{"phone": fmt.Sprintf("08127779100%d", i)}})
		msg, _ := r.Body["error"].(string)
		if r.Status != http.StatusServiceUnavailable || r.Body["code"] != "otp_daily_cap" || !strings.Contains(msg, "Coba lagi besok") ||
			r.Header.Get("Retry-After") == "" {
			t.Fatalf("over the cap %d: %d %v %s", i, r.Status, r.Header, r.Raw)
		}
	}
	var n int
	if err := a.App.Pool.QueryRow(ctx, `SELECT count(*) FROM notifications WHERE user_id = $1 AND kind = 'keamanan' AND title = 'Batas harian OTP tercapai'`,
		admin.UserID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("admin notifications = %d, want 1", n)
	}
}
