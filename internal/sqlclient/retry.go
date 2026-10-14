package sqlclient

import (
	"context"
	"fmt"
	"time"
)

// sqlErrPrincipalNotFound is SQL Server error 33130: "Principal '<name>' could not be found or
// this principal type is not supported." Azure SQL raises it from CREATE USER ... FROM EXTERNAL
// PROVIDER when its Entra ID lookup of the name finds nothing.
const sqlErrPrincipalNotFound = 33130

// IsPrincipalNotFound reports whether err is SQL Server error 33130. Right after an Entra ID
// identity is created (a user-assigned managed identity, an app registration, a group) it can
// take seconds to minutes to become visible to SQL's directory lookup, so in that window this
// error is transient, not a misconfiguration.
func IsPrincipalNotFound(err error) bool {
	n, ok := errorNumber(err)
	return ok && n == sqlErrPrincipalNotFound
}

// retryPolicy retries an operation with exponential backoff while its error is retryable,
// giving up once the next wait would pass timeout. now and sleep are fields so tests can run
// the policy without real waits.
type retryPolicy struct {
	timeout      time.Duration
	initialDelay time.Duration
	maxDelay     time.Duration

	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
}

// principalPropagationRetry waits out Entra ID propagation for CREATE USER ... FROM EXTERNAL
// PROVIDER: 5s, 10s, 20s, then every 30s, for up to 5 minutes in total. Propagation normally
// finishes within a minute or two; a name that still isn't found after 5 minutes is far more
// likely to be wrong (a typo, the wrong tenant) than slow, so the error is returned then.
var principalPropagationRetry = retryPolicy{
	timeout:      5 * time.Minute,
	initialDelay: 5 * time.Second,
	maxDelay:     30 * time.Second,
	now:          time.Now,
	sleep:        sleepContext,
}

// do runs op until it succeeds, returns an error retryable rejects, the context ends, or the
// next wait would pass the timeout. The error returned after giving up wraps op's last error,
// so callers' error checks (errors.As, IsUniqueViolation) still see it.
func (p retryPolicy) do(ctx context.Context, retryable func(error) bool, op func(context.Context) error) error {
	deadline := p.now().Add(p.timeout)
	delay := p.initialDelay
	attempts := 0

	for {
		attempts++
		err := op(ctx)
		if err == nil || !retryable(err) {
			return err
		}
		if p.now().Add(delay).After(deadline) {
			return fmt.Errorf("still failing after %d attempts over %s: %w", attempts, p.timeout, err)
		}
		if sleepErr := p.sleep(ctx, delay); sleepErr != nil {
			return fmt.Errorf("%w (retry stopped: %v)", err, sleepErr)
		}

		delay *= 2
		if delay > p.maxDelay {
			delay = p.maxDelay
		}
	}
}

// sleepContext waits for d, or returns ctx's error if it ends first (Terraform cancelled, or a
// resource timeout passed).
func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
