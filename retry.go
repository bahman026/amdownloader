package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ----------------------------------------
// Error classification
//
// Retries exist to absorb transient faults. Retrying a permanent
// failure only multiplies load on a server we already know is
// going to reject us, so every failure is explicitly classified.
// ----------------------------------------

// permanentError marks a failure that cannot succeed on a retry:
// input validation, a 4xx other than 408/429, or an error the
// remote API reported in-band.
type permanentError struct {
	err error
}

func (e *permanentError) Error() string {
	return e.err.Error()
}

func (e *permanentError) Unwrap() error {
	return e.err
}

// permanent marks err as not worth retrying.
func permanent(err error) error {
	if err == nil {
		return nil
	}

	if isPermanent(err) {
		return err
	}

	return &permanentError{err: err}
}

func isPermanent(err error) bool {
	var target *permanentError

	return errors.As(err, &target)
}

// retryAfterError carries a server-supplied Retry-After hint so that
// backoff honours the rate limit the server actually asked for.
type retryAfterError struct {
	err   error
	after time.Duration
}

func (e *retryAfterError) Error() string {
	return e.err.Error()
}

func (e *retryAfterError) Unwrap() error {
	return e.err
}

func retryAfterOf(err error) (time.Duration, bool) {
	var target *retryAfterError

	if errors.As(err, &target) && target.after > 0 {
		return target.after, true
	}

	return 0, false
}

// ----------------------------------------
// Retry policy
// ----------------------------------------

type retryPolicy struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
}

func (p retryPolicy) normalized() retryPolicy {
	if p.MaxAttempts <= 0 {
		p.MaxAttempts = 3
	}

	// Bound the attempt count so the exponential shift below can
	// never overflow.
	if p.MaxAttempts > 10 {
		p.MaxAttempts = 10
	}

	if p.BaseDelay <= 0 {
		p.BaseDelay = 500 * time.Millisecond
	}

	if p.MaxDelay <= 0 {
		p.MaxDelay = 30 * time.Second
	}

	if p.MaxDelay < p.BaseDelay {
		p.MaxDelay = p.BaseDelay
	}

	return p
}

// delayFor returns an exponentially growing delay with full jitter.
//
// Jitter matters here because every stage runs several workers that
// tend to fail at the same instant when the server rate limits. Without
// it they would all retry in lockstep and reproduce the same burst.
func (p retryPolicy) delayFor(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}

	delay := p.BaseDelay << uint(attempt-1)

	if delay > p.MaxDelay || delay <= 0 {
		delay = p.MaxDelay
	}

	half := delay / 2

	if half <= 0 {
		return delay
	}

	return half + time.Duration(rand.Int64N(int64(half)+1))
}

// maxSessionResets bounds how many times one operation will accept a
// fresh session before giving up, so a service rejecting every session
// cannot spin here.
const maxSessionResets = 3

// sessionResetDelay is the short pause before retrying on a new session.
// Establishing that session is itself a round trip, so there is nothing
// to back off from.
const sessionResetDelay = 100 * time.Millisecond

// retryOperation runs fn until it succeeds, until the error is classified
// as permanent, until attempts run out, or until ctx is done.
//
// Attempts count tries against a usable session. A failure caused by the
// service discarding the session is retried without consuming one,
// because the operation never received a real answer.
func retryOperation(
	ctx context.Context,
	policy retryPolicy,
	name string,
	onRetry func(attempt int, delay time.Duration, err error),
	fn func(context.Context) error,
) error {

	policy = policy.normalized()

	var lastErr error

	attempt := 0
	resets := 0

	for {

		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return lastErr
			}

			return err
		}

		err := fn(ctx)

		if err == nil {
			return nil
		}

		lastErr = err

		// The session was discarded underneath this call. Getting a
		// new one is not an attempt at the service's actual answer,
		// so it is not charged as one.
		if isSessionReset(err) && resets < maxSessionResets {
			resets++

			if onRetry != nil {
				onRetry(attempt+1, sessionResetDelay, err)
			}

			timer := time.NewTimer(sessionResetDelay)

			select {

			case <-ctx.Done():
				timer.Stop()

				return lastErr

			case <-timer.C:
			}

			continue
		}

		// A permanent failure is returned immediately: no sleep, no
		// second request.
		if isPermanent(err) {
			return err
		}

		attempt++

		if attempt >= policy.MaxAttempts {
			break
		}

		delay := policy.delayFor(attempt)

		// A server that told us how long to wait wins over our own
		// backoff curve.
		if after, ok := retryAfterOf(err); ok && after > delay {
			delay = after

			if delay > policy.MaxDelay {
				delay = policy.MaxDelay
			}
		}

		if onRetry != nil {
			onRetry(attempt, delay, err)
		}

		timer := time.NewTimer(delay)

		select {

		case <-ctx.Done():
			timer.Stop()

			return lastErr

		case <-timer.C:
		}
	}

	return fmt.Errorf(
		"%s failed after %d attempts: %w",
		name,
		attempt,
		lastErr,
	)
}

// ----------------------------------------
// HTTP helpers
// ----------------------------------------

// maxDrainBytes bounds how much of an unwanted response body we are
// willing to read purely to make the connection reusable.
const maxDrainBytes = 4 << 20

// readAndDrain reads up to limit bytes, then drains whatever remains.
//
// Draining is what allows net/http to return the connection to the idle
// pool. Leaving a body partially read forces the transport to close it,
// which costs a fresh TCP and TLS handshake on the next request.
func readAndDrain(resp *http.Response, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit))

	_, _ = io.Copy(
		io.Discard,
		io.LimitReader(resp.Body, maxDrainBytes),
	)

	return body, err
}

// drainAndClose consumes and closes a response body that we do not need.
func drainAndClose(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}

	_, _ = io.Copy(
		io.Discard,
		io.LimitReader(resp.Body, maxDrainBytes),
	)

	_ = resp.Body.Close()
}

// classifyHTTPStatus turns a non-2xx response into a correctly
// classified error. 408, 429 and 5xx are worth another attempt;
// everything else is permanent.
func classifyHTTPStatus(
	resp *http.Response,
	body []byte,
	what string,
) error {

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}

	base := fmt.Errorf(
		"%s HTTP %d: %s; body=%q",
		what,
		resp.StatusCode,
		resp.Status,
		truncate(strings.TrimSpace(string(body)), 256),
	)

	switch {

	case resp.StatusCode == http.StatusTooManyRequests,
		resp.StatusCode == http.StatusRequestTimeout,
		resp.StatusCode >= 500:

		if after, ok := parseRetryAfter(
			resp.Header.Get("Retry-After"),
		); ok {
			return &retryAfterError{
				err:   base,
				after: after,
			}
		}

		return base

	default:
		return permanent(base)
	}
}

// classifyTransportError decides whether a failure from client.Do is
// worth retrying. Context cancellation is final; genuine network faults
// are not.
func classifyTransportError(err error, what string) error {
	wrapped := fmt.Errorf("%s: %w", what, err)

	if errors.Is(err, context.Canceled) {
		return permanent(wrapped)
	}

	return wrapped
}

// parseRetryAfter understands both forms allowed by RFC 9110:
// a delay in seconds, or an absolute HTTP date.
func parseRetryAfter(value string) (time.Duration, bool) {
	value = strings.TrimSpace(value)

	if value == "" {
		return 0, false
	}

	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds < 0 {
			return 0, false
		}

		return time.Duration(seconds) * time.Second, true
	}

	if when, err := http.ParseTime(value); err == nil {
		delay := time.Until(when)

		if delay > 0 {
			return delay, true
		}
	}

	return 0, false
}
