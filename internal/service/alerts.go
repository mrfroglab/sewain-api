package service

import (
	"context"

	"github.com/mrfroglab/sewain-api/internal/domain"
)

// NotifyAdmins sends one in-app notification (queued for push/email like any other) to every admin account, for
// operational alerts raised outside the core, such as the OTP daily send cap.
func (s *Service) NotifyAdmins(ctx context.Context, kind, title, body, href string) error {
	_, err := s.run(ctx, func(t *txn) error {
		t.notify(domain.AdminID, kind, title, body, href)
		return nil
	})
	return err
}
