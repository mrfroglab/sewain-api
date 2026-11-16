// Package kyctest holds test doubles for the kyc package (in-memory blob store and auditor) and a contract
// test every KYCProvider implementation must pass.
package kyctest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mrfroglab/sewain-api/internal/kyc"
)

// ErrNotFound is returned by MemBlobStore.Get for missing keys.
var ErrNotFound = errors.New("blob not found")

// MemBlobStore is an in-memory kyc.BlobStore.
type MemBlobStore struct {
	mu    sync.Mutex
	Items map[string][]byte
}

func NewMemBlobStore() *MemBlobStore { return &MemBlobStore{Items: map[string][]byte{}} }

func (m *MemBlobStore) Put(_ context.Context, key string, data []byte, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Items[key] = append([]byte(nil), data...)
	return nil
}

func (m *MemBlobStore) Get(_ context.Context, key string) ([]byte, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.Items[key]
	if !ok {
		return nil, "", ErrNotFound
	}
	return append([]byte(nil), b...), "application/octet-stream", nil
}

func (m *MemBlobStore) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.Items, key)
	return nil
}

func (m *MemBlobStore) Exists(_ context.Context, key string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.Items[key]
	return ok, nil
}

// Snapshot returns a copy of all stored blobs.
func (m *MemBlobStore) Snapshot() map[string][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string][]byte, len(m.Items))
	for k, v := range m.Items {
		out[k] = append([]byte(nil), v...)
	}
	return out
}

// AuditEntry is one recorded audit call.
type AuditEntry struct {
	ActorID, Action, Target string
	Old, New                *string
	At                      time.Time
}

// MemAuditor is an in-memory kyc.Auditor. Set Fail to make every call fail.
type MemAuditor struct {
	mu      sync.Mutex
	Entries []AuditEntry
	Fail    bool
}

func (a *MemAuditor) Audit(_ context.Context, actorID, action, target string, oldValue, newValue *string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.Fail {
		return errors.New("audit unavailable")
	}
	a.Entries = append(a.Entries, AuditEntry{actorID, action, target, oldValue, newValue, time.Now()})
	return nil
}

// Find returns entries with the given action.
func (a *MemAuditor) Find(action string) []AuditEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []AuditEntry
	for _, e := range a.Entries {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

// ProviderContract checks the KYCProvider contract: a stable non-empty name, a valid decision for a
// well-formed request, a reason on rejection, and respect for context cancellation (an error or a pending
// verdict, never an approval).
func ProviderContract(t *testing.T, p kyc.KYCProvider, sample kyc.VerifyRequest) {
	t.Helper()
	if name := p.Name(); name == "" || name != p.Name() {
		t.Fatal("provider name must be stable and non-empty")
	}
	res, err := p.Verify(context.Background(), sample)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	switch res.Decision {
	case kyc.StatusPending, kyc.StatusApproved:
	case kyc.StatusRejected:
		if res.Reason == "" {
			t.Fatal("rejection needs a user-facing reason")
		}
	default:
		t.Fatalf("invalid decision %q", res.Decision)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err = p.Verify(ctx, sample)
	if err == nil && res.Decision == kyc.StatusApproved {
		t.Fatal("a cancelled verification must not approve")
	}
}
