package service_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/mrfroglab/sewain-api/internal/domain"
)

const importHeader = "owner_phone,owner_name,title,category,price_per_day,item_value,lat,lng,area_id,handover,collateral,photos\n"

func importRow(title, handover, photos string) string {
	return fmt.Sprintf("081578904412,,%s,camping,15000,200000,-7.7596,110.3727,pogung,%s,identitas:KTM,%s\n", title, handover, photos)
}

const seedPhotos = "/img/listings/a.webp|/img/listings/b.webp|/img/listings/c.webp"

// Polish (review wave 2, I1): the CSV import accepts exactly the handover methods of the domain and the web
// (cod | jemput | kurir | antar), one at a time and together, and nothing else.
func TestImportHandoverMethods(t *testing.T) {
	e := newEnv(t)
	csv := importHeader
	for _, h := range domain.HandoverMethods {
		csv += importRow("Hammock "+h, h, seedPhotos)
	}
	csv += importRow("Hammock semua", strings.Join(domain.HandoverMethods, "|"), seedPhotos)
	csv += importRow("Hammock huruf besar", "COD|Kurir", seedPhotos)
	rep, _, err := e.svc.ImportListings(e.ctx, "u-admin", strings.NewReader(csv), false)
	if err != nil || rep.Failed != 0 || rep.Created != len(domain.HandoverMethods)+2 {
		t.Fatalf("import: %+v %v", rep, err)
	}
	got := map[string][]string{}
	for _, l := range e.state().Listings {
		if strings.HasPrefix(l.Title, "Hammock") {
			got[l.Title] = l.Handover
		}
	}
	if h := got["Hammock semua"]; strings.Join(h, "|") != strings.Join(domain.HandoverMethods, "|") {
		t.Fatalf("all methods: %v", h)
	}
	if h := got["Hammock huruf besar"]; strings.Join(h, "|") != "cod|kurir" {
		t.Fatalf("case-insensitive: %v", h)
	}
	if h := got["Hammock kurir"]; len(h) != 1 || h[0] != "kurir" {
		t.Fatalf("kurir: %v", got)
	}

	rep, _, err = e.svc.ImportListings(e.ctx, "u-admin", strings.NewReader(importHeader+importRow("Hammock drone", "drone", seedPhotos)), true)
	if err != nil || rep.Failed != 1 || !strings.Contains(strings.Join(rep.Rows[0].Errors, " "), "cod | jemput | kurir | antar") {
		t.Fatalf("unknown method: %+v %v", rep, err)
	}
}
