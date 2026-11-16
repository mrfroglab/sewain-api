package service_test

import (
	"sync"
	"testing"

	"github.com/mrfroglab/sewain-api/internal/service"
)

// changeLog records the changes the service publishes (what the realtime hub fans out).
type changeLog struct {
	mu  sync.Mutex
	all []service.Change
}

func (c *changeLog) last() service.Change {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.all) == 0 {
		return service.Change{}
	}
	return c.all[len(c.all)-1]
}

func watchChanges(e *env) *changeLog {
	c := &changeLog{}
	e.svc.OnChange(func(ch service.Change) { c.mu.Lock(); c.all = append(c.all, ch); c.mu.Unlock() })
	return c
}

// Load test finding 7.3: a booking that starts or stops holding dates (listingBusy) or changes public reputation
// (publicStats) is a public change, so guests and other users get a "changed" event and the listing calendar does
// not go stale. A request, a decline or a chat message stays private to the parties.
func TestBookingStatusChangesArePublic(t *testing.T) {
	e := newEnv(t)
	log := watchChanges(e)
	l := e.listing("l-kompor-lapangan") // manual approval
	step := func(public bool, who, name string, a ...any) any {
		t.Helper()
		v := e.must(who, name, a...)
		if ch := log.last(); ch.Public != public || ch.Version == 0 {
			t.Fatalf("%s: change %+v, want public=%v", name, ch, public)
		}
		return v
	}
	id := step(false, "u-raka", "createBooking", "u-raka", bookingInput(l, e.today(5), e.today(6))).(string)
	step(false, "u-raka", "sendMessage", "u-raka", id, map[string]any{"text": "Halo kak"})
	step(true, l.OwnerID, "acceptBooking", l.OwnerID, id, nil) // listingBusy gains the dates
	busy := false
	for _, r := range e.state().ListingBusy[l.ID] {
		busy = busy || r.Start == e.today(5)
	}
	if !busy {
		t.Fatal("accepted booking not in listingBusy")
	}
	step(true, "u-raka", "cancelBooking", "u-raka", id, "Tidak jadi") // the dates are free again

	id2 := step(false, "u-raka", "createBooking", "u-raka", bookingInput(l, e.today(8), e.today(9))).(string)
	step(false, l.OwnerID, "declineBooking", l.OwnerID, id2, "Sedang dipakai")

	// Disputes count in publicStats: opening one on an active rental (ACTIVE → DISPUTED) is public too.
	step(true, "u-sinta", "openDispute", "u-sinta", "b-active", map[string]any{"caseType": "rusak", "description": "Sobek"})
}
