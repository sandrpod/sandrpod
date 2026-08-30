package aws

import (
	"context"
	"testing"
	"time"
)

// The agent-registration bound used to be skipped whenever the caller already
// had a deadline — and the scheduler always does, with a 20-minute budget for
// the whole provision. An instance that will never become SSM-managed (no IAM
// instance profile) therefore retried for the full twenty minutes, billing
// throughout. Measured on a real account before the fix: 20 minutes.
//
// This pins the shape the fix relies on — WithTimeout takes whichever deadline
// is sooner — so applying the bound unconditionally cannot let a call outlive
// its caller's budget.
func TestRegistrationBoundNeverOutlivesCaller(t *testing.T) {
	long, cancelLong := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancelLong()
	bounded, cancel := context.WithTimeout(long, ssmRegistrationTimeout)
	defer cancel()

	dl, ok := bounded.Deadline()
	if !ok {
		t.Fatal("bounded context has no deadline")
	}
	if remaining := time.Until(dl); remaining > ssmRegistrationTimeout+time.Second {
		t.Errorf("registration wait would run %v, want at most %v — it inherited the caller's budget",
			remaining.Round(time.Second), ssmRegistrationTimeout)
	}

	// The other direction: a caller with less time than the bound keeps its own.
	short, cancelShort := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShort()
	inner, cancelInner := context.WithTimeout(short, ssmRegistrationTimeout)
	defer cancelInner()
	if dl, _ := inner.Deadline(); time.Until(dl) > 10*time.Second {
		t.Error("a caller with a shorter deadline had it widened by the bound")
	}
}
