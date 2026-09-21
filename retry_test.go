package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestParseRetryAfter(t *testing.T) {
	if d, ok := parseRetryAfter("12"); !ok || d != 12*time.Second {
		t.Errorf("seconds form: got %v %v", d, ok)
	}

	future := time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)

	if d, ok := parseRetryAfter(future); !ok || d <= 0 {
		t.Errorf("date form: got %v %v", d, ok)
	}

	past := time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)

	if _, ok := parseRetryAfter(past); ok {
		t.Error("a past date should not produce a delay")
	}

	for _, bad := range []string{"", "   ", "nonsense", "-5"} {
		if _, ok := parseRetryAfter(bad); ok {
			t.Errorf("%q should not parse", bad)
		}
	}
}

func TestPermanentIsNotRetried(t *testing.T) {
	calls := 0

	err := retryOperation(
		context.Background(),
		retryPolicy{MaxAttempts: 5, BaseDelay: time.Millisecond},
		"op",
		nil,
		func(context.Context) error {
			calls++

			return permanent(errors.New("bad input"))
		},
	)

	if err == nil {
		t.Fatal("expected failure")
	}

	if calls != 1 {
		t.Errorf("permanent error attempted %d times, want 1", calls)
	}
}

func TestRetryableIsRetriedThenSucceeds(t *testing.T) {
	calls := 0

	err := retryOperation(
		context.Background(),
		retryPolicy{MaxAttempts: 4, BaseDelay: time.Millisecond},
		"op",
		nil,
		func(context.Context) error {
			calls++

			if calls < 3 {
				return errors.New("flaky")
			}

			return nil
		},
	)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if calls != 3 {
		t.Errorf("made %d calls, want 3", calls)
	}
}

func TestSessionResetDoesNotConsumeAttempts(t *testing.T) {
	calls := 0

	// Two session resets followed by two real failures must still get
	// the full attempt budget for the real failures.
	err := retryOperation(
		context.Background(),
		retryPolicy{MaxAttempts: 2, BaseDelay: time.Millisecond},
		"op",
		nil,
		func(context.Context) error {
			calls++

			if calls <= 2 {
				return sessionReset(errors.New("session gone"))
			}

			return errors.New("real failure")
		},
	)

	if err == nil {
		t.Fatal("expected failure")
	}

	if calls != 4 {
		t.Errorf("made %d calls, want 4 (2 resets + 2 attempts)", calls)
	}
}

func TestSessionResetsAreBounded(t *testing.T) {
	calls := 0

	err := retryOperation(
		context.Background(),
		retryPolicy{MaxAttempts: 2, BaseDelay: time.Millisecond},
		"op",
		nil,
		func(context.Context) error {
			calls++

			return sessionReset(errors.New("always gone"))
		},
	)

	if err == nil {
		t.Fatal("expected failure")
	}

	if calls > maxSessionResets+2 {
		t.Errorf("session resets were not bounded: %d calls", calls)
	}
}

func TestCancelledContextStopsRetrying(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	calls := 0

	err := retryOperation(
		ctx,
		retryPolicy{MaxAttempts: 5, BaseDelay: 50 * time.Millisecond},
		"op",
		nil,
		func(context.Context) error {
			calls++
			cancel()

			return errors.New("fail")
		},
	)

	if err == nil {
		t.Fatal("expected failure")
	}

	if calls != 1 {
		t.Errorf("kept going after cancellation: %d calls", calls)
	}
}

func TestClassifyHTTPStatus(t *testing.T) {
	cases := []struct {
		code      int
		permanent bool
	}{
		{http.StatusOK, false},
		{http.StatusNotFound, true},
		{http.StatusForbidden, true},
		{http.StatusBadRequest, true},
		{http.StatusRequestTimeout, false},
		{http.StatusTooManyRequests, false},
		{http.StatusInternalServerError, false},
		{http.StatusBadGateway, false},
	}

	for _, c := range cases {
		resp := &http.Response{
			StatusCode: c.code,
			Status:     fmt.Sprint(c.code),
			Header:     http.Header{},
		}

		err := classifyHTTPStatus(resp, nil, "test")

		if c.code == http.StatusOK {
			if err != nil {
				t.Errorf("200 produced an error: %v", err)
			}

			continue
		}

		if err == nil {
			t.Errorf("%d produced no error", c.code)

			continue
		}

		if got := isPermanent(err); got != c.permanent {
			t.Errorf("%d: permanent=%v, want %v", c.code, got, c.permanent)
		}
	}
}

func TestRetryAfterHeaderIsHonoured(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Status:     "429",
		Header:     http.Header{"Retry-After": []string{"7"}},
	}

	err := classifyHTTPStatus(resp, nil, "test")

	delay, ok := retryAfterOf(err)

	if !ok || delay != 7*time.Second {
		t.Errorf("Retry-After not honoured: %v %v", delay, ok)
	}
}

func TestBackoffGrowsAndIsJittered(t *testing.T) {
	policy := retryPolicy{
		MaxAttempts: 5,
		BaseDelay:   100 * time.Millisecond,
		MaxDelay:    10 * time.Second,
	}.normalized()

	seen := make(map[time.Duration]bool)

	for i := 0; i < 50; i++ {
		seen[policy.delayFor(3)] = true
	}

	if len(seen) < 5 {
		t.Errorf("delays are not jittered: %d distinct values", len(seen))
	}

	for _, d := range []time.Duration{
		policy.delayFor(1),
		policy.delayFor(2),
		policy.delayFor(3),
	} {
		if d <= 0 || d > policy.MaxDelay {
			t.Errorf("delay out of range: %v", d)
		}
	}

	// Never exceeds the ceiling, however many attempts.
	if d := policy.delayFor(30); d > policy.MaxDelay {
		t.Errorf("delay %v exceeded max %v", d, policy.MaxDelay)
	}
}

func TestPlanNamesAreUniquePerTrackIndex(t *testing.T) {
	d := &Downloader{OutputDir: "/tmp/x"}

	a := d.Plan(Track{Index: 0, Name: "Masnavi", Artist: "Shajarian"})
	b := d.Plan(Track{Index: 9, Name: "Masnavi", Artist: "Shajarian"})
	c := d.Plan(Track{Index: 99, Name: "Masnavi", Artist: "Shajarian"})

	if a.Stem == b.Stem || b.Stem == c.Stem {
		t.Errorf("duplicate stems: %q %q %q", a.Stem, b.Stem, c.Stem)
	}

	// A zero padded index must not be a prefix of a longer one, or the
	// skip check would match the wrong track.
	if len(b.Stem) < len(c.Stem) && c.Stem[:len(b.Stem)] == b.Stem {
		t.Errorf("stem %q is a prefix of %q", b.Stem, c.Stem)
	}
}

func TestSafeFilenameStripsPathSeparators(t *testing.T) {
	got := safeFilename("../../etc/passwd")

	for _, bad := range []string{"/", "\\", ".."} {
		if bad == ".." {
			continue
		}

		if contains(got, bad) {
			t.Errorf("%q still contains %q", got, bad)
		}
	}

	d := &Downloader{OutputDir: "/tmp/out"}
	plan := d.Plan(Track{Index: 0, Name: "../../etc", Artist: "passwd"})

	if contains(plan.Stem, "/") {
		t.Errorf("plan stem escapes the directory: %q", plan.Stem)
	}
}

func contains(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) &&
		(func() bool {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}

			return false
		})()
}
