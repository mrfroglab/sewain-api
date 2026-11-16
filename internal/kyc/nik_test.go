package kyc

import "testing"

func TestValidateNIK(t *testing.T) {
	for _, ok := range []string{"3404010101900001", "3404014112990123", "1101010112000001", "9471017112990001"} {
		if err := ValidateNIK(ok); err != nil {
			t.Errorf("ValidateNIK(%s) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "340401010190000", "34040101019000011", "34040101019000a1",
		"0004010101900001", "9904010101900001", "3404010001900001", "3404013201900001", "3404017201900001",
		"3404010113900001", "3404010100900001", "3404010101900000"} {
		if err := ValidateNIK(bad); err == nil {
			t.Errorf("ValidateNIK(%s) should fail", bad)
		}
	}
}

func TestMaskNIK(t *testing.T) {
	if got := MaskNIK("3404010101900001"); got != "3404********0001" {
		t.Fatal(got)
	}
}

func TestManualProviderQueues(t *testing.T) {
	res, err := ManualProvider{}.Verify(t.Context(), VerifyRequest{})
	if err != nil || res.Decision != StatusPending {
		t.Fatalf("%+v %v", res, err)
	}
}
