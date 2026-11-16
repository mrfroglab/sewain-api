package media

import "testing"

func TestCanView(t *testing.T) {
	b := &Booking{ID: "b-1", RenterID: "u-renter", ProviderID: "u-provider"}
	other := &Booking{ID: "b-2", RenterID: "u-stranger", ProviderID: "u-provider2"}
	renter, provider := &Viewer{ID: "u-renter"}, &Viewer{ID: "u-provider"}
	stranger, admin := &Viewer{ID: "u-stranger"}, &Viewer{ID: "u-admin", Admin: true}

	ref := Photo{ID: "p-ref", Stage: StageReferensi, ListingID: "l-1", UploaderID: "u-provider"}
	for _, v := range []*Viewer{nil, renter, stranger, admin} {
		if !CanView(v, ref, nil) {
			t.Fatalf("listing photo hidden from %+v", v)
		}
	}

	for _, stage := range []string{StageCheckout, StageCheckin, StageJaminan, StageMasalah, StageSengketa, StageChat} {
		p := Photo{ID: "p-" + stage, Stage: stage, BookingID: "b-1", UploaderID: "u-renter"}
		for _, tc := range []struct {
			name    string
			v       *Viewer
			booking *Booking
			want    bool
		}{
			{"renter", renter, b, true},
			{"provider", provider, b, true},
			{"admin", admin, b, true},
			{"admin without booking row", admin, nil, true},
			{"guest", nil, b, false},
			{"empty viewer id", &Viewer{}, b, false},
			{"other user", stranger, b, false},
			{"party of another booking", stranger, other, false},
			{"renter but booking missing", renter, nil, false},
			{"renter with mismatched booking", renter, &Booking{ID: "b-2", RenterID: "u-renter"}, false},
		} {
			if got := CanView(tc.v, p, tc.booking); got != tc.want {
				t.Errorf("%s/%s: got %v want %v", stage, tc.name, got, tc.want)
			}
		}
	}

	for _, stage := range []string{"ktp", "selfie", "kyc", "", "lainnya"} {
		p := Photo{ID: "p-x", Stage: stage, BookingID: "b-1", UploaderID: "u-renter"}
		for _, v := range []*Viewer{nil, renter, admin} {
			if CanView(v, p, b) {
				t.Errorf("stage %q visible to %+v", stage, v)
			}
		}
	}
}
