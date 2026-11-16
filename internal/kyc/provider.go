package kyc

import "context"

// File is an uploaded image.
type File struct {
	Data        []byte
	ContentType string
}

// VerifyRequest is what a KYCProvider receives. It carries plaintext and must never be logged or persisted
// by the provider beyond what its vendor contract requires.
type VerifyRequest struct {
	SubmissionID string
	UserID       string
	NIK          string
	NameOnKTP    string
	KTP          File
	Selfie       File
}

// Check is one automated check (OCR, liveness, face match, Dukcapil).
type Check struct {
	Name   string  `json:"name"`
	Passed bool    `json:"passed"`
	Score  float64 `json:"score,omitempty"`
	Detail string  `json:"detail,omitempty"`
}

// VerifyResult is a provider's verdict. Decision is StatusPending (queue for an admin), StatusApproved or
// StatusRejected (Reason is then shown to the user, in Indonesian).
type VerifyResult struct {
	Decision string
	Reason   string
	Checks   []Check
}

// KYCProvider verifies identities automatically (OCR + liveness + face match + Dukcapil, FR-A4). Vendor
// implementations arrive in Phase 8; ManualProvider queues everything for admin review. A provider error
// falls back to the manual queue.
type KYCProvider interface {
	Name() string
	Verify(ctx context.Context, req VerifyRequest) (VerifyResult, error)
}

// ManualProvider sends every submission to the admin queue.
type ManualProvider struct{}

func (ManualProvider) Name() string { return "manual" }

func (ManualProvider) Verify(context.Context, VerifyRequest) (VerifyResult, error) {
	return VerifyResult{Decision: StatusPending}, nil
}
