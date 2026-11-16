package service_test

import (
	"testing"

	"github.com/mrfroglab/sewain-api/internal/domain"
	"github.com/mrfroglab/sewain-api/internal/service"
)

// TestSnapshotUsersReferenced: with SNAPSHOT_USERS=referenced a scoped snapshot carries exactly the profiles it
// refers to (with the same public data and reputation as "all"), the viewer's own profile in full, and never a
// user nothing points at. Admins still get everyone.
func TestSnapshotUsersReferenced(t *testing.T) {
	e := newEnv(t)
	all := service.New(e.svc.Pool(), service.Options{DemoMode: true, SnapshotScope: service.ScopeScoped,
		SnapshotUsers: service.SnapshotUsersAll, Now: e.clock.Now})
	// Outside DEMO_MODE: exactly the referenced profiles (the demo personas are added only in DEMO_MODE).
	ref := service.New(e.svc.Pool(), service.Options{DemoMode: false, SnapshotScope: service.ScopeScoped,
		SnapshotUsers: service.SnapshotUsersReferenced, Now: e.clock.Now})

	// A registered user with no listings, bookings or reviews: nobody refers to them.
	loner, err := e.svc.Dispatch(e.ctx, nil, "registerUser", args(domain.RegisterInput{Name: "Penyendiri", Phone: "089600000001", AreaID: "ugm"}), "u-loner")
	if err != nil {
		t.Fatal(err)
	}
	lonerID := loner.Value.(domain.ID)

	for _, tc := range []struct {
		name   string
		viewer *service.Viewer
	}{
		{"guest", nil},
		{"user", e.viewer("u-dina")},
		{"loner", &service.Viewer{UserID: lonerID, Role: "user"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, err := all.Snapshot(e.ctx, tc.viewer)
			if err != nil {
				t.Fatal(err)
			}
			r, err := ref.Snapshot(e.ctx, tc.viewer)
			if err != nil {
				t.Fatal(err)
			}
			me := ""
			if tc.viewer != nil {
				me = tc.viewer.UserID
			}
			want := map[string]bool{domain.AdminID: true}
			if me != "" {
				want[me] = true
			}
			for _, l := range r.Listings {
				want[l.OwnerID] = true
			}
			for _, x := range r.Reviews {
				want[x.FromID], want[x.ToID] = true, true
			}
			for _, w := range r.Wanted {
				want[w.RequesterID] = true
			}
			for _, b := range r.Bookings {
				want[b.RenterID], want[b.ProviderID] = true, true
			}
			byID := map[string]*domain.User{}
			for _, u := range a.Users {
				byID[u.ID] = u
			}
			got := map[string]bool{}
			for _, u := range r.Users {
				got[u.ID] = true
				if !want[u.ID] {
					t.Errorf("unreferenced user %s in snapshot", u.ID)
				}
				full := byID[u.ID]
				if full == nil {
					t.Fatalf("user %s missing from the all-users snapshot", u.ID)
				}
				if u.Name != full.Name || u.Phone != full.Phone || (u.PublicStats == nil) != (full.PublicStats == nil) ||
					(u.PublicStats != nil && *u.PublicStats != *full.PublicStats) {
					t.Errorf("user %s differs from the all-users snapshot", u.ID)
				}
			}
			for id := range want {
				if !got[id] && byID[id] != nil {
					t.Errorf("referenced user %s missing", id)
				}
			}
			if tc.name != "loner" && got[lonerID] {
				t.Errorf("%s sees the unreferenced user", tc.name)
			}
			if me != "" && byID[me] != nil && byID[me].Phone == "" {
				t.Errorf("own profile should be complete")
			}
			if len(r.Users) >= len(a.Users) {
				t.Errorf("referenced snapshot has %d users, all has %d", len(r.Users), len(a.Users))
			}
		})
	}

	admin, err := ref.Snapshot(e.ctx, e.viewer(domain.AdminID))
	if err != nil {
		t.Fatal(err)
	}
	a, _ := all.Snapshot(e.ctx, e.viewer(domain.AdminID))
	if len(admin.Users) != len(a.Users) {
		t.Errorf("admin: %d users, want all %d", len(admin.Users), len(a.Users))
	}
}

// TestSnapshotUsersDemoPersonas (default SNAPSHOT_USERS=referenced): in DEMO_MODE guests and users still get every
// demo persona (the persona picker and Demo panel need them), but not other unreferenced accounts.
func TestSnapshotUsersDemoPersonas(t *testing.T) {
	e := newEnv(t)
	svc := service.New(e.svc.Pool(), service.Options{DemoMode: true, SnapshotScope: service.ScopeScoped, Now: e.clock.Now}) // default users scope
	loner, err := e.svc.Dispatch(e.ctx, nil, "registerUser", args(domain.RegisterInput{Name: "Penyendiri", Phone: "089600000002", AreaID: "ugm"}), "u-loner2")
	if err != nil {
		t.Fatal(err)
	}
	var personas []string
	for _, u := range e.state().Users {
		if u.Demo != nil {
			personas = append(personas, u.ID)
		}
	}
	if len(personas) < 5 {
		t.Fatalf("seed personas: %v", personas)
	}
	for _, v := range []*service.Viewer{nil, e.viewer("u-bima")} {
		snap, err := svc.Snapshot(e.ctx, v)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]*domain.User{}
		for _, u := range snap.Users {
			got[u.ID] = u
		}
		for _, id := range personas {
			if got[id] == nil || got[id].Demo == nil {
				t.Errorf("viewer %v: persona %s missing (or without demo)", v, id)
			}
		}
		if got[loner.Value.(domain.ID)] != nil {
			t.Errorf("viewer %v: unreferenced non-demo account visible", v)
		}
	}
}
