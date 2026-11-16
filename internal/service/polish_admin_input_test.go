package service_test

import (
	"strings"
	"testing"
)

// Polish (review wave 2, L1): admin actions on a finished entity are refused instead of reopening or deciding it twice.
func TestAdminActionsOnFinalEntities(t *testing.T) {
	e := newEnv(t)
	want := func(msg, prefix string) {
		t.Helper()
		if !strings.HasPrefix(msg, prefix) {
			t.Errorf("got %q, want %q…", msg, prefix)
		}
	}
	// d-actioncam is already "selesai": no new recommendation (it used to go back to mediation) and no second outcome.
	want(e.reject("u-admin", "adminRecommend", "u-admin", "d-actioncam", map[string]any{"text": "Ganti 100rb"}), "Sengketa sudah selesai")
	want(e.reject("u-admin", "resolveDispute", "u-admin", "d-actioncam", "Hasil lain"), "Sengketa sudah selesai")
	e.must("u-admin", "resolveDispute", "u-admin", "d-lensa", "Penyewa mengganti Rp200.000.")
	want(e.reject("u-admin", "resolveDispute", "u-admin", "d-lensa", "Hasil kedua"), "Sengketa sudah selesai")

	// Reports are decided once.
	want(e.reject("u-admin", "resolveReport", "u-admin", "rp-3", "ditolak", "Ubah keputusan"), "Laporan ini sudah ditinjau")
	e.must("u-admin", "resolveReport", "u-admin", "rp-1", "ditindak", "Listing diturunkan.")
	want(e.reject("u-admin", "resolveReport", "u-admin", "rp-1", "ditolak", "Berubah pikiran"), "Laporan ini sudah ditinjau")

	// Verification requests (simulated/manual path) are decided once.
	want(e.reject("u-admin", "reviewVerification", "u-admin", "v-u-raka", false, "Foto buram"), "Pengajuan ini sudah diputuskan")
	e.must("u-admin", "reviewVerification", "u-admin", "v-u-gilang", true, nil)
	want(e.reject("u-admin", "reviewVerification", "u-admin", "v-u-gilang", false, "Foto buram"), "Pengajuan ini sudah diputuskan")
	if u, _ := e.svc.User(e.ctx, "u-gilang"); u.Verification.Status != "terverifikasi" {
		t.Fatalf("second decision changed the user: %s", u.Verification.Status)
	}
}

// Polish (L1): the commission minimum may not exceed the maximum, and no amount may be negative.
func TestUpdateCommissionBounds(t *testing.T) {
	e := newEnv(t)
	for name, patch := range map[string]map[string]any{
		"min > max":    {"min": 60000, "max": 50000},
		"negative min": {"min": -1},
		"negative max": {"max": -5000},
		"negative fee": {"flat": -1},
		"giant max":    {"max": int64(9_000_000_000_000_000_000)},
	} {
		if _, err := e.try("u-admin", "updateCommission", "u-admin", patch); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	e.must("u-admin", "updateCommission", "u-admin", map[string]any{"min": 0, "max": 50000})
	e.must("u-admin", "updateCommission", "u-admin", map[string]any{"min": 5000, "max": nil})
	if c := e.state().Commission; c.Min == nil || *c.Min != 5000 || c.Max != nil {
		t.Fatalf("commission: %+v", c)
	}
}

// Polish (L2): every free text outside listings has a length limit, refused with an Indonesian message.
func TestFreeTextLimits(t *testing.T) {
	e := newEnv(t)
	long := func(n int) string { return strings.Repeat("é", n) } // runes, not bytes
	l := e.listing("l-kompor-lapangan")
	booking := bookingInput(l, e.today(2), e.today(3))
	booking["note"] = long(1001)
	disputeID := e.must("u-sinta", "openDispute", "u-sinta", "b-active", map[string]any{"caseType": "rusak", "description": "Tenda sobek"}).(string)
	cases := []struct {
		name, who, action string
		args              []any
	}{
		{"chat message", "u-sinta", "sendMessage", []any{"u-sinta", "b-active", map[string]any{"text": long(2001)}}},
		{"booking note", "u-raka", "createBooking", []any{"u-raka", booking}},
		{"decline reason", "u-tini", "declineBooking", []any{"u-tini", "b-requested-2", long(501)}},
		{"cancel reason", "u-raka", "cancelBooking", []any{"u-raka", "b-requested", long(501)}},
		{"reject at checkout", "u-raka", "rejectAtCheckout", []any{"u-raka", "b-accepted", long(1001)}},
		{"review comment", "u-raka", "submitReview", []any{"u-raka", "b-completed", 5, long(2001)}},
		{"report reason", "u-raka", "createReport", []any{"u-raka", map[string]any{"targetType": "listing", "targetId": "l-laptop", "reason": long(501)}}},
		{"report detail", "u-raka", "createReport", []any{"u-raka", map[string]any{"targetType": "listing", "targetId": "l-laptop", "reason": "Penipuan", "detail": long(2001)}}},
		{"dispute description", "u-raka", "openDispute", []any{"u-raka", "b-overdue", map[string]any{"caseType": "telat", "description": long(2001)}}},
		{"proposal note", "u-sinta", "proposeSolution", []any{"u-sinta", disputeID, map[string]any{"type": "tanpa-kompensasi", "note": long(1001)}}},
		{"admin recommendation", "u-admin", "adminRecommend", []any{"u-admin", "d-lensa", map[string]any{"text": long(2001)}}},
		{"dispute outcome", "u-admin", "resolveDispute", []any{"u-admin", "d-lensa", long(2001)}},
		{"strike reason", "u-admin", "addStrike", []any{"u-admin", "u-arif", map[string]any{"reason": long(501), "severity": "ringan"}}},
		{"status reason", "u-admin", "setUserStatus", []any{"u-admin", "u-arif", "suspend", long(501)}},
		{"wanted title", "u-raka", "createWanted", []any{"u-raka", map[string]any{"title": long(121), "description": "x", "areaId": "ugm", "radiusKm": 3}}},
		{"wanted description", "u-raka", "createWanted", []any{"u-raka", map[string]any{"title": "Tenda", "description": long(2001), "areaId": "ugm", "radiusKm": 3}}},
		{"bio", "u-raka", "updateUser", []any{"u-raka", map[string]any{"bio": long(501)}}},
		{"name", "u-raka", "updateUser", []any{"u-raka", map[string]any{"name": long(81)}}},
		{"store name", "u-tini", "updateUser", []any{"u-tini", map[string]any{"store": map[string]any{"name": long(81), "areaId": "ugm", "hours": "08.00–17.00"}}}},
	}
	for _, c := range cases {
		if msg := e.reject(c.who, c.action, c.args...); !strings.Contains(msg, "terlalu panjang (maks ") {
			t.Errorf("%s: %q", c.name, msg)
		}
	}
	// The limit counts characters: exactly the limit passes.
	e.must("u-sinta", "sendMessage", "u-sinta", "b-active", map[string]any{"text": long(2000)})
	e.must("u-raka", "updateUser", "u-raka", map[string]any{"bio": long(500)})

	// Wanted board amounts and report targets.
	if msg := e.reject("u-raka", "createWanted", "u-raka", map[string]any{"title": "Tenda", "description": "x", "areaId": "ugm", "radiusKm": 3, "budgetPerDay": -5000}); !strings.HasPrefix(msg, "Budget per hari") {
		t.Errorf("negative budget: %q", msg)
	}
	if msg := e.reject("u-raka", "createReport", "u-raka", map[string]any{"targetType": "listing", "targetId": "l-tidak-ada", "reason": "Penipuan"}); msg != "Yang kamu laporkan tidak ditemukan." {
		t.Errorf("missing target: %q", msg)
	}
	wid := e.must("u-raka", "createWanted", "u-raka", map[string]any{"title": "Tenda 4 orang", "description": "Buat camping", "areaId": "ugm", "radiusKm": 3}).(string)
	if msg := e.reject("u-bayu", "offerWanted", "u-bayu", wid, map[string]any{"message": long(1001)}); !strings.Contains(msg, "terlalu panjang") {
		t.Errorf("offer message: %q", msg)
	}
}

// Polish (L2, PRD §11.3): settlement amounts are bounded: a deposit cut by the deposit held, repairs and replacement by
// the item value in the agreement, nothing negative, and "tanpa kompensasi" without an amount.
func TestProposalAmountBounds(t *testing.T) {
	e := newEnv(t)
	// b-active: deposit Rp300.000, item value Rp1.200.000.
	id := e.must("u-sinta", "openDispute", "u-sinta", "b-active", map[string]any{"caseType": "rusak", "description": "Tenda sobek"}).(string)
	propose := func(kind string, amount any) error {
		_, err := e.try("u-sinta", "proposeSolution", "u-sinta", id, map[string]any{"type": kind, "amount": amount})
		return err
	}
	for _, c := range []struct {
		kind   string
		amount any
		ok     bool
	}{
		{"potong-deposit", 300001, false},
		{"potong-deposit", 300000, true},
		{"ganti-perbaikan", 1200001, false},
		{"ganti-perbaikan", 150000, true},
		{"ganti-nilai", int64(9_000_000_000_000_000_000), false},
		{"ganti-nilai", 1200000, true},
		{"ganti-nilai", -1, false},
		{"tanpa-kompensasi", 1, false},
		{"tanpa-kompensasi", nil, true},
	} {
		if err := propose(c.kind, c.amount); (err == nil) != c.ok {
			t.Errorf("%s %v: err=%v, want ok=%v", c.kind, c.amount, err, c.ok)
		}
	}
	// No deposit on b-overdue (identity collateral): a deposit cut is refused.
	id2 := e.must("u-tini", "openDispute", "u-tini", "b-overdue", map[string]any{"caseType": "telat", "description": "Belum kembali"}).(string)
	if msg := e.reject("u-tini", "proposeSolution", "u-tini", id2, map[string]any{"type": "potong-deposit", "amount": 1000}); msg != "Booking ini tidak memakai deposit." {
		t.Errorf("deposit cut without deposit: %q", msg)
	}
	// Admin recommendation (b-disputed: deposit Rp1.000.000, item value Rp2.500.000).
	if msg := e.reject("u-admin", "adminRecommend", "u-admin", "d-lensa", map[string]any{"text": "Ganti", "amount": 2500001}); !strings.HasPrefix(msg, "Nominal maksimal") {
		t.Errorf("recommendation above item value: %q", msg)
	}
	e.must("u-admin", "adminRecommend", "u-admin", "d-lensa", map[string]any{"text": "Ganti lensa", "amount": 2500000})
}
