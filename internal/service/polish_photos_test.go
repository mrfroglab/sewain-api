package service_test

import (
	"strings"
	"testing"

	"github.com/mrfroglab/sewain-api/internal/service"
)

// Polish (review wave 2, L4): listing photos are photos uploaded through Sewain as listing photos by the owner (or
// the admin who edits/imports), or bundled /img images in DEMO_MODE. External URLs are refused everywhere.
func TestListingPhotosMustBeOwnUploads(t *testing.T) {
	e := newEnv(t)
	own := e.photos(3, "referensi", "u-raka", nil)
	others := e.photos(1, "referensi", "u-bayu", nil)
	evidence := e.photos(1, "checkout", "u-raka", nil)
	with := func(photos ...string) map[string]any {
		in := listingInput("Tenda kecil 2 orang")
		in["photos"] = photos
		return in
	}
	for name, c := range map[string]struct {
		photos []string
		want   string
	}{
		"external https":     {[]string{"https://evil.example/a.jpg", "/img/listings/b.webp", "/img/listings/c.webp"}, "bukan tautan dari situs lain"},
		"protocol-relative":  {[]string{"//evil.example/a.jpg", own[1], own[2]}, "bukan tautan dari situs lain"},
		"path traversal":     {[]string{"/img/../api/v1/admin", own[1], own[2]}, "bukan tautan dari situs lain"},
		"signed url":         {[]string{service.PhotoPath + own[0] + "?sig=x", own[1], own[2]}, "bukan tautan dari situs lain"},
		"someone else's":     {[]string{own[0], own[1], others[0]}, "unggah sendiri"},
		"evidence, not ref":  {[]string{own[0], own[1], evidence[0]}, "unggah sendiri"},
		"unknown photo id":   {[]string{own[0], own[1], "p-tidakada99"}, "unggah sendiri"},
		"javascript scheme":  {[]string{"javascript:alert(1)", own[1], own[2]}, "bukan tautan dari situs lain"},
		"data url":           {[]string{"data:image/png;base64,AAAA", own[1], own[2]}, "bukan tautan dari situs lain"},
		"external, 2nd slot": {[]string{own[0], "http://10.0.0.1/x.png", own[2]}, "bukan tautan dari situs lain"},
	} {
		if msg := e.reject("u-raka", "createListing", "u-raka", with(c.photos...)); !strings.Contains(msg, c.want) {
			t.Errorf("%s: %q", name, msg)
		}
	}
	// Own uploads as bare ids or canonical paths are stored as canonical paths; /img is fine in DEMO_MODE.
	id := e.must("u-raka", "createListing", "u-raka", with(own[0], service.PhotoPath+own[1], "/img/listings/c.webp")).(string)
	if got := e.listing(id).Photos; strings.Join(got, ",") != service.PhotoPath+own[0]+","+service.PhotoPath+own[1]+",/img/listings/c.webp" {
		t.Fatalf("stored photos: %v", got)
	}

	// Outside DEMO_MODE (same database): no bundled images, own uploads only. Photos a listing already has stay.
	prod := service.New(e.svc.Pool(), service.Options{DemoMode: false, SnapshotScope: service.ScopeFull, Now: e.clock.Now})
	act := func(who, name string, a ...any) error {
		_, err := prod.Dispatch(e.ctx, e.viewer(who), name, args(a...), "")
		return err
	}
	if err := act("u-raka", "createListing", "u-raka", with("/img/listings/a.webp", own[1], own[2])); err == nil || !strings.Contains(err.Error(), "unggah sendiri") {
		t.Fatalf("/img outside the demo: %v", err)
	}
	if err := act("u-raka", "createListing", "u-raka", with(own...)); err != nil {
		t.Fatalf("own uploads outside the demo: %v", err)
	}
	seeded := e.listing("l-kompor-lapangan") // seeded with /img photos
	keep := map[string]any{"title": seeded.Title + " (baru)"}
	if err := act(seeded.OwnerID, "updateListing", seeded.OwnerID, seeded.ID, keep); err != nil {
		t.Fatalf("update keeping the seeded photos: %v", err)
	}
	if err := act(seeded.OwnerID, "updateListing", seeded.OwnerID, seeded.ID, map[string]any{"photos": append(seeded.Photos, "/img/listings/lain.webp")}); err == nil {
		t.Fatal("new /img photo outside the demo accepted")
	}
	if err := act(seeded.OwnerID, "updateListing", seeded.OwnerID, seeded.ID, map[string]any{"photos": append(seeded.Photos, "https://cdn.example/x.jpg")}); err == nil {
		t.Fatal("external photo on update accepted")
	}
	// The admin may put a photo they uploaded on someone's listing (moderation, onboarding).
	adminPhoto := e.photos(1, "referensi", "u-admin", nil)
	if err := act("u-admin", "updateListing", "u-admin", seeded.ID, map[string]any{"photos": append(seeded.Photos, adminPhoto[0])}); err != nil {
		t.Fatalf("admin photo: %v", err)
	}

	// CSV import outside the demo: /img and external URLs are row errors, photos uploaded by the admin pass.
	ref := e.photos(3, "referensi", "u-admin", nil)
	csv := importHeader + importRow("Hammock stok", "cod", seedPhotos) + importRow("Hammock luar", "cod", "https://x.example/a.jpg|"+ref[1]+"|"+ref[2])
	rep, _, err := prod.ImportListings(e.ctx, "u-admin", strings.NewReader(csv), true)
	if err != nil || rep.Failed != 2 || !strings.HasPrefix(rep.Rows[0].Errors[0], "photos: ") || !strings.HasPrefix(rep.Rows[1].Errors[0], "photos: ") {
		t.Fatalf("import with stock/external photos: %+v %v", rep, err)
	}
	rep, _, err = prod.ImportListings(e.ctx, "u-admin", strings.NewReader(importHeader+importRow("Hammock asli", "cod", strings.Join(ref, "|"))), false)
	if err != nil || rep.Failed != 0 || rep.Created != 1 {
		t.Fatalf("import with uploaded photos: %+v %v", rep, err)
	}
}
