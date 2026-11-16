package app_test

import (
	"testing"

	"github.com/mrfroglab/sewain-api/internal/app/apptest"
)

// Review wave 2 (P1): "Pakai foto contoh" registered a bundled stock image as evidence, with a server hash and
// timestamp, for any signed-in user in every environment. Outside DEMO_MODE a provider could document a hand-over
// with stock pictures instead of the real item.
func TestDemoPhotoOnlyInDemo(t *testing.T) {
	a := apptest.New(t, apptest.Options{})
	s := a.OTPLogin("081211119001")
	r := a.Do(s, apptest.Req{Method: "POST", Path: "/api/v1/photos/demo", JSON: map[string]any{"url": "/img/listings/tenda-dome-4p-1.webp", "stage": "chat"}})
	if r.Status != 403 {
		t.Fatalf("demo photo outside DEMO_MODE: %d %s", r.Status, r.Raw)
	}
}
