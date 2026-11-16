package service_test

import "testing"

// Review wave 2 (W1): offerWanted accepted any listing id, so a user could answer a request with someone else's
// listing (the requester is sent to that listing under the offerer's name) or with a hidden/unknown listing.
func TestOfferWantedOnlyOwnListing(t *testing.T) {
	e := newEnv(t)
	w := ""
	for _, x := range e.state().Wanted {
		if x.Status == "terbuka" && x.RequesterID != "u-raka" && x.RequesterID != "u-bayu" {
			w = x.ID
			break
		}
	}
	if w == "" {
		t.Fatal("no open wanted post in the seed")
	}
	if _, err := e.try("u-raka", "offerWanted", "u-raka", w, map[string]any{"listingId": "l-tenda-dome-4p", "message": "punyaku"}); err == nil {
		t.Error("offer with another provider's listing accepted")
	}
	if _, err := e.try("u-raka", "offerWanted", "u-raka", w, map[string]any{"listingId": "l-nope", "message": "x"}); err == nil {
		t.Error("offer with an unknown listing accepted")
	}
	e.must("u-bayu", "offerWanted", "u-bayu", w, map[string]any{"listingId": "l-tenda-dome-4p", "message": "ada"})
	e.must("u-raka", "offerWanted", "u-raka", w, map[string]any{"message": "aku bisa pinjamkan"})
}
