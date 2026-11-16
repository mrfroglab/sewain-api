package service_test

import (
	"strings"
	"testing"
)

// Review wave 2 (K1): the keyword filter (FR-L4) was bypassed by spreading letters, full-width or accented letters and
// look-alike Cyrillic letters, and it ignored the condition notes and special rules shown on the listing page.
func TestBannedKeywordsBypass(t *testing.T) {
	e := newEnv(t)
	for _, title := range []string{"Jual a i r s o f t murah", "A.I.R.S.O.F.T gun", "ＡＩＲＳＯＦＴ", "Aírsoft gun", "Аirsoft gun" /* Cyrillic А */, "air-soft gun"} {
		in := listingInput(title)
		if msg := e.reject("u-raka", "createListing", "u-raka", in); !strings.HasPrefix(msg, "Ada kata yang dilarang") {
			t.Errorf("%q: %q", title, msg)
		}
	}
	in := listingInput("Tenda kecil 2 orang")
	in["conditionNotes"] = "Bonus airsoft gun"
	if msg := e.reject("u-raka", "createListing", "u-raka", in); !strings.HasPrefix(msg, "Ada kata yang dilarang") {
		t.Errorf("condition notes: %q", msg)
	}
	// No false positive for words that merely contain the keyword's letters.
	e.must("u-raka", "createListing", "u-raka", listingInput("Kursi lipat airy software bag"))
	e.must("u-raka", "createListing", "u-raka", listingInput("Kipas: fresh air, soft breeze"))
}
