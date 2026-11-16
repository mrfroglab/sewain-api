package service

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/mrfroglab/sewain-api/internal/domain"
)

// ImportColumns documents the CSV accepted by POST /api/v1/admin/listings/import (Early Lender onboarding, PRD
// §18.1). The first row is the header (names case-insensitive, any order). Lists use "|".
var ImportColumns = []ImportColumn{
	{"owner_phone", true, "Nomor HP pemilik (08…/62…/+62…). Akun dicari dari nomor ini; kalau belum ada dibuat dengan status verifikasi \"belum\"."},
	{"owner_name", false, "Nama pemilik. Wajib kalau akun pemilik belum ada."},
	{"early_lender", false, "ya/tidak: tandai pemilik baru sebagai Early Lender."},
	{"title", true, "Judul barang (≥ 4 huruf)."},
	{"category", true, "camping | kamera | pakaian-acara | elektronik | alat-masak | musim-hujan | kendaraan | olahraga"},
	{"description", false, "Deskripsi."},
	{"condition_notes", false, "Catatan kondisi."},
	{"price_per_day", true, "Harga sewa per hari (rupiah, angka)."},
	{"price_per_week", false, "Harga per minggu (rupiah)."},
	{"item_value", true, "Perkiraan nilai barang (rupiah)."},
	{"lat", true, "Lintang titik serah terima (presisi; publik dibulatkan ± 400 m)."},
	{"lng", true, "Bujur."},
	{"area_id", false, "ugm | pogung | sendowo | karangmalang | karangwuni | klebengan | terban | condongcatur (default: area pemilik)."},
	{"radius_km", false, "Jangkauan penyedia (km, default 3)."},
	{"handover", true, "Cara serah terima, dipisah |: cod | jemput | kurir | antar."},
	{"collateral", true, "Jaminan, dipisah |: identitas:KTM/SIM, deposit:50000, deposit:25%, barang:<deskripsi>."},
	{"photos", true, "Minimal 3 URL foto dipisah | (mis. /img/listings/a.webp atau /api/v1/photos/<id>)."},
	{"stock", false, "Jumlah unit (akun Toko saja, default 1)."},
	{"meeting_point", false, "Titik temu publik (mis. \"Gerbang Pogung\")."},
	{"late_fee_per_day", false, "Denda telat per hari (rupiah)."},
	{"cancellation", false, "Kebijakan pembatalan."},
}

// ImportColumn describes one CSV column.
type ImportColumn struct {
	Name     string `json:"name"`
	Required bool   `json:"required"`
	Doc      string `json:"doc"`
}

// MaxImportRows bounds one import.
const MaxImportRows = 1000

// ImportRow is the result for one data row (Row = line number in the file, header = 1).
type ImportRow struct {
	Row          int      `json:"row"`
	OK           bool     `json:"ok"`
	ListingID    string   `json:"listingId,omitempty"`
	OwnerID      string   `json:"ownerId,omitempty"`
	OwnerCreated bool     `json:"ownerCreated,omitempty"`
	Status       string   `json:"status,omitempty"` // aktif | jeda (owner not verified yet)
	Errors       []string `json:"errors,omitempty"`
}

// ImportReport is the response of an import.
type ImportReport struct {
	DryRun  bool        `json:"dryRun"`
	Created int         `json:"created"`
	Failed  int         `json:"failed"`
	Rows    []ImportRow `json:"rows"`
}

var errImportRollback = errors.New("import: rollback")

// ImportListings creates listings from a CSV for the admin (all rows or none: any row error writes nothing). Owners
// are found by phone or created (verification "belum"); listings of unverified owners start "jeda" until the owner
// verifies. dryRun validates everything, including owner creation, then rolls back.
func (s *Service) ImportListings(ctx context.Context, adminID string, src io.Reader, dryRun bool) (ImportReport, Change, error) {
	rep := ImportReport{DryRun: dryRun, Rows: []ImportRow{}}
	records, err := readCSV(src)
	if err != nil {
		return rep, Change{}, err
	}
	header := map[string]int{}
	for i, h := range records[0] {
		header[strings.ToLower(strings.TrimSpace(strings.TrimPrefix(h, "\uFEFF")))] = i
	}
	var missing []string
	for _, c := range ImportColumns {
		if _, ok := header[c.Name]; c.Required && !ok {
			missing = append(missing, c.Name)
		}
	}
	if len(missing) > 0 {
		return rep, Change{}, domain.Fail("Kolom wajib tidak ada: " + strings.Join(missing, ", ") + ".")
	}
	if len(records)-1 > MaxImportRows {
		return rep, Change{}, domain.Fail(fmt.Sprintf("Maksimal %d baris per impor.", MaxImportRows))
	}
	ch, err := s.run(ctx, func(t *txn) error {
		if !t.isAdmin(adminID) {
			return domain.Fail("Hanya admin.")
		}
		owners := map[string]*domain.User{} // normalized phone → owner (incl. owners new in this import)
		newOwners := map[string]bool{}      // phones of owners this import creates
		created := map[string]bool{}
		for i, rec := range records[1:] {
			row := ImportRow{Row: i + 2}
			get := func(col string) string {
				if j, ok := header[col]; ok && j < len(rec) {
					return strings.TrimSpace(rec[j])
				}
				return ""
			}
			l, owner, isNew, errs := t.importRow(get, owners, newOwners)
			if t.err != nil {
				return nil
			}
			if len(errs) > 0 {
				row.Errors = errs
				rep.Failed++
				rep.Rows = append(rep.Rows, row)
				continue
			}
			if isNew && !created[owner.Phone] {
				if err := t.insertUser(owner); err != nil {
					row.Errors = []string{err.Error()}
					rep.Failed++
					rep.Rows = append(rep.Rows, row)
					continue
				}
				created[owner.Phone] = true
				t.notify(owner.ID, "verifikasi", "Selamat datang di Sewain!",
					"Admin mendaftarkan barangmu. Masuk pakai nomor HP ini lalu verifikasi KTP + selfie supaya barangmu tampil.", "/verifikasi")
			}
			t.insertListing(l)
			row.OK, row.ListingID, row.OwnerID, row.OwnerCreated, row.Status = true, l.ID, owner.ID, isNew, l.Status
			rep.Created++
			rep.Rows = append(rep.Rows, row)
		}
		if t.err != nil {
			return nil
		}
		if rep.Failed > 0 || dryRun {
			return errImportRollback
		}
		t.audit(adminID, "listing.impor", fmt.Sprintf("%d listing", rep.Created), nil, nil)
		return nil
	})
	if errors.Is(err, errImportRollback) {
		if rep.Failed > 0 {
			rep.Created = 0
			for i := range rep.Rows {
				if rep.Rows[i].OK {
					rep.Rows[i].ListingID = "" // nothing was written
				}
			}
		}
		return rep, Change{}, nil
	}
	return rep, ch, err
}

func readCSV(src io.Reader) ([][]string, error) {
	data, err := io.ReadAll(io.LimitReader(src, 5<<20))
	if err != nil {
		return nil, err
	}
	text := strings.TrimPrefix(string(data), "\uFEFF")
	r := csv.NewReader(strings.NewReader(text))
	first, _, _ := strings.Cut(text, "\n")
	if strings.Count(first, ";") > strings.Count(first, ",") {
		r.Comma = ';' // spreadsheet exports with an Indonesian locale
	}
	r.FieldsPerRecord = -1
	records, err := r.ReadAll()
	if err != nil {
		return nil, domain.Fail("CSV tidak valid: " + err.Error())
	}
	if len(records) < 2 {
		return nil, domain.Fail("CSV kosong: butuh baris judul kolom dan minimal satu baris data.")
	}
	return records, nil
}

// importRow builds (but does not insert) a listing and its owner from one CSV row; errs lists every problem.
func (t *txn) importRow(get func(string) string, owners map[string]*domain.User, newOwners map[string]bool) (*domain.Listing, *domain.User, bool, []string) {
	var errs []string
	fail := func(msg string) { errs = append(errs, msg) }
	num := func(col string, required bool) *int64 {
		v := strings.NewReplacer(".", "", ",", "", " ", "", "Rp", "").Replace(get(col))
		if v == "" {
			if required {
				fail(col + ": wajib diisi.")
			}
			return nil
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			fail(col + ": harus angka rupiah.")
			return nil
		}
		return &n
	}
	float := func(col string, required bool) *float64 {
		v := strings.ReplaceAll(get(col), ",", ".")
		if v == "" {
			if required {
				fail(col + ": wajib diisi.")
			}
			return nil
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			fail(col + ": harus angka.")
			return nil
		}
		return &f
	}
	list := func(col string) []string {
		var out []string
		for _, p := range strings.Split(get(col), "|") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return out
	}

	// Owner.
	phone := NormalizePhone(get("owner_phone"))
	var owner *domain.User
	isNew := false
	switch {
	case len(phone) < 9 || !strings.HasPrefix(phone, "08"):
		fail("owner_phone: nomor HP tidak valid.")
	case owners[phone] != nil:
		owner, isNew = owners[phone], newOwners[phone]
	default:
		r, err := t.q.FindUserByPhone(t.ctx, phone)
		if t.found(err) {
			owner = t.loadUser(r, nil)
		} else if t.err == nil {
			name := strings.TrimSpace(get("owner_name"))
			if name == "" {
				fail("owner_name: wajib untuk pemilik baru.")
				break
			}
			owner = &domain.User{ID: domain.NewID("u"), Name: name, Phone: phone, Role: "user", AccountType: "individu",
				Verification: domain.Verification{Status: "belum"}, AreaID: "ugm", JoinedAt: t.now, Status: "aktif"}
			if a := get("area_id"); domain.Areas[a].ID != "" {
				owner.AreaID = a
			}
			if v := strings.ToLower(get("early_lender")); v == "ya" || v == "true" || v == "1" {
				owner.EarlyLender = domain.Ptr(true)
			}
			isNew = true
			newOwners[phone] = true
		}
		if owner != nil {
			owners[phone] = owner
		}
	}
	if owner != nil && owner.Status != "aktif" {
		fail("owner_phone: akun pemilik sedang " + owner.Status + ".")
	}

	// Listing.
	in := domain.ListingInput{Title: domain.Ptr(get("title")), Category: domain.Ptr(get("category")), Photos: list("photos"),
		PricePerDay: num("price_per_day", true), PricePerWeek: num("price_per_week", false), ItemValue: num("item_value", true),
		Handover: lower(list("handover"))}
	if v := get("description"); v != "" {
		in.Description = &v
	}
	if v := get("condition_notes"); v != "" {
		in.ConditionNotes = &v
	}
	if v := get("meeting_point"); v != "" {
		in.MeetingPoint = &v
	}
	lat, lng := float("lat", true), float("lng", true)
	if lat != nil && lng != nil {
		if *lat < -11 || *lat > 6 || *lng < 95 || *lng > 141 {
			fail("lat/lng: titik di luar Indonesia.")
		}
		area := get("area_id")
		if area == "" && owner != nil {
			area = owner.AreaID
		}
		if domain.Areas[area].ID == "" {
			fail("area_id: area tidak dikenal.")
		}
		in.Location = &domain.ListingLocation{Lat: *lat, Lng: *lng, AreaID: area}
	}
	if r := float("radius_km", false); r != nil {
		if *r <= 0 || *r > 50 {
			fail("radius_km: harus 0–50.")
		}
		in.RadiusKm = r
	}
	for _, h := range in.Handover {
		if !domain.Contains(domain.HandoverMethods, h) {
			fail("handover: " + h + " tidak dikenal (" + strings.Join(domain.HandoverMethods, " | ") + ").")
		}
	}
	rules := domain.ListingRules{Cancellation: get("cancellation")}
	if rules.Cancellation == "" {
		rules.Cancellation = "Gratis jika batal lebih dari 24 jam sebelum serah terima."
	}
	if v := num("late_fee_per_day", false); v != nil {
		rules.LateFeePerDay, rules.LateFeeCapPercent = *v, 100
	}
	in.Rules = &rules
	for i, c := range list("collateral") {
		o, err := parseCollateral(c)
		if err != nil {
			fail("collateral: " + err.Error())
			continue
		}
		o.ID = fmt.Sprintf("co-%d", i+1)
		in.CollateralOptions = append(in.CollateralOptions, o)
	}
	if v := get("stock"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			fail("stock: minimal 1.")
		} else {
			in.Stock = &n
		}
	}
	if len(errs) > 0 || owner == nil {
		return nil, owner, isNew, errs
	}
	id := t.pickID("", "l", t.q.ListingExists)
	l := &domain.Listing{ID: id, Slug: id, OwnerID: owner.ID, Status: "aktif", CreatedAt: t.now, Stock: 1, Approval: "manual", RadiusKm: 3}
	domain.ApplyListing(l, in)
	if owner.AccountType != "toko" || l.Stock < 1 {
		l.Stock = 1
	}
	if err := domain.ValidateListing(l, t.bannedKeywords()); err != nil {
		return nil, owner, isNew, []string{err.Error()}
	}
	if owner.Verification.Status != "terverifikasi" {
		l.Status = "jeda" // shown once the owner verifies and switches it on
	}
	return l, owner, isNew, nil
}

func parseCollateral(spec string) (domain.CollateralOption, error) {
	kind, arg, _ := strings.Cut(spec, ":")
	kind, arg = strings.ToLower(strings.TrimSpace(kind)), strings.TrimSpace(arg)
	switch kind {
	case "identitas":
		cards := []string{"KTM"}
		if arg != "" {
			cards = strings.Split(arg, "/")
		}
		return domain.CollateralOption{Kind: "identitas", Cards: cards}, nil
	case "deposit":
		if strings.HasSuffix(arg, "%") {
			p, err := strconv.ParseFloat(strings.TrimSuffix(arg, "%"), 64)
			if err != nil || p <= 0 || p > 100 {
				return domain.CollateralOption{}, errors.New("persen deposit harus 1–100")
			}
			return domain.CollateralOption{Kind: "deposit", DepositMode: domain.Ptr("persen"), DepositPercent: &p}, nil
		}
		n, err := strconv.ParseInt(strings.NewReplacer(".", "", ",", "").Replace(arg), 10, 64)
		if err != nil || n <= 0 {
			return domain.CollateralOption{}, errors.New("nominal deposit tidak valid")
		}
		return domain.CollateralOption{Kind: "deposit", DepositMode: domain.Ptr("tetap"), DepositAmount: &n}, nil
	case "barang":
		if arg == "" {
			return domain.CollateralOption{}, errors.New("jaminan barang butuh deskripsi")
		}
		return domain.CollateralOption{Kind: "barang", Description: &arg}, nil
	}
	return domain.CollateralOption{}, fmt.Errorf("jenis %q tidak dikenal (identitas | deposit | barang)", kind)
}

func lower(xs []string) []string {
	for i, x := range xs {
		xs[i] = strings.ToLower(x)
	}
	return xs
}
