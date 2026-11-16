package service_test

import (
	"testing"
	"time"
)

// Review wave 2 (R1): a request past its 24 h expiry could still be accepted while the periodic job had not run yet
// (scheduler interval, a stopped runner, several instances): FR-R3 depends on the job's timing.
func TestAcceptAfterExpiryRefused(t *testing.T) {
	e := newEnv(t)
	e.clock.Add(25 * time.Hour) // no RunJobs
	if msg := e.reject("u-bayu", "acceptBooking", "u-bayu", "b-requested", nil); msg != "Request ini sudah kedaluwarsa." {
		t.Fatalf("accept after expiry: %q", msg)
	}
}
