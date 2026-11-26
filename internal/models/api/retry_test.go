package api

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

// Do sends once more after a failure worth waiting out, and returns as soon
// as one send succeeds.
func TestRetryDoRetriesWorthwhileFailures(t *testing.T) {
	fail := &HTTPError{StatusCode: http.StatusInternalServerError}
	r := Retry{MaxAttempts: 4, BaseDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond, MaxRetryAfter: time.Second}
	sends := 0
	err := r.Do(context.Background(), func(context.Context) error {
		if sends++; sends < 3 {
			return fail
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if sends != 3 {
		t.Errorf("send ran %d times, want 3", sends)
	}
}

// A 4xx other than a rate limit is the request's own fault: sending it again
// would fail the same way.
func TestRetryDoSendsANonRetryableFailureBackAtOnce(t *testing.T) {
	bad := &HTTPError{StatusCode: http.StatusBadRequest}
	r := Retry{MaxAttempts: 4, BaseDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond}
	sends := 0
	err := r.Do(context.Background(), func(context.Context) error {
		sends++
		return bad
	})
	if !errors.Is(err, bad) {
		t.Errorf("Do err = %v, want the request's own error", err)
	}
	if sends != 1 {
		t.Errorf("send ran %d times, want 1", sends)
	}
}

func TestRetryDoGivesUpAfterMaxAttempts(t *testing.T) {
	fail := &HTTPError{StatusCode: http.StatusBadGateway}
	r := Retry{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond}
	sends := 0
	err := r.Do(context.Background(), func(context.Context) error {
		sends++
		return fail
	})
	if !errors.Is(err, fail) {
		t.Errorf("Do err = %v, want the last failure", err)
	}
	if sends != 3 {
		t.Errorf("send ran %d times, want %d", sends, 3)
	}
}

// A caller that went away is not waited out: the backoff stops and the
// cancellation goes back instead of the provider's error.
func TestRetryDoStopsOnceTheContextIsDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	transient := &HTTPError{StatusCode: http.StatusServiceUnavailable}
	r := Retry{MaxAttempts: 4, BaseDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond}
	sends := 0
	err := r.Do(ctx, func(context.Context) error {
		sends++
		cancel()
		return transient
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Do err = %v, want context.Canceled", err)
	}
	if sends != 1 {
		t.Errorf("send ran %d times, want 1", sends)
	}
}

// A Retry built in code with no attempts still sends once rather than never
// asking the provider at all.
func TestRetryDoSendsAtLeastOnce(t *testing.T) {
	sends := 0
	r := Retry{}
	if err := r.Do(context.Background(), func(context.Context) error {
		sends++
		return nil
	}); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if sends != 1 {
		t.Errorf("send ran %d times, want 1", sends)
	}
}

// A Retry built in code needs no clamping of its own: negative durations mean
// none, so a wait is never negative and a cap below zero means a zero cap.
func TestRetryDoTreatsNegativeDurationsAsZero(t *testing.T) {
	r := Retry{MaxAttempts: 2, BaseDelay: -time.Second, MaxDelay: -time.Second, MaxRetryAfter: -time.Second}
	if got := r.clamped(); got.BaseDelay != 0 || got.MaxDelay != 0 || got.MaxRetryAfter != 0 {
		t.Errorf("clamped Retry = %+v, want non-negative durations", got)
	}
	// Retry-After 0 asks for no wait, and a zero cap still serves it, so the
	// second attempt runs.
	sends := 0
	soon := &HTTPError{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": {"0"}}}
	if err := r.Do(context.Background(), func(context.Context) error {
		if sends++; sends < 2 {
			return soon
		}
		return nil
	}); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if sends != 2 {
		t.Errorf("send ran %d times, want 2: Retry-After 0 is honoured", sends)
	}
	// A positive Retry-After is longer than the zero cap, so the provider's
	// own error goes back at once.
	sends = 0
	later := &HTTPError{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": {"7"}}}
	err := r.Do(context.Background(), func(context.Context) error {
		sends++
		return later
	})
	if !errors.Is(err, later) {
		t.Errorf("Do err = %v, want the provider's own error", err)
	}
	if sends != 1 {
		t.Errorf("send ran %d times, want 1", sends)
	}
	// Without a Retry-After the zero backoff applies instead of a negative
	// one.
	if got, ok := r.clamped().retryWait(5, &HTTPError{StatusCode: http.StatusInternalServerError}); !ok || got < 0 {
		t.Errorf("retryWait = %v, %v, want a wait of at least zero", got, ok)
	}
}

func TestRetryWaitHonoursRetryAfterAndBacksOff(t *testing.T) {
	r := Retry{BaseDelay: time.Second, MaxDelay: 30 * time.Second, MaxRetryAfter: 2 * time.Minute}
	header := func(v string) http.Header { return http.Header{"Retry-After": {v}} }
	tooMany := &HTTPError{StatusCode: http.StatusTooManyRequests, Header: header("7")}

	// The provider says how long to wait, and jitter may only add to it.
	if got, ok := r.retryWait(1, tooMany); !ok || got < 7*time.Second || got > 7*time.Second*5/4 {
		t.Errorf("retryWait after a 429 with Retry-After 7 = %v, %v, want 7s plus at most a quarter", got, ok)
	}
	unavailable := &HTTPError{StatusCode: http.StatusServiceUnavailable, Header: header("120")}
	if got, ok := r.retryWait(1, unavailable); !ok || got < 120*time.Second || got > 150*time.Second {
		t.Errorf("retryWait after Retry-After 120 = %v, %v, want 120s plus at most a quarter", got, ok)
	}
	// A wait longer than this client serves is not waited out at all: coming
	// back earlier than the provider asked only earns the same answer.
	impatient := Retry{BaseDelay: time.Second, MaxDelay: 30 * time.Second, MaxRetryAfter: 30 * time.Second}
	if got, ok := impatient.retryWait(1, unavailable); ok {
		t.Errorf("retryWait after Retry-After 120 with a 30s cap = %v, true, want false", got)
	}
	// Any other status backs off instead, whatever headers come with it.
	badGateway := &HTTPError{StatusCode: http.StatusBadGateway, Header: header("60")}
	if got, ok := r.retryWait(1, badGateway); !ok || got < 750*time.Millisecond || got > time.Second {
		t.Errorf("retryWait after a 502 = %v, %v, want the 1s backoff, not Retry-After", got, ok)
	}
	// Without a Retry-After the wait doubles per attempt, up to the cap, and
	// jitter shortens it so callers at the cap do not come back together.
	overloaded := &HTTPError{StatusCode: http.StatusInternalServerError}
	if got, ok := r.retryWait(3, overloaded); !ok || got < 3*time.Second || got > 4*time.Second {
		t.Errorf("retryWait after 3 attempts = %v, %v, want the 4s backoff jittered down", got, ok)
	}
	capped := Retry{BaseDelay: time.Second, MaxDelay: 2 * time.Second}
	draws := make(map[time.Duration]bool)
	for range 20 {
		got, ok := capped.retryWait(9, overloaded)
		if !ok {
			t.Fatalf("retryWait after 9 attempts = %v, false, want a wait", got)
		}
		if got > 2*time.Second {
			t.Fatalf("retryWait after 9 attempts = %v, want at most the 2s cap", got)
		}
		draws[got] = true
	}
	// Once the backoff reaches the cap, jitter has to shorten the wait rather
	// than be clipped away by it.
	if len(draws) < 2 {
		t.Errorf("20 draws at the cap were all the same wait, want them to vary")
	}
	transport := &TransportError{Op: "send request", Err: errors.New("connection reset")}
	if got, ok := r.retryWait(1, transport); !ok || got < 750*time.Millisecond || got > time.Second {
		t.Errorf("retryWait after a transport failure = %v, %v, want the 1s backoff jittered down", got, ok)
	}
}
