package realtime

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func testDB(t *testing.T) (string, *pgxpool.Pool) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return url, pool
}

func TestFanOutAcrossInstancesDeliversOnce(t *testing.T) {
	dbURL, pool := testDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	h1, srv1 := startHub(t, Options{})
	h2, srv2 := startHub(t, Options{})
	o1, o2 := NewInstanceID(), NewInstanceID()
	for _, x := range []struct {
		h *Hub
		o string
	}{{h1, o1}, {h2, o2}} {
		ready := make(chan struct{}, 1)
		l := &Listener{ConnString: dbURL, Hub: x.h, Origin: x.o, OnListen: func() { ready <- struct{}{} }}
		go func() { _ = l.Run(ctx) }()
		select {
		case <-ready:
		case <-time.After(10 * time.Second):
			t.Fatal("listener did not start")
		}
	}
	p1 := &Publisher{DB: pool, Hub: h1, Origin: o1}

	a1 := mustDial(t, srv1, "u=u-a") // user A has a tab on each instance
	a2 := mustDial(t, srv2, "u=u-a")
	b2 := mustDial(t, srv2, "u=u-b")
	guest2 := mustDial(t, srv2, "")

	if err := p1.Publish(ctx, Event{Version: 1, Users: []string{"u-a"}}); err != nil {
		t.Fatal(err)
	}
	// Sentinel through NOTIFY without an origin, so both instances receive it via their listener. NOTIFY order
	// is preserved, so a duplicate of v1 on either instance would show up before v2.
	sentinel := func(v int64) {
		t.Helper()
		if err := notify(ctx, pool, encodeEnvelope("", Event{Version: v, Public: true})); err != nil {
			t.Fatal(err)
		}
	}
	sentinel(2)
	a1.expect(1, 2)
	a2.expect(1, 2)
	b2.expect(2)
	guest2.expect(2)

	// PublishTx: nothing on rollback, exactly once everywhere (origin included) on commit.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := p1.PublishTx(ctx, tx, Event{Version: 3, Users: []string{"u-a"}}); err != nil {
		t.Fatal(err)
	}
	_ = tx.Rollback(ctx)
	tx, _ = pool.Begin(ctx)
	if err := p1.PublishTx(ctx, tx, Event{Version: 4, Users: []string{"u-a"}}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	sentinel(5)
	a1.expect(4, 5)
	a2.expect(4, 5)
	b2.expect(5)
	guest2.expect(5)

	// An event too large for NOTIFY degrades to a Public refetch on the other instance.
	users := make([]string, 2000)
	for i := range users {
		users[i] = "u-many-" + strings.Repeat("x", 3)
	}
	users[0] = "u-a"
	if err := p1.Publish(ctx, Event{Version: 6, Users: users}); err != nil {
		t.Fatal(err)
	}
	sentinel(7)
	a1.expect(6, 7) // local delivery keeps the precise list
	a2.expect(6, 7)
	b2.expect(6, 7)
	guest2.expect(6, 7)
}

func TestEncodeEnvelopeFallsBackWhenTooLarge(t *testing.T) {
	users := make([]string, 1000)
	for i := range users {
		users[i] = "u-0123456789"
	}
	b := encodeEnvelope("x", Event{Version: 9, Users: users})
	if len(b) > MaxPayload || !strings.Contains(string(b), `"public":true`) || strings.Contains(string(b), "u-0123") {
		t.Fatalf("compact payload = %s", b)
	}
}

func TestListenerReconnectsAndTriggersRefetch(t *testing.T) {
	dbURL, pool := testDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h, srv := startHub(t, Options{})
	ready := make(chan struct{}, 4)
	l := &Listener{ConnString: dbURL + "&application_name=sewain-listener-reconnect-test", Hub: h, Origin: NewInstanceID(),
		MaxBackoff: 200 * time.Millisecond, OnListen: func() { ready <- struct{}{} }}
	go func() { _ = l.Run(ctx) }()
	<-ready
	c := mustDial(t, srv, "")

	if err := notify(ctx, pool, encodeEnvelope("", Event{Version: 10, Public: true})); err != nil {
		t.Fatal(err)
	}
	c.expect(10)
	if _, err := pool.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		WHERE application_name = 'sewain-listener-reconnect-test'`); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("listener did not reconnect")
	}
	c.expect(10) // catch-up refetch after the gap
	if err := notify(ctx, pool, encodeEnvelope("", Event{Version: 11, Public: true})); err != nil {
		t.Fatal(err)
	}
	c.expect(11)
}
