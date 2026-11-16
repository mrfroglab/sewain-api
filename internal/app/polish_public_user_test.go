package app_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/mrfroglab/sewain-api/internal/app/apptest"
)

// TestPublicUserEndpoint: GET /api/v1/users/{id} (web /u/[id] fallback with SNAPSHOT_USERS=referenced). The user
// is exactly the users[] entry of a guest snapshot (publicStats, demo in DEMO_MODE), whoever asks; listings are the
// active ones with the public location; reviews are the shown ones about the user. Deleted and blacklisted users
// are 404.
func TestPublicUserEndpoint(t *testing.T) {
	a := apptest.New(t, apptest.Options{Demo: true})
	var guestState struct {
		Users    []map[string]any `json:"users"`
		Listings []map[string]any `json:"listings"`
	}
	if err := json.Unmarshal(a.Get(nil, "/api/v1/state").Raw, &guestState); err != nil {
		t.Fatal(err)
	}
	entry := func(id string) map[string]any {
		for _, u := range guestState.Users {
			if u["id"] == id {
				return u
			}
		}
		t.Fatalf("%s not in the guest snapshot", id)
		return nil
	}
	bayu := a.DemoLogin("u-bayu")
	for _, s := range []*apptest.Session{nil, bayu, a.DemoLogin("u-raka")} {
		r := a.Get(s, "/api/v1/users/u-bayu")
		if r.Status != 200 || r.Body["ok"] != true || r.Header.Get("Cache-Control") != "public, max-age=60" {
			t.Fatalf("status %d, cache %q: %s", r.Status, r.Header.Get("Cache-Control"), r.Raw)
		}
		user := r.Body["user"].(map[string]any)
		if !reflect.DeepEqual(user, entry("u-bayu")) {
			t.Fatalf("user differs from the guest snapshot entry:\n%v\n%v", user, entry("u-bayu"))
		}
		if user["phone"] != "" || user["publicStats"] == nil || user["demo"] == nil {
			t.Fatalf("projection: %v", user)
		}
		listings := r.Body["listings"].([]any)
		if len(listings) == 0 {
			t.Fatal("no listings")
		}
		for _, x := range listings {
			l := x.(map[string]any)
			if l["ownerId"] != "u-bayu" || l["status"] != "aktif" || l["meetingPoint"] != nil {
				t.Fatalf("listing: %v", l)
			}
			for _, g := range guestState.Listings {
				if g["id"] == l["id"] && !reflect.DeepEqual(g["location"], l["location"]) {
					t.Fatalf("listing %v location %v, guest snapshot %v", l["id"], l["location"], g["location"])
				}
			}
		}
		reviews := r.Body["reviews"].([]any)
		if len(reviews) == 0 {
			t.Fatal("no reviews")
		}
		for _, x := range reviews {
			if x.(map[string]any)["toId"] != "u-bayu" {
				t.Fatalf("review about someone else: %v", x)
			}
		}
	}

	notFound := func(id string) {
		t.Helper()
		r := a.Get(nil, "/api/v1/users/"+id)
		if r.Status != 404 || r.Body["ok"] != false || r.Body["error"] != "Pengguna tidak ditemukan." {
			t.Fatalf("%s: %d %s", id, r.Status, r.Raw)
		}
	}
	notFound("u-tidak-ada")
	adm := a.DemoLogin("u-admin")
	a.OK(adm, "setUserStatus", "u-admin", "u-arif", "blacklist", "Penipuan berulang")
	notFound("u-arif")
	maya := a.DemoLogin("u-maya")
	if r := a.Do(maya, apptest.Req{Method: "DELETE", Path: "/api/v1/me"}); r.Status != 200 {
		t.Fatalf("delete: %d %s", r.Status, r.Raw)
	}
	notFound("u-maya")
}
