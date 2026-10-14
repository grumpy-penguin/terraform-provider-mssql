package sqlclient

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
)

var errPrincipalNotFound = mssql.Error{Number: sqlErrPrincipalNotFound, Message: "Principal 'uai-x' could not be found or this principal type is not supported."}

// fakeClock lets a retryPolicy run without real waits: sleep advances the clock and records
// each requested delay.
type fakeClock struct {
	t      time.Time
	delays []time.Duration
}

func (c *fakeClock) now() time.Time { return c.t }

func (c *fakeClock) sleep(_ context.Context, d time.Duration) error {
	c.delays = append(c.delays, d)
	c.t = c.t.Add(d)
	return nil
}

func testPolicy(c *fakeClock) retryPolicy {
	return retryPolicy{
		timeout:      5 * time.Minute,
		initialDelay: 5 * time.Second,
		maxDelay:     30 * time.Second,
		now:          c.now,
		sleep:        c.sleep,
	}
}

func TestIsPrincipalNotFound(t *testing.T) {
	if !IsPrincipalNotFound(errPrincipalNotFound) {
		t.Error("expected 33130 to be principal-not-found")
	}
	if !IsPrincipalNotFound(fmt.Errorf("creating user: %w", errPrincipalNotFound)) {
		t.Error("expected a wrapped 33130 to be principal-not-found")
	}
	if IsPrincipalNotFound(mssql.Error{Number: 15023}) {
		t.Error("15023 (user already exists) is not principal-not-found")
	}
	if IsPrincipalNotFound(errors.New("network down")) {
		t.Error("a non-SQL error is not principal-not-found")
	}
}

func TestRetrySucceedsAfterPropagation(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	calls := 0
	err := testPolicy(clock).do(context.Background(), IsPrincipalNotFound, func(context.Context) error {
		calls++
		if calls < 4 {
			return errPrincipalNotFound
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 4 {
		t.Errorf("calls = %d, want 4", calls)
	}
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second}
	if fmt.Sprint(clock.delays) != fmt.Sprint(want) {
		t.Errorf("delays = %v, want %v (exponential backoff)", clock.delays, want)
	}
}

func TestRetryDoesNotRetryOtherErrors(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	calls := 0
	permissionDenied := mssql.Error{Number: 15247, Message: "User does not have permission to perform this action."}
	err := testPolicy(clock).do(context.Background(), IsPrincipalNotFound, func(context.Context) error {
		calls++
		return permissionDenied
	})
	if calls != 1 || len(clock.delays) != 0 {
		t.Errorf("calls = %d, waits = %d; a non-retryable error must fail at once", calls, len(clock.delays))
	}
	var sqlErr mssql.Error
	if !errors.As(err, &sqlErr) || sqlErr.Number != 15247 {
		t.Errorf("err = %v, want the original 15247 error", err)
	}
}

func TestRetryGivesUpAtTimeout(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	calls := 0
	err := testPolicy(clock).do(context.Background(), IsPrincipalNotFound, func(context.Context) error {
		calls++
		return errPrincipalNotFound
	})
	if err == nil {
		t.Fatal("expected an error once the timeout passes")
	}
	if !IsPrincipalNotFound(err) {
		t.Errorf("the final error should still wrap 33130, got %v", err)
	}
	var total time.Duration
	for _, d := range clock.delays {
		if d > 30*time.Second {
			t.Errorf("delay %s exceeds maxDelay", d)
		}
		total += d
	}
	if total > 5*time.Minute {
		t.Errorf("waited %s in total, past the 5m timeout", total)
	}
	if calls < 5 {
		t.Errorf("calls = %d; expected several attempts within 5 minutes", calls)
	}
}

func TestRetryStopsWhenContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	policy := retryPolicy{
		timeout:      5 * time.Minute,
		initialDelay: time.Hour, // only a cancelled context can end this wait
		maxDelay:     time.Hour,
		now:          time.Now,
		sleep:        sleepContext,
	}
	cancel()
	err := policy.do(ctx, IsPrincipalNotFound, func(context.Context) error {
		calls++
		return errPrincipalNotFound
	})
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
	if err == nil || !IsPrincipalNotFound(err) {
		t.Errorf("err = %v, want the 33130 error noting the retry stopped", err)
	}
}
