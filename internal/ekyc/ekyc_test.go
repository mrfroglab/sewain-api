package ekyc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mrfroglab/sewain-api/internal/kyc"
	"github.com/mrfroglab/sewain-api/internal/kyc/kyctest"
)

type fakeVendor struct {
	ocr        OCRResult
	live, face float64
	dk         DukcapilResult
	err        error
	calls      []string
}

func (f *fakeVendor) Name() string { return "fake" }
func (f *fakeVendor) OCRKTP(context.Context, kyc.File) (OCRResult, error) {
	f.calls = append(f.calls, StepOCR)
	return f.ocr, f.err
}
func (f *fakeVendor) Liveness(context.Context, kyc.File) (float64, error) {
	f.calls = append(f.calls, StepLiveness)
	return f.live, f.err
}
func (f *fakeVendor) FaceMatch(context.Context, kyc.File, kyc.File) (float64, error) {
	f.calls = append(f.calls, StepFaceMatch)
	return f.face, f.err
}
func (f *fakeVendor) Dukcapil(context.Context, string, string, kyc.File) (DukcapilResult, error) {
	f.calls = append(f.calls, StepDukcapil)
	return f.dk, f.err
}

func good() *fakeVendor {
	return &fakeVendor{ocr: OCRResult{NIK: "3404010101010001", Name: "Raka  Pratama", Confidence: 0.97}, live: 0.99, face: 0.93,
		dk: DukcapilResult{NIKFound: true, NameMatch: true}}
}

var sample = kyc.VerifyRequest{SubmissionID: "ks-1", UserID: "u-1", NIK: "3404010101010001", NameOnKTP: "raka pratama",
	KTP: kyc.File{Data: []byte{0xff, 0xd8}, ContentType: "image/jpeg"}, Selfie: kyc.File{Data: []byte{0xff, 0xd8}, ContentType: "image/jpeg"}}

func TestProviderContract(t *testing.T) {
	kyctest.ProviderContract(t, &Provider{Vendor: good(), Costs: XenditCosts}, sample)
	kyctest.ProviderContract(t, &Provider{Vendor: Manual{}}, sample)
	kyctest.ProviderContract(t, &Provider{Vendor: XenditEKYC{}}, sample)
	kyctest.ProviderContract(t, &Provider{Vendor: Verihubs{}}, sample)
}

func TestVerifyDecisionsAndCost(t *testing.T) {
	tests := []struct {
		name  string
		v     func() *fakeVendor
		want  string
		calls int
		cost  int64
	}{
		{"all pass", good, kyc.StatusApproved, 4, 6550},
		{"nik mismatch stops after OCR", func() *fakeVendor { v := good(); v.ocr.NIK = "x"; return v }, kyc.StatusPending, 1, 500},
		{"low OCR confidence", func() *fakeVendor { v := good(); v.ocr.Confidence = 0.5; return v }, kyc.StatusPending, 1, 500},
		{"face mismatch stops before liveness", func() *fakeVendor { v := good(); v.face = 0.2; return v }, kyc.StatusPending, 2, 550},
		{"liveness fails before Dukcapil", func() *fakeVendor { v := good(); v.live = 0.1; return v }, kyc.StatusPending, 3, 2550},
		{"Dukcapil: NIK not found", func() *fakeVendor { v := good(); v.dk.NIKFound = false; return v }, kyc.StatusPending, 4, 6550},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := tt.v()
			var cost int64
			p := &Provider{Vendor: v, Costs: XenditCosts, OnCost: func(_, _ string, r int64) { cost += r }}
			res, err := p.Verify(context.Background(), sample)
			if err != nil || res.Decision != tt.want || len(v.calls) != tt.calls || cost != tt.cost {
				t.Fatalf("decision %s calls %v cost %d err %v", res.Decision, v.calls, cost, err)
			}
			for _, c := range res.Checks {
				if c.Detail != "" && (strings.Contains(c.Detail, sample.NIK) || strings.Contains(strings.ToUpper(c.Detail), "PRATAMA")) {
					t.Fatal("check detail leaks identity data")
				}
			}
		})
	}
}

func TestVendorErrorAndManual(t *testing.T) {
	v := good()
	v.err = errors.New("timeout")
	if _, err := (&Provider{Vendor: v}).Verify(context.Background(), sample); err == nil {
		t.Fatal("vendor error must surface so kyc falls back to the manual queue")
	}
	res, err := (&Provider{Vendor: Manual{}}).Verify(context.Background(), sample)
	if err != nil || res.Decision != kyc.StatusPending || len(res.Checks) != 0 {
		t.Fatalf("%+v %v", res, err)
	}
}
