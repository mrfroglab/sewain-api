package service_test

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/mrfroglab/sewain-api/internal/domain"
	"github.com/mrfroglab/sewain-api/internal/service"
)

// TestConcurrentCreateBookingRespectsStock (F1.10): 24 parallel requests for the same dates on an auto-approve
// listing with stock 3 (Toko account) end with exactly 3 accepted bookings; the rest are told the item is taken.
func TestConcurrentCreateBookingRespectsStock(t *testing.T) {
	e := newEnv(t)
	l := e.listing("l-tenda-dome-4p") // u-bayu (toko), stock 3, approval otomatis, min rating 4.5
	if l.Stock != 3 || l.Approval != "otomatis" {
		t.Fatalf("seed changed: %+v", l)
	}
	renters := []string{"u-raka", "u-sinta"} // both rated ≥ 4.5 → auto-approved
	start, end := e.today(10), e.today(12)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, taken := 0, 0
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(who string) {
			defer wg.Done()
			_, err := e.svc.Dispatch(e.ctx, &service.Viewer{UserID: who, Role: "user"}, "createBooking", args(who, bookingInput(l, start, end)), "")
			mu.Lock()
			defer mu.Unlock()
			switch msg, _ := service.IsUserError(err); {
			case err == nil:
				ok++
			case msg == "Barang sudah dipesan di tanggal itu. Coba tanggal lain.":
				taken++
			default:
				t.Errorf("unexpected: %v", err)
			}
		}(renters[i%2])
	}
	wg.Wait()
	accepted := 0
	for _, b := range e.state().Bookings {
		if b.ListingID == l.ID && b.StartDate == start && b.Status == "ACCEPTED" {
			accepted++
		}
	}
	if accepted != 3 || ok != 3 || taken != 21 {
		t.Fatalf("stock 3: accepted=%d ok=%d taken=%d", accepted, ok, taken)
	}
}

// TestConcurrentAcceptRespectsStock (F1.10): 20 requests for the same dates on a stock-1 listing, accepted in
// parallel by the provider: only one is accepted.
func TestConcurrentAcceptRespectsStock(t *testing.T) {
	e := newEnv(t)
	l := e.listing("l-kamera-mirrorless") // u-dina, individual → stock 1, manual approval
	renters := []string{"u-raka", "u-sinta", "u-tini", "u-bayu", "u-fajar"}
	start, end := e.today(20), e.today(21)
	ids := []string{}
	for i := 0; i < 20; i++ {
		who := renters[i%len(renters)]
		id := e.must(who, "createBooking", who, bookingInput(l, start, end)).(string)
		ids = append(ids, id)
	}
	dina := &service.Viewer{UserID: "u-dina", Role: "user"}
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			_, err := e.svc.Dispatch(e.ctx, dina, "acceptBooking", args("u-dina", id), "")
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				ok++
			} else if _, rule := service.IsUserError(err); !rule {
				t.Errorf("unexpected: %v", err)
			}
		}(id)
	}
	wg.Wait()
	accepted := 0
	for _, b := range e.state().Bookings {
		if b.ListingID == l.ID && b.StartDate == start && b.Status == "ACCEPTED" {
			accepted++
		}
	}
	if ok != 1 || accepted != 1 {
		t.Fatalf("stock 1: ok=%d accepted=%d", ok, accepted)
	}
}

// TestIdempotency (F1.8): the same key replays the same response without doing the work twice.
func TestIdempotency(t *testing.T) {
	e := newEnv(t)
	l := e.listing("l-kompor-lapangan")
	raka := &service.Viewer{UserID: "u-raka", Role: "user"}
	a := args("u-raka", bookingInput(l, e.today(2), e.today(3)))
	body, _ := json.Marshal(map[string]any{"args": a})
	hash := service.RequestHash("createBooking", body)
	respond := func(res service.Result, err error) (int, json.RawMessage) {
		if err != nil {
			msg, _ := service.IsUserError(err)
			b, _ := json.Marshal(map[string]any{"ok": false, "error": msg})
			return 200, b
		}
		b, _ := json.Marshal(map[string]any{"ok": true, "value": res.Value, "version": res.Version})
		return 200, b
	}
	before := len(e.state().Bookings)

	// Parallel duplicates with one key.
	var wg sync.WaitGroup
	bodies := make([]string, 8)
	replays := make([]bool, 8)
	for i := range bodies {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out, replayed, err := e.svc.DispatchIdempotent(e.ctx, raka, "key-1", hash, "createBooking", a, "", respond)
			if err != nil {
				t.Error(err)
				return
			}
			bodies[i], replays[i] = string(out.Body), replayed
		}(i)
	}
	wg.Wait()
	fresh := 0
	for i := range bodies {
		if bodies[i] != bodies[0] {
			t.Fatalf("different responses: %s vs %s", bodies[i], bodies[0])
		}
		if !replays[i] {
			fresh++
		}
	}
	if fresh != 1 {
		t.Fatalf("exactly one execution expected, got %d", fresh)
	}
	if after := len(e.state().Bookings); after != before+1 {
		t.Fatalf("bookings %d → %d", before, after)
	}

	// Same key, different request → mismatch.
	other := args("u-raka", bookingInput(l, e.today(4), e.today(5)))
	ob, _ := json.Marshal(map[string]any{"args": other})
	if _, _, err := e.svc.DispatchIdempotent(e.ctx, raka, "key-1", service.RequestHash("createBooking", ob), "createBooking", other, "", respond); err != service.ErrIdempotencyMismatch {
		t.Fatalf("want mismatch, got %v", err)
	}

	// A rejected action is replayed with the same answer too.
	bad := args("u-raka", map[string]any{"listingId": "l-flagged-airsoft"})
	bb, _ := json.Marshal(map[string]any{"args": bad})
	h := service.RequestHash("createBooking", bb)
	first, _, err := e.svc.DispatchIdempotent(e.ctx, raka, "key-2", h, "createBooking", bad, "", respond)
	if err != nil {
		t.Fatal(err)
	}
	second, replayed, err := e.svc.DispatchIdempotent(e.ctx, raka, "key-2", h, "createBooking", bad, "", respond)
	if err != nil || !replayed || string(first.Body) != string(second.Body) {
		t.Fatalf("rejection replay: %s / %s (%v)", first.Body, second.Body, err)
	}
	if want := fmt.Sprintf(`{"error":%q,"ok":false}`, "Barang tidak tersedia."); string(first.Body) != want {
		t.Fatalf("body %s", first.Body)
	}
	_ = domain.Hour
}
