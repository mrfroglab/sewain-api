// Package ekyc automates identity verification with an eKYC vendor (ROADMAP F8.5, PRD FR-A4): KTP OCR,
// liveness, face match and a Dukcapil data check. Provider adapts a Vendor to kyc.KYCProvider, so the
// existing KYC flow (encrypted upload, admin queue, one NIK one account) stays the single path; anything
// short of a clean pass goes to the manual admin queue.
package ekyc

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mrfroglab/sewain-api/internal/kyc"
)

// Steps and their price per call in rupiah (PRD §15.2, Xendit list price [S*]: OCR 500 + liveness 2.000 +
// data authentication 4.000 + face match 50 = 6.550 per user). Verihubs, VIDA, Privy, ASLI RI: by quote.
const (
	StepOCR       = "ocr_ktp"
	StepLiveness  = "liveness"
	StepFaceMatch = "face_match"
	StepDukcapil  = "dukcapil"
)

// XenditCosts is the per-call price list from PRD §15.2.
var XenditCosts = map[string]int64{StepOCR: 500, StepLiveness: 2000, StepDukcapil: 4000, StepFaceMatch: 50}

// ErrNotConfigured means the vendor has no contract/credentials yet: the submission goes to the admin queue.
var ErrNotConfigured = errors.New("ekyc: vendor not configured")

// OCRResult is what a vendor read from the KTP.
type OCRResult struct {
	NIK        string
	Name       string
	Confidence float64 // 0..1
}

// DukcapilResult is the population registry check (NIK + name, optionally selfie vs. registry photo).
type DukcapilResult struct {
	NIKFound   bool
	NameMatch  bool
	PhotoScore float64 // 0..1, 0 when the vendor does not compare photos
}

// Vendor is one eKYC vendor. Implementations must not log or keep the images beyond the vendor contract.
type Vendor interface {
	Name() string
	OCRKTP(ctx context.Context, ktp kyc.File) (OCRResult, error)
	Liveness(ctx context.Context, selfie kyc.File) (score float64, err error)
	FaceMatch(ctx context.Context, ktp, selfie kyc.File) (score float64, err error)
	Dukcapil(ctx context.Context, nik, name string, selfie kyc.File) (DukcapilResult, error)
}

// Thresholds for an automatic approval. Below them the submission is queued for an admin, never rejected
// automatically (a false rejection blocks a real student; a human looks instead).
type Thresholds struct {
	OCRConfidence float64
	Liveness      float64
	FaceMatch     float64
}

// DefaultThresholds are conservative placeholders until vendor sandbox data calibrates them.
var DefaultThresholds = Thresholds{OCRConfidence: 0.85, Liveness: 0.9, FaceMatch: 0.8}

// Provider implements kyc.KYCProvider on top of a Vendor. Steps run cheapest-informative first and stop at
// the first failing check, so a doomed submission does not pay for the expensive Dukcapil call.
type Provider struct {
	Vendor     Vendor
	Thresholds Thresholds
	// OnCost, when set, is called once per billed vendor call (for cost reporting).
	OnCost func(vendor, step string, rupiah int64)
	Costs  map[string]int64
}

var _ kyc.KYCProvider = (*Provider)(nil)

// Name implements kyc.KYCProvider.
func (p *Provider) Name() string { return "ekyc-" + p.Vendor.Name() }

func (p *Provider) bill(step string) {
	if p.OnCost != nil {
		p.OnCost(p.Vendor.Name(), step, p.Costs[step])
	}
}

// Verify implements kyc.KYCProvider. Result: approved when every check passes; pending otherwise (with the
// checks for the admin); an error when the vendor fails (the kyc service then falls back to the queue).
func (p *Provider) Verify(ctx context.Context, req kyc.VerifyRequest) (kyc.VerifyResult, error) {
	th := p.Thresholds
	if th == (Thresholds{}) {
		th = DefaultThresholds
	}
	pending := func(checks []kyc.Check) (kyc.VerifyResult, error) {
		return kyc.VerifyResult{Decision: kyc.StatusPending, Checks: checks}, nil
	}
	var checks []kyc.Check
	step := func(err error) (stop bool, res kyc.VerifyResult, rerr error) {
		if errors.Is(err, ErrNotConfigured) {
			res, rerr = pending(checks)
			return true, res, rerr
		}
		if err != nil {
			return true, kyc.VerifyResult{}, fmt.Errorf("ekyc %s: %w", p.Vendor.Name(), err)
		}
		return false, kyc.VerifyResult{}, nil
	}
	if err := ctx.Err(); err != nil {
		return kyc.VerifyResult{}, err
	}

	ocr, err := p.Vendor.OCRKTP(ctx, req.KTP)
	if stop, res, err := step(err); stop {
		return res, err
	}
	p.bill(StepOCR)
	ocrOK := ocr.Confidence >= th.OCRConfidence && ocr.NIK == req.NIK && sameName(ocr.Name, req.NameOnKTP)
	checks = append(checks, kyc.Check{Name: StepOCR, Passed: ocrOK, Score: ocr.Confidence, Detail: ocrDetail(ocr, req)})
	if !ocrOK {
		return pending(checks)
	}

	fm, err := p.Vendor.FaceMatch(ctx, req.KTP, req.Selfie)
	if stop, res, err := step(err); stop {
		return res, err
	}
	p.bill(StepFaceMatch)
	checks = append(checks, kyc.Check{Name: StepFaceMatch, Passed: fm >= th.FaceMatch, Score: fm})
	if fm < th.FaceMatch {
		return pending(checks)
	}

	lv, err := p.Vendor.Liveness(ctx, req.Selfie)
	if stop, res, err := step(err); stop {
		return res, err
	}
	p.bill(StepLiveness)
	checks = append(checks, kyc.Check{Name: StepLiveness, Passed: lv >= th.Liveness, Score: lv})
	if lv < th.Liveness {
		return pending(checks)
	}

	dk, err := p.Vendor.Dukcapil(ctx, req.NIK, req.NameOnKTP, req.Selfie)
	if stop, res, err := step(err); stop {
		return res, err
	}
	p.bill(StepDukcapil)
	dkOK := dk.NIKFound && dk.NameMatch
	checks = append(checks, kyc.Check{Name: StepDukcapil, Passed: dkOK, Score: dk.PhotoScore})
	if !dkOK {
		return pending(checks)
	}
	if err := ctx.Err(); err != nil { // never approve a cancelled verification
		return kyc.VerifyResult{}, err
	}
	return kyc.VerifyResult{Decision: kyc.StatusApproved, Checks: checks}, nil
}

func sameName(a, b string) bool {
	norm := func(s string) string { return strings.Join(strings.Fields(strings.ToUpper(s)), " ") }
	return norm(a) != "" && norm(a) == norm(b)
}

// ocrDetail explains a mismatch without echoing the NIK or name (checks are shown to admins and stored).
func ocrDetail(ocr OCRResult, req kyc.VerifyRequest) string {
	var d []string
	if ocr.NIK != req.NIK {
		d = append(d, "NIK di KTP tidak sama dengan yang diisi")
	}
	if !sameName(ocr.Name, req.NameOnKTP) {
		d = append(d, "nama di KTP tidak sama dengan yang diisi")
	}
	return strings.Join(d, "; ")
}

// Manual is the stub vendor used until a contract exists: every call is ErrNotConfigured, so every
// submission goes to the admin queue without cost.
type Manual struct{}

// Name implements Vendor.
func (Manual) Name() string { return "manual" }

// OCRKTP implements Vendor.
func (Manual) OCRKTP(context.Context, kyc.File) (OCRResult, error) {
	return OCRResult{}, ErrNotConfigured
}

// Liveness implements Vendor.
func (Manual) Liveness(context.Context, kyc.File) (float64, error) { return 0, ErrNotConfigured }

// FaceMatch implements Vendor.
func (Manual) FaceMatch(context.Context, kyc.File, kyc.File) (float64, error) {
	return 0, ErrNotConfigured
}

// Dukcapil implements Vendor.
func (Manual) Dukcapil(context.Context, string, string, kyc.File) (DukcapilResult, error) {
	return DukcapilResult{}, ErrNotConfigured
}

// XenditEKYC and Verihubs are placeholders for the vendor adapters. Their endpoints are not public without a
// merchant account, so nothing is implemented until sandbox access exists (docs/integration/payments.md §7).
// Until then they behave like Manual.
type (
	XenditEKYC struct{ Manual }
	Verihubs   struct{ Manual }
)

// Name implements Vendor.
func (XenditEKYC) Name() string { return "xendit" }

// Name implements Vendor.
func (Verihubs) Name() string { return "verihubs" }
