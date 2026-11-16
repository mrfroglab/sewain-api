package service_test

import (
	"testing"
)

// Review wave 2 (D1): DELETE /api/v1/me anonymised the profile but kept the verification request with the name on the
// KTP and the masked NIK (UU PDP: identity data of a deleted account).
func TestDeleteAccountErasesVerificationRequests(t *testing.T) {
	e := newEnv(t)
	e.must("u-bima", "submitVerification", "u-bima", map[string]any{"method": "ekyc", "nameOnKtp": "Bima Sakti", "nik": "3404010101010099"})
	if _, err := e.svc.DeleteAccount(e.ctx, "u-bima"); err != nil {
		t.Fatal(err)
	}
	for _, v := range e.state().Verifications {
		if v.UserID != "u-bima" {
			continue
		}
		if v.NameOnKtp != "" || v.NikMasked != "" {
			t.Fatalf("verification request of a deleted account keeps identity data: %q %q", v.NameOnKtp, v.NikMasked)
		}
	}
}
