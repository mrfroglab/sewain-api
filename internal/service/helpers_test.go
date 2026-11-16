package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mrfroglab/sewain-api/internal/db/dbtest"
	"github.com/mrfroglab/sewain-api/internal/domain"
	"github.com/mrfroglab/sewain-api/internal/service"
)

// fakeClock is the injectable real clock (the demo clock adds its offset on top).
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

type env struct {
	t     *testing.T
	ctx   context.Context
	svc   *service.Service
	clock *fakeClock
}

// newEnv: fresh database with the demo seed at a fixed "now".
func newEnv(t *testing.T) *env {
	t.Helper()
	pool := dbtest.NewPool(t)
	clk := &fakeClock{t: time.Now()}
	svc := service.New(pool, service.Options{DemoMode: true, SnapshotScope: service.ScopeFull, Now: clk.Now})
	e := &env{t: t, ctx: context.Background(), svc: svc, clock: clk}
	if _, err := svc.ResetDemo(e.ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return e
}

func args(vs ...any) []json.RawMessage {
	out := make([]json.RawMessage, len(vs))
	for i, v := range vs {
		out[i], _ = json.Marshal(v)
	}
	return out
}

func (e *env) viewer(id string) *service.Viewer {
	e.t.Helper()
	u, err := e.svc.GetUserAuth(e.ctx, id)
	if err != nil || u == nil {
		e.t.Fatalf("viewer %s: %v", id, err)
	}
	return &service.Viewer{UserID: u.ID, Role: u.Role}
}

func (e *env) try(who, name string, a ...any) (any, error) {
	e.t.Helper()
	res, err := e.svc.Dispatch(e.ctx, e.viewer(who), name, args(a...), "")
	return res.Value, err
}

func (e *env) tryID(who, name, wantID string, a ...any) (any, error) {
	e.t.Helper()
	res, err := e.svc.Dispatch(e.ctx, e.viewer(who), name, args(a...), wantID)
	return res.Value, err
}

func (e *env) must(who, name string, a ...any) any {
	e.t.Helper()
	v, err := e.try(who, name, a...)
	if err != nil {
		e.t.Fatalf("%s by %s: %v", name, who, err)
	}
	return v
}

// reject asserts a rule violation and returns its message.
func (e *env) reject(who, name string, a ...any) string {
	e.t.Helper()
	_, err := e.try(who, name, a...)
	msg, ok := service.IsUserError(err)
	if !ok {
		e.t.Fatalf("%s by %s: want rule violation, got %v", name, who, err)
	}
	return msg
}

func (e *env) state() *domain.Data {
	e.t.Helper()
	s, err := e.svc.Snapshot(e.ctx, &service.Viewer{UserID: "u-admin", Role: "admin"})
	if err != nil {
		e.t.Fatal(err)
	}
	return s.Data
}

func (e *env) booking(id string) *domain.Booking {
	e.t.Helper()
	for _, b := range e.state().Bookings {
		if b.ID == id {
			return b
		}
	}
	return nil
}

func (e *env) listing(id string) *domain.Listing {
	e.t.Helper()
	for _, l := range e.state().Listings {
		if l.ID == id {
			return l
		}
	}
	e.t.Fatalf("listing %s not found", id)
	return nil
}

func (e *env) photos(n int, stage, uploader string, bookingID *string) []string {
	e.t.Helper()
	ids := []string{}
	for i := 0; i < n; i++ {
		p, err := e.svc.AddPhoto(e.ctx, service.NewPhoto{BookingID: bookingID, Stage: stage, UploaderID: uploader, Src: "/img/x.webp", Sha256: "00", Source: "kamera"})
		if err != nil {
			e.t.Fatal(err)
		}
		ids = append(ids, p.ID)
	}
	return ids
}

func (e *env) today(days int) string {
	now, err := e.svc.Now(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	return domain.AddDays(domain.IsoDate(now), days)
}

func isForbidden(err error) bool { return errors.Is(err, domain.ErrForbidden) }
