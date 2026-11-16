package kyc

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"

	"github.com/mrfroglab/sewain-api/internal/auth"
)

type failingAuditor struct{}

func (failingAuditor) Audit(context.Context, string, string, string, *string, *string) error {
	return errors.New("audit db down")
}

// Finding K3: when auditing failed, ReadFile logged the submission id from the URL path verbatim. PathValue
// is URL-decoded, so ".../kyc/x%0Akyc: audit ok/files/ktp" wrote a forged second line into the log that
// is supposed to prove who looked at identity documents.
func TestReadFileAuditFailureLogIsNotInjectable(t *testing.T) {
	var buf bytes.Buffer
	prev, flags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() { log.SetOutput(prev); log.SetFlags(flags) }()

	s := &Service{Auditor: failingAuditor{}}
	admin := auth.Viewer{UserID: "u-admin", Role: auth.RoleAdmin}
	_, _, err := s.ReadFile(context.Background(), admin, "x\nkyc: forged line", "ktp\r\n")
	if !errors.Is(err, errAuditFailed) {
		t.Fatalf("err = %v", err)
	}
	if out := strings.TrimRight(buf.String(), "\n"); strings.ContainsAny(out, "\r\n") {
		t.Fatalf("log line contains raw line breaks: %q", out)
	}
}
