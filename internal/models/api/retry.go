package api

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Retry is the retry policy one request tries under: MaxAttempts sends in
// total (1 sends once), waiting BaseDelay before the second and doubling that
// wait up to MaxDelay. MaxRetryAfter bounds the wait a provider asks for
// through Retry-After. It is shared by every model client that sends a
// request again after a failure worth waiting out.
type Retry struct {
	MaxAttempts   int
	BaseDelay     time.Duration
	MaxDelay      time.Duration
	MaxRetryAfter time.Duration
}

// DefaultRetry is the policy the model clients ship with: a few attempts,
// backing off from half a second, never waiting longer than ten seconds
// between two, and never waiting out a Retry-After longer than a minute.
func DefaultRetry() Retry {
	return Retry{
		MaxAttempts:   4,
		BaseDelay:     500 * time.Millisecond,
		MaxDelay:      10 * time.Second,
		MaxRetryAfter: 60 * time.Second,
	}
}

// Do runs send, sending it again while it reports a failure worth waiting
// out. It returns nil once send succeeds. Otherwise the last error send gave
// goes back, except that a cancelled or expired caller gets its own error
// instead, and a Retry-After longer than MaxRetryAfter stops the retries at
// the provider's answer. A Retry built in code needs no clamping: at least
// one attempt is sent, and negative durations mean none.
func (r Retry) Do(ctx context.Context, send func(context.Context) error) error {
	_, err := r.DoWithAttempts(ctx, send)
	return err
}

// DoWithAttempts is Do, also reporting how many attempts send ran: at least
// one, and never more than MaxAttempts. One model call logs its retries from
// this count instead of inferring them.
func (r Retry) DoWithAttempts(ctx context.Context, send func(context.Context) error) (int, error) {
	r = r.clamped()
	var last error
	for attempt := 1; attempt <= r.MaxAttempts; attempt++ {
		if attempt > 1 {
			d, ok := r.retryWait(attempt-1, last)
			if !ok {
				// The provider asked for a wait this policy will not serve; its
				// own answer goes back to the caller.
				return attempt - 1, last
			}
			if err := wait(ctx, d); err != nil {
				return attempt - 1, err
			}
		}
		err := send(ctx)
		if err == nil {
			return attempt, nil
		}
		last = err
		if !retryable(err) {
			return attempt, err
		}
		// The caller went away: waiting out a backoff for a retry nobody will
		// read is worse than reporting the cancellation.
		if err := ctx.Err(); err != nil {
			return attempt, err
		}
	}
	return r.MaxAttempts, last
}

// clamped is a Retry every field of which means what this policy expects: at
// least one attempt, and durations that are never negative. Do clamps once
// rather than checking each field at every use, so a Retry built in code can
// be handed over as it is.
func (r Retry) clamped() Retry {
	return Retry{
		MaxAttempts:   max(1, r.MaxAttempts),
		BaseDelay:     max(0, r.BaseDelay),
		MaxDelay:      max(0, r.MaxDelay),
		MaxRetryAfter: max(0, r.MaxRetryAfter),
	}
}

// retryable reports whether err is worth sending again: a rate limit, a
// server-side failure, or a request that never got a whole answer. Any other
// 4xx is the request's own fault and would fail the same way again.
func retryable(err error) bool {
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode == http.StatusTooManyRequests ||
			httpErr.StatusCode >= http.StatusInternalServerError
	}
	var transportErr *TransportError
	return errors.As(err, &transportErr)
}

// retryWait is how long to wait before the next attempt, after failed attempts
// went wrong with err. It reports false when waiting is pointless: the provider
// asked for a Retry-After longer than MaxRetryAfter, and coming back earlier
// than it asked would only earn the same answer.
func (r Retry) retryWait(failed int, err error) (time.Duration, bool) {
	if after, ok := retryAfter(err); ok {
		if after > r.MaxRetryAfter {
			return 0, false
		}
		// Jitter may only lengthen a wait the vendor asked for, and never past
		// the cap on it.
		return min(after+jitter(after), r.MaxRetryAfter), true
	}
	d := r.backoff(failed)
	// Shorten rather than lengthen: backoff is already capped, so adding jitter
	// and clipping back to the cap would leave every caller waiting exactly
	// MaxDelay and coming back in lockstep.
	return d - jitter(d), true
}

// jitter is up to a quarter of d.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(d)/4 + 1))
}

// backoff is BaseDelay doubled once per failed attempt, capped at MaxDelay.
func (r Retry) backoff(failed int) time.Duration {
	d := r.BaseDelay
	for i := 1; i < failed && d < r.MaxDelay; i++ {
		d *= 2
	}
	return min(d, r.MaxDelay)
}

// retryAfter is the wait a 429 or 503 asks for, in whole seconds. A header
// naming an HTTP date instead is left alone and the backoff applies: a vendor
// that will not say how long it needs gets this policy's own schedule.
func retryAfter(err error) (time.Duration, bool) {
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		return 0, false
	}
	if httpErr.StatusCode != http.StatusTooManyRequests &&
		httpErr.StatusCode != http.StatusServiceUnavailable {
		return 0, false
	}
	seconds, convErr := strconv.Atoi(strings.TrimSpace(httpErr.Header.Get("Retry-After")))
	if convErr != nil || seconds < 0 {
		return 0, false
	}
	return time.Duration(seconds) * time.Second, true
}

// wait sleeps d, or returns ctx's error as soon as ctx is done, so a cancelled
// call is not held up by the backoff it happened to be in.
func wait(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
