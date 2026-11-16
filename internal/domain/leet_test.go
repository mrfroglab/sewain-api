package domain_test

import (
	"strings"
	"testing"
	"time"

	"github.com/mrfroglab/sewain-api/internal/domain"
	"github.com/mrfroglab/sewain-api/internal/seed"
)

// Polish (review wave 2, L3): simple leetspeak and stretched letters no longer hide a banned keyword.
func TestBannedKeywordsLeetspeak(t *testing.T) {
	banned := []string{"airsoft", "pistol", "senjata", "kucing", "obat keras", "sabu", "petasan"}
	for text, want := range map[string]string{
		"Jual 4irs0ft murah":   "airsoft",
		"a1rsoft gun":          "airsoft",
		"P1ST0L mainan":        "pistol",
		"pi5tol":               "pistol",
		"s3nj4t4 tajam":        "senjata",
		"anak kuuucing lucu":   "kucing",
		"kuc1ng persia":        "kucing",
		"0b4t k3r4s":           "obat keras",
		"5abu":                 "sabu",
		"p37454n tahun baru":   "petasan",
		"4 i r s 0 f t":        "airsoft",
		"airrrsoooft":          "airsoft",
		"P.1.S.T.0.L":          "pistol",
		"pistooool":            "pistol",
		"Ａ1ＲＳＯＦＴ (full width)": "airsoft",
	} {
		got := domain.BannedWordsIn(text, banned)
		if len(got) != 1 || got[0] != want {
			t.Errorf("%q: got %v, want [%s]", text, got, want)
		}
	}
	// Numbers on their own are not letters: "tenda 4 orang" is not "a orang", "500 W" stays a number.
	for _, text := range []string{"Tenda 4 orang, 2 pintu", "Rice cooker 500 W 1,8 L", "Kamera 24 MP + lensa 50mm f/1.8", "Motor 110 cc tahun 2019"} {
		if got := domain.BannedWordsIn(text, banned); len(got) > 0 {
			t.Errorf("false positive %q: %v", text, got)
		}
	}
}

// The leetspeak and stretched-letter readings must not flag the ordinary Indonesian text of the demo catalog: every
// seeded listing (title, description, condition notes, special rules) and wanted post passes the seed's banned list.
func TestBannedKeywordsNoFalsePositiveOnSeed(t *testing.T) {
	d, err := seed.Build(time.Now().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if len(d.BannedKeywords) == 0 || len(d.Listings) < 30 {
		t.Fatalf("seed: %d keywords, %d listings", len(d.BannedKeywords), len(d.Listings))
	}
	checked := 0
	for _, l := range d.Listings {
		if l.ID == "l-flagged-airsoft" { // the seed's deliberately banned listing (moderation demo)
			if got := domain.BannedWordsIn(l.Title+"\n"+l.Description, d.BannedKeywords); len(got) == 0 {
				t.Error("the flagged demo listing is no longer caught")
			}
			continue
		}
		for _, text := range []string{l.Title, l.Description, l.ConditionNotes, domain.Deref(l.Rules.Special, ""), l.Rules.Cancellation, domain.Deref(l.MeetingPoint, "")} {
			if got := domain.BannedWordsIn(text, d.BannedKeywords); len(got) > 0 {
				t.Errorf("%s: %v in %q", l.ID, got, text)
			}
			checked++
		}
	}
	for _, w := range d.Wanted {
		if got := domain.BannedWordsIn(w.Title+"\n"+w.Description, d.BannedKeywords); len(got) > 0 {
			t.Errorf("wanted %s: %v", w.ID, got)
		}
	}
	// Plus everyday words with doubled letters and digits that the readings touch.
	for _, text := range []string{"Maaf, saat ini taat aturan", "Kipas angin 3 speed", "Laptop i5 RAM 8 GB", "Sepatu no 37 dan 40", "Tas 45L anti air", "Sound 7.1 surround"} {
		if got := domain.BannedWordsIn(text, d.BannedKeywords); len(got) > 0 {
			t.Errorf("false positive %q: %v", text, got)
		}
	}
	if checked < 100 || !strings.Contains(strings.Join(d.BannedKeywords, ","), "airsoft") {
		t.Fatalf("checked %d texts", checked)
	}
}
