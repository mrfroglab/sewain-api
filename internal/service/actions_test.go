package service_test

import (
	"strings"
	"testing"

	"github.com/mrfroglab/sewain-api/internal/domain"
)

func bookingInput(l *domain.Listing, start, end string) map[string]any {
	return map[string]any{"listingId": l.ID, "startDate": start, "endDate": end, "handover": l.Handover[0], "collateralOptionId": l.CollateralOptions[0].ID}
}

func listingInput(title string) map[string]any {
	return map[string]any{
		"title": title, "category": "camping", "description": "Tenda bagus", "photos": []string{"/a.webp", "/b.webp", "/c.webp"},
		"pricePerDay": 20000, "itemValue": 300000, "collateralOptions": []map[string]any{{"id": "co-x1", "kind": "identitas", "cards": []string{"KTM"}}},
		"location": map[string]any{"lat": -7.77, "lng": 110.37, "areaId": "ugm"}, "radiusKm": 3, "handover": []string{"cod"},
		"rules": map[string]any{"lateFeePerDay": 10000, "lateFeeCapPercent": 100, "cancellation": "Gratis"},
	}
}

func TestAccountActions(t *testing.T) {
	e := newEnv(t)
	// registerUser needs no session.
	res, err := e.svc.Dispatch(e.ctx, nil, "registerUser", args(map[string]any{"name": " Budi Santoso ", "phone": "0812-9999-0000", "areaId": "pogung"}), "u-budi123456")
	if err != nil || res.Value != "u-budi123456" {
		t.Fatalf("register: %v %v", res.Value, err)
	}
	if _, err := e.svc.Dispatch(e.ctx, nil, "registerUser", args(map[string]any{"name": "X", "phone": "081299990000"}), ""); err == nil || err.Error() != "Nomor HP sudah terdaftar. Masuk pakai nomor itu." {
		t.Fatalf("duplicate phone: %v", err)
	}
	if _, err := e.svc.Dispatch(e.ctx, nil, "registerUser", args(map[string]any{"name": "X", "phone": "123"}), ""); err == nil || err.Error() != "Nomor HP tidak valid." {
		t.Fatalf("short phone: %v", err)
	}
	if _, err := e.svc.Dispatch(e.ctx, nil, "registerUser", args(map[string]any{"name": "Y", "phone": "081200001111", "email": "RAKA.pratama@mail.ugm.ac.id"}), ""); err == nil || err.Error() != "Email sudah dipakai akun lain." {
		t.Fatalf("duplicate email: %v", err)
	}
	u, _ := e.svc.User(e.ctx, "u-budi123456")
	if u.Name != "Budi Santoso" || u.Phone != "081299990000" || u.Verification.Status != "belum" {
		t.Fatalf("user %+v", u)
	}

	e.must("u-budi123456", "updateUser", "u-budi123456", map[string]any{"nickname": "Budi", "bio": "Halo", "areaId": "nowhere", "accountType": "toko",
		"store": map[string]any{"name": "Toko Budi", "areaId": "pogung", "hours": "08-17"}})
	u, _ = e.svc.User(e.ctx, "u-budi123456")
	if *u.Nickname != "Budi" || u.AreaID != "pogung" || u.AccountType != "toko" || u.Store == nil || u.Store.Name != "Toko Budi" {
		t.Fatalf("update: %+v", u)
	}

	// Verification: one NIK = one account; blacklisted NIK cannot come back.
	vid := e.must("u-budi123456", "submitVerification", "u-budi123456", map[string]any{"method": "ekyc", "nameOnKtp": "budi", "nik": "3404010101010001"}).(string)
	u, _ = e.svc.User(e.ctx, "u-budi123456")
	if u.Verification.Status != "terverifikasi" || *u.Verification.NikMasked != "3404********0001" || vid == "" {
		t.Fatalf("verify: %+v", u.Verification)
	}
	if msg := e.reject("u-bima", "submitVerification", "u-bima", map[string]any{"nik": "3404010101010001"}); msg != "NIK ini sudah dipakai akun lain. Satu NIK hanya untuk satu akun." {
		t.Fatal(msg)
	}
	e.must("u-admin", "setUserStatus", "u-admin", "u-budi123456", "blacklist", "Penipuan")
	if msg := e.reject("u-bima", "submitVerification", "u-bima", map[string]any{"nik": "3404010101010001"}); msg != "NIK ini diblokir dari Sewain." {
		t.Fatal(msg)
	}

	// Admin review of a pending verification.
	if msg := e.reject("u-raka", "reviewVerification", "u-raka", "v-u-gilang", true); msg != "Hanya admin." {
		t.Fatal(msg)
	}
	if msg := e.reject("u-admin", "reviewVerification", "u-admin", "v-nope", true); msg != "Pengajuan tidak ditemukan." {
		t.Fatal(msg)
	}
	e.must("u-admin", "reviewVerification", "u-admin", "v-u-putri", false, "Foto buram")
	e.must("u-admin", "reviewVerification", "u-admin", "v-u-gilang", true)
	g, _ := e.svc.User(e.ctx, "u-gilang")
	p, _ := e.svc.User(e.ctx, "u-putri")
	if g.Verification.Status != "terverifikasi" || p.Verification.Status != "ditolak" {
		t.Fatalf("review: %s %s", g.Verification.Status, p.Verification.Status)
	}
	if st := e.state(); st.Audit[0].Action != "verifikasi.setuju" || st.Notifications[0].UserID != "u-gilang" {
		t.Fatalf("audit/notification: %+v %+v", st.Audit[0], st.Notifications[0])
	}
}

func TestListingActions(t *testing.T) {
	e := newEnv(t)
	if msg := e.reject("u-bima", "createListing", "u-bima", listingInput("Tenda kecil")); !strings.HasPrefix(msg, "Verifikasi KTP") {
		t.Fatal(msg)
	}
	if msg := e.reject("u-raka", "createListing", "u-raka", listingInput("Te")); msg != "Judul terlalu pendek." {
		t.Fatal(msg)
	}
	if msg := e.reject("u-raka", "createListing", "u-raka", listingInput("Airsoft gun mainan")); msg != "Ada kata yang dilarang: airsoft." {
		t.Fatal(msg)
	}
	in := listingInput("Tenda kecil 2 orang")
	in["stock"] = 5 // individual accounts always have stock 1
	id, err := e.tryID("u-raka", "createListing", "l-tendakecil1", "u-raka", in)
	if err != nil || id != "l-tendakecil1" {
		t.Fatalf("create: %v %v", id, err)
	}
	l := e.listing("l-tendakecil1")
	if l.Stock != 1 || l.Approval != "manual" || len(l.Photos) != 3 || l.CollateralOptions[0].ID != "co-x1" {
		t.Fatalf("listing %+v", l)
	}
	// The open wanted post for "camping" (w-4) gets a notification about the new listing.
	if n := e.state().Notifications[0]; n.Kind != "papan" || n.Title != "Ada barang baru yang cocok" {
		t.Fatalf("wanted requester notification: %+v", n)
	}
	in["title"] = "Tenda kecil 2 orang (update)"
	if msg := e.reject("u-sinta", "updateListing", "u-sinta", "l-tendakecil1", in); msg != "Bukan listing milikmu." {
		t.Fatal(msg)
	}
	e.must("u-raka", "updateListing", "u-raka", "l-tendakecil1", in)
	if e.listing("l-tendakecil1").Title != "Tenda kecil 2 orang (update)" {
		t.Fatal("update not saved")
	}
	if msg := e.reject("u-raka", "setListingStatus", "u-raka", "l-tendakecil1", "diturunkan"); msg != "Hanya admin yang bisa menurunkan listing." {
		t.Fatal(msg)
	}
	if msg := e.reject("u-raka", "setListingStatus", "u-raka", "l-tendakecil1", "hapus"); msg != "Status tidak dikenal." {
		t.Fatal(msg)
	}
	e.must("u-raka", "setListingStatus", "u-raka", "l-tendakecil1", "jeda")
	e.must("u-admin", "setListingStatus", "u-admin", "l-tendakecil1", "diturunkan")
	st := e.state()
	if st.Audit[0].Action != "listing.diturunkan" {
		t.Fatalf("audit %+v", st.Audit[0])
	}

	e.must("u-raka", "toggleFavorite", "u-raka", "l-kamera-dslr")
	if f := e.state().Favorites["u-raka"]; len(f) != 3 || f[2] != "l-kamera-dslr" {
		t.Fatalf("favorites %v", f)
	}
	e.must("u-raka", "toggleFavorite", "u-raka", "l-tenda-dome-4p")
	if f := e.state().Favorites["u-raka"]; len(f) != 2 || f[0] != "l-action-cam" {
		t.Fatalf("favorites %v", f)
	}
}

func TestBookingRejections(t *testing.T) {
	e := newEnv(t)
	l := e.listing("l-kompor-lapangan")
	cases := []struct {
		who  string
		in   map[string]any
		want string
	}{
		{"u-raka", map[string]any{"listingId": "l-flagged-airsoft"}, "Barang tidak tersedia."},
		{"u-bayu", bookingInput(l, e.today(1), e.today(2)), "Tidak bisa menyewa barang sendiri."},
		{"u-raka", bookingInput(l, "kemarin", e.today(2)), "Tanggal ambil tidak valid."},
		{"u-raka", bookingInput(l, e.today(3), e.today(2)), "Tanggal kembali harus setelah tanggal ambil."},
		{"u-raka", bookingInput(l, e.today(-2), e.today(1)), "Tanggal ambil sudah lewat."},
		{"u-raka", map[string]any{"listingId": l.ID, "startDate": e.today(1), "endDate": e.today(2), "handover": "jemput", "collateralOptionId": "x"}, "Pilih bentuk jaminan dari opsi penyedia."},
		{"u-raka", map[string]any{"listingId": l.ID, "startDate": e.today(1), "endDate": e.today(2), "handover": "kurir", "collateralOptionId": l.CollateralOptions[0].ID}, "Metode serah terima tidak tersedia."},
	}
	for _, c := range cases {
		if msg := e.reject(c.who, "createBooking", c.who, c.in); msg != c.want {
			t.Errorf("want %q, got %q", c.want, msg)
		}
	}
	// State-machine guards on seeded bookings.
	guards := []struct {
		who, name string
		a         []any
		want      string
	}{
		{"u-dina", "acceptBooking", []any{"u-dina", "b-accepted"}, "Request ini sudah tidak menunggu persetujuan."},
		{"u-bayu", "declineBooking", []any{"u-bayu", "b-active", "x"}, "Request ini sudah tidak menunggu persetujuan."},
		{"u-sinta", "cancelBooking", []any{"u-sinta", "b-active", "x"}, "Booking hanya bisa dibatalkan sebelum serah terima."},
		{"u-raka", "payBooking", []any{"u-raka", "b-requested", "qris"}, "Pembayaran tersedia setelah request disetujui."},
		{"u-raka", "submitCheckout", []any{"u-raka", "b-accepted", map[string]any{}}, "Hanya penyedia yang memotret serah terima."},
		{"u-dina", "submitCheckout", []any{"u-dina", "b-accepted", map[string]any{"photoIds": []string{"a"}}}, "Minimal 3 foto kondisi barang."},
		{"u-dina", "submitCheckout", []any{"u-dina", "b-accepted", map[string]any{"photoIds": []string{"a", "b", "c"}}}, "Foto jaminan wajib (FR-J5)."},
		{"u-dina", "submitCheckout", []any{"u-dina", "b-accepted", map[string]any{"photoIds": []string{"a", "b", "c"}, "collateralPhotoIds": []string{"d"}}}, "Ada foto yang belum terunggah ke server. Coba lagi."},
		{"u-raka", "agreeCheckout", []any{"u-raka", "b-accepted"}, "Foto check-out belum ada."},
		{"u-sinta", "rejectAtCheckout", []any{"u-sinta", "b-active", "x"}, "Hanya saat serah terima."},
		{"u-bayu", "submitCheckin", []any{"u-bayu", "b-active", map[string]any{"photoIds": []string{"a", "b", "c"}, "result": "?"}}, "Pilih: kondisi sesuai atau ada masalah."},
		{"u-bayu", "submitCheckin", []any{"u-bayu", "b-active", map[string]any{"photoIds": []string{"a", "b", "c"}, "result": "masalah"}}, "Pilih jenis masalah dan lampirkan foto masalahnya."},
		{"u-tini", "submitCheckin", []any{"u-tini", "b-active", map[string]any{}}, "Hanya penyedia yang memotret pengembalian."},
		{"u-bayu", "returnCollateral", []any{"u-bayu", "b-active"}, "Jaminan dikembalikan setelah barang kembali."},
		{"u-sinta", "confirmCollateral", []any{"u-sinta", "b-returned"}, "Penyedia belum menandai jaminan dikembalikan."},
		{"u-sinta", "submitReview", []any{"u-sinta", "b-active", 5, ""}, "Rating diberikan setelah sewa selesai."},
		{"u-raka", "submitReview", []any{"u-raka", "b-completed", 6, ""}, "Rating 1–5."},
		{"u-dina", "sendMessage", []any{"u-dina", "b-active", map[string]any{"text": "hai"}}, "Chat tidak ditemukan."},
		{"u-sinta", "sendMessage", []any{"u-sinta", "b-active", map[string]any{"text": "  "}}, "Pesan kosong."},
		{"u-sinta", "sendMessage", []any{"u-sinta", "b-active", map[string]any{"photoId": "p-nope"}}, "Foto belum terunggah ke server. Coba lagi."},
	}
	for _, g := range guards {
		if msg := e.reject(g.who, g.name, g.a...); msg != g.want {
			t.Errorf("%s: want %q, got %q", g.name, g.want, msg)
		}
	}
}

func TestBookingSideActions(t *testing.T) {
	e := newEnv(t)
	// decline + cancel
	e.must("u-bayu", "declineBooking", "u-bayu", "b-requested", " ")
	if b := e.booking("b-requested"); b.Status != "DECLINED" || *b.DeclineReason != "Tidak ada alasan." {
		t.Fatalf("decline %+v", b)
	}
	// pay twice (unknown method falls back to QRIS)
	e.must("u-raka", "payBooking", "u-raka", "b-accepted", "bitcoin")
	if b := e.booking("b-accepted"); b.Payment.Status != "lunas" || *b.Payment.Method != "qris" {
		t.Fatalf("pay %+v", b.Payment)
	}
	if msg := e.reject("u-raka", "payBooking", "u-raka", "b-accepted", "qris"); msg != "Sudah lunas." {
		t.Fatal(msg)
	}
	e.must("u-raka", "cancelBooking", "u-raka", "b-accepted", "Ganti jadwal")
	st := e.state()
	if b := e.booking("b-accepted"); b.Status != "CANCELLED" || *b.CancelledBy != "u-raka" {
		t.Fatalf("cancel %+v", b)
	}
	if n := st.Notifications[0]; n.UserID != "u-dina" || n.Title != "Booking dibatalkan" {
		t.Fatalf("notification %+v", n)
	}
	if m := st.Messages[len(st.Messages)-1]; m.SenderID != "system" || m.Text == nil || *m.Text != "Booking dibatalkan oleh penyewa. Alasan: Ganti jadwal" {
		t.Fatalf("system message %+v", m)
	}

	// chat + read markers
	e.must("u-sinta", "sendMessage", "u-sinta", "b-active", map[string]any{"text": "Sudah sampai?"})
	st = e.state()
	if st.ChatReads["u-sinta:b-active"] == 0 || st.Notifications[0].Kind != "chat" || st.Notifications[0].UserID != "u-bayu" {
		t.Fatal("chat must notify the other party and mark the sender's thread as read")
	}
	e.must("u-bayu", "markThreadRead", "u-bayu", "b-active")
	e.must("u-bayu", "markNotificationsRead", "u-bayu", nil)
	for _, n := range e.state().Notifications {
		if n.UserID == "u-bayu" && !n.Read {
			t.Fatal("all of u-bayu's notifications must be read")
		}
	}

	// reject at checkout files a report and cancels without penalty
	e2 := newEnv(t)
	e2.must("u-raka", "rejectAtCheckout", "u-raka", "b-accepted", "Lensa jamuran")
	st2 := e2.state()
	if b := e2.booking("b-accepted"); b.Status != "CANCELLED" {
		t.Fatalf("reject %+v", b.Status)
	}
	if st2.Reports[0].TargetID != "l-kamera-mirrorless" || st2.Notifications[0].UserID != domain.AdminID {
		t.Fatalf("report/admin notification: %+v %+v", st2.Reports[0], st2.Notifications[0])
	}

	// check-in with a problem opens an L1 dispute automatically
	pid := "b-active"
	e.must("u-bayu", "submitCheckin", "u-bayu", "b-active", map[string]any{"photoIds": e.photos(3, "checkin", "u-bayu", &pid), "result": "masalah",
		"problemType": "Lecet di frame", "problemNote": "pojok kiri", "problemPhotoIds": e.photos(1, "masalah", "u-bayu", &pid)})
	b := e.booking("b-active")
	if b.Status != "DISPUTED" || b.DisputeID == nil {
		t.Fatalf("checkin problem: %+v", b.Status)
	}
	d := e.state().Disputes[0]
	if d.CaseType != "lecet-ringan" || d.Layer != "L1" || len(d.PhotoIDs) != 1 || d.Description != "Lecet di frame — pojok kiri" {
		t.Fatalf("dispute %+v", d)
	}
	if msg := e.reject("u-sinta", "openDispute", "u-sinta", "b-active", map[string]any{"description": "x"}); msg != "Sengketa bisa dibuka saat sedang disewa, telat, atau setelah pengembalian." {
		t.Fatal(msg)
	}
}

func TestDisputeAndAdminActions(t *testing.T) {
	e := newEnv(t)
	if msg := e.reject("u-raka", "openDispute", "u-raka", "b-overdue", map[string]any{"description": " "}); msg != "Ceritakan masalahnya." {
		t.Fatal(msg)
	}
	did := e.must("u-tini", "openDispute", "u-tini", "b-overdue", map[string]any{"caseType": "hilang", "description": "Tutup panci hilang"}).(string)
	if msg := e.reject("u-raka", "openDispute", "u-raka", "b-overdue", map[string]any{"description": "lagi"}); msg != "Sengketa bisa dibuka saat sedang disewa, telat, atau setelah pengembalian." {
		t.Fatal(msg)
	}
	if msg := e.reject("u-sinta", "proposeSolution", "u-sinta", did, map[string]any{"type": "potong-deposit"}); msg != "Sengketa tidak ditemukan." {
		t.Fatal(msg)
	}
	if msg := e.reject("u-raka", "proposeSolution", "u-raka", did, map[string]any{"type": "nego"}); msg != "Jenis solusi tidak dikenal." {
		t.Fatal(msg)
	}
	e.must("u-raka", "proposeSolution", "u-raka", did, map[string]any{"type": "tanpa-kompensasi"})
	e.must("u-tini", "proposeSolution", "u-tini", did, map[string]any{"type": "potong-deposit", "amount": 50000})
	var props []domain.DisputeProposal
	for _, d := range e.state().Disputes {
		if d.ID == did {
			props = d.Proposals
		}
	}
	if len(props) != 2 || props[0].ByID != "u-raka" {
		t.Fatalf("proposals %+v", props)
	}
	if msg := e.reject("u-raka", "respondProposal", "u-raka", did, props[0].ID, true); msg != "Tidak bisa merespons usulan sendiri." {
		t.Fatal(msg)
	}
	e.must("u-tini", "respondProposal", "u-tini", did, props[0].ID, false)
	if msg := e.reject("u-tini", "respondProposal", "u-tini", did, props[0].ID, true); msg != "Usulan sudah direspons." {
		t.Fatal(msg)
	}
	e.must("u-raka", "respondProposal", "u-raka", did, props[1].ID, true)
	if b := e.booking("b-overdue"); b.Status != "RESOLVED" {
		t.Fatalf("L1 agreement resolves the booking: %s", b.Status)
	}
	if msg := e.reject("u-raka", "escalateDispute", "u-raka", did); msg != "Sengketa sudah di tahap mediasi." {
		t.Fatal(msg)
	}

	// L2 on the seeded dispute: recommendation + resolution by an admin.
	if msg := e.reject("u-raka", "adminRecommend", "u-raka", "d-lensa", map[string]any{"text": "x"}); msg != "Hanya admin." {
		t.Fatal(msg)
	}
	if msg := e.reject("u-admin", "adminRecommend", "u-admin", "d-lensa", map[string]any{"text": " "}); msg != "Tulis rekomendasinya." {
		t.Fatal(msg)
	}
	e.must("u-admin", "adminRecommend", "u-admin", "d-lensa", map[string]any{"text": "Bagi dua biaya poles", "amount": 175000})
	e.must("u-admin", "resolveDispute", "u-admin", "d-lensa", "Penyewa membayar Rp175.000")
	st := e.state()
	if b := e.booking("b-disputed"); b.Status != "RESOLVED" {
		t.Fatalf("resolve: %s", b.Status)
	}
	if st.Audit[0].Action != "sengketa.selesai" || st.Audit[1].Action != "sengketa.rekomendasi" {
		t.Fatalf("audit %+v %+v", st.Audit[0], st.Audit[1])
	}

	// Reports, strikes, user status, settings.
	rid := e.must("u-raka", "createReport", "u-raka", map[string]any{"targetType": "user", "targetId": "u-arif", "reason": "Tidak datang"}).(string)
	if msg := e.reject("u-raka", "createReport", "u-raka", map[string]any{"targetType": "chat", "reason": "x"}); msg != "Target laporan tidak dikenal." {
		t.Fatal(msg)
	}
	if msg := e.reject("u-admin", "resolveReport", "u-admin", rid, "hapus", ""); msg != "Status laporan tidak dikenal." {
		t.Fatal(msg)
	}
	e.must("u-admin", "resolveReport", "u-admin", rid, "ditindak", "Strike ringan")
	e.must("u-admin", "addStrike", "u-admin", "u-arif", map[string]any{"reason": "No-show", "severity": "berat"})
	arif, _ := e.svc.User(e.ctx, "u-arif")
	if arif.Strikes != 3 { // 1 seeded + 2 (berat)
		t.Fatalf("strikes %d", arif.Strikes)
	}
	e.must("u-admin", "setUserStatus", "u-admin", "u-bayu", "suspend", "Investigasi")
	for _, l := range e.state().Listings {
		if l.OwnerID == "u-bayu" && l.Status == "aktif" {
			t.Fatal("listings of a suspended user must be paused")
		}
	}
	if msg := e.reject("u-bayu", "createListing", "u-bayu", listingInput("Tenda baru")); msg != "Akun sedang ditangguhkan." {
		t.Fatal(msg)
	}

	if msg := e.reject("u-admin", "updateCommission", "u-admin", map[string]any{"percent": 120}); msg != "Nilai komisi tidak valid." {
		t.Fatal(msg)
	}
	e.must("u-admin", "updateCommission", "u-admin", map[string]any{"active": true, "min": nil})
	st = e.state()
	if !st.Commission.Active || st.Commission.Min != nil || st.Audit[0].Action != "komisi.ubah" {
		t.Fatalf("commission %+v", st.Commission)
	}
	if msg := e.reject("u-admin", "updateCollateralRules", "u-admin", map[string]any{"midMax": 1}); !strings.HasPrefix(msg, "Ambang nilai tidak valid") {
		t.Fatal(msg)
	}
	e.must("u-admin", "updateCollateralRules", "u-admin", map[string]any{"highDepositPercent": 50})
	e.must("u-admin", "setBannedKeywords", "u-admin", []string{" Zebra ", "apel", "zebra"})
	st = e.state()
	if st.CollateralRules.HighDepositPercent != 50 || strings.Join(st.BannedKeywords, ",") != "apel,zebra" {
		t.Fatalf("rules %+v keywords %v", st.CollateralRules, st.BannedKeywords)
	}
}

func TestWantedActions(t *testing.T) {
	e := newEnv(t)
	if msg := e.reject("u-raka", "createWanted", "u-raka", map[string]any{"title": " "}); msg != "Tulis barang yang kamu cari." {
		t.Fatal(msg)
	}
	wid := e.must("u-raka", "createWanted", "u-raka", map[string]any{"title": "Tenda 6 orang", "category": "camping", "areaId": "mars"}).(string)
	st := e.state()
	if w := st.Wanted[0]; w.ID != wid || w.AreaID != "ugm" || w.RadiusKm != 3 {
		t.Fatalf("wanted %+v", w)
	}
	if n := st.Notifications[0]; n.Kind != "papan" || n.UserID == "u-raka" {
		t.Fatalf("providers must be notified: %+v", n)
	}
	if msg := e.reject("u-raka", "offerWanted", "u-raka", wid, map[string]any{"message": "x"}); msg != "Ini permintaanmu sendiri." {
		t.Fatal(msg)
	}
	e.must("u-bayu", "offerWanted", "u-bayu", wid, map[string]any{"listingId": "l-tenda-dome-4p", "message": "Ada!"})
	if msg := e.reject("u-bayu", "closeWanted", "u-bayu", wid, "ditutup"); msg != "Permintaan tidak ditemukan." {
		t.Fatal(msg)
	}
	e.must("u-raka", "closeWanted", "u-raka", wid, "terpenuhi")
	if msg := e.reject("u-bayu", "offerWanted", "u-bayu", wid, map[string]any{"message": "lagi"}); msg != "Permintaan sudah ditutup." {
		t.Fatal(msg)
	}
	if w := e.state().Wanted[0]; w.Status != "terpenuhi" || len(w.Offers) != 1 {
		t.Fatalf("wanted %+v", w)
	}
}

func TestUnknownActionAndBadArgs(t *testing.T) {
	e := newEnv(t)
	if _, err := e.try("u-raka", "dropTables", "u-raka"); err != domain.ErrUnknownAction {
		t.Fatalf("want ErrUnknownAction, got %v", err)
	}
	if msg := e.reject("u-raka", "createBooking", "u-raka", "not-an-object"); msg != "Argumen 2 tidak valid." {
		t.Fatal(msg)
	}
	if _, err := e.svc.Dispatch(e.ctx, nil, "acceptBooking", args("u-bayu", "b-requested"), ""); !isForbidden(err) {
		t.Fatalf("guest: %v", err)
	}
}
