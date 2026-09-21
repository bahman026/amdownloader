package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"sync"
	"time"
)

// sessionTTL is a safety net, not the primary rotation trigger.
//
// PHP's default session.gc_maxlifetime is 24 minutes, so a session older
// than this is likely to have been collected server side already.
const sessionTTL = 10 * time.Minute

// consecutiveFailureThreshold is how many resolves must fail in a row on
// one session before the session itself is suspected.
//
// A single track failing is a property of that track. Several in a row
// on the same session is a property of the session, and that difference
// is what distinguishes the two without having to guess at the
// service's error strings.
const consecutiveFailureThreshold = 3

const defaultSessionURL = "https://aaplmusicdownloader.com/album.php"

// sessionResetError marks a failure caused by the service refusing the
// session rather than by anything about the request.
//
// It is distinct from an ordinary retryable error because the operation
// never really ran: it was answered by a session that had already been
// discarded. Retrying on a fresh session is therefore not a second
// attempt at the same question, and the retry layer does not charge it
// as one.
type sessionResetError struct {
	err error
}

func (e *sessionResetError) Error() string {
	return e.err.Error()
}

func (e *sessionResetError) Unwrap() error {
	return e.err
}

func sessionReset(err error) error {
	if err == nil {
		return nil
	}

	return &sessionResetError{err: err}
}

func isSessionReset(err error) bool {
	var target *sessionResetError

	return errors.As(err, &target)
}

// sessionHandle identifies the session a request was made on, so a
// caller reporting a failure cannot invalidate a session that another
// goroutine has already replaced.
type sessionHandle struct {
	client     *http.Client
	generation int
}

func (h sessionHandle) Client() *http.Client {
	return h.client
}

// SessionManager owns the single upstream session shared by every stage.
//
// Previously the cookie jar lived on one client used only by the
// resolver, while saveid3.php and the generated-file download went out
// through a client with no jar at all. Those two endpoints therefore
// presented no session cookie while still sending Origin and Referer
// headers claiming to come from the album page. Any server-side
// session_start() on that path mints a brand new session per request,
// so a 40 track run left the service holding dozens of orphaned
// sessions on top of the one per track the resolver was already
// abandoning. Routing every request through one jar makes the whole
// flow a single coherent session, the way a browser would.
//
// Rotation is driven by observed failure rather than by a fixed
// schedule. When a session is rejected the number of uses it reached is
// remembered, and later sessions rotate just before that point, so the
// service's actual limit is discovered instead of assumed.
type SessionManager struct {
	Transport  http.RoundTripper
	SessionURL string
	Log        *Logger

	mu         sync.Mutex
	client     *http.Client
	generation int
	createdAt  time.Time
	uses       int
	successes  int

	// observedLimit is the number of uses the last rejected session
	// reached. Zero means the limit has not been observed yet.
	observedLimit int

	consecutive int
	rotations   int
}

func (m *SessionManager) sessionURL() string {
	if strings.TrimSpace(m.SessionURL) != "" {
		return m.SessionURL
	}

	return defaultSessionURL
}

// budget returns the number of uses a session may serve before being
// rotated proactively, or zero while the limit is still unknown.
//
// It is derived from requests the service actually accepted, not from
// requests started, so concurrent work in flight when a limit is hit
// cannot inflate the figure past the real one.
func (m *SessionManager) budget() int {
	if m.observedLimit <= 1 {
		return 0
	}

	return m.observedLimit - 1
}

// Acquire returns the live session, establishing or rotating it first if
// needed.
func (m *SessionManager) Acquire(ctx context.Context) (sessionHandle, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	reason := ""

	switch {

	case m.client == nil:
		reason = "no active session"

	case time.Since(m.createdAt) >= sessionTTL:
		reason = "session past its time to live"

	case m.budget() > 0 && m.uses >= m.budget():
		reason = fmt.Sprintf(
			"reached observed service limit of %d uses",
			m.observedLimit,
		)
	}

	if reason != "" {
		if err := m.rotateLocked(ctx, reason); err != nil {
			return sessionHandle{}, err
		}
	}

	m.uses++

	return sessionHandle{
		client:     m.client,
		generation: m.generation,
	}, nil
}

// rotateLocked builds a fresh session. The caller must hold m.mu, which
// also means only one rotation can be in flight at a time: concurrent
// callers wait and then share the result rather than each making their
// own.
func (m *SessionManager) rotateLocked(ctx context.Context, reason string) error {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return fmt.Errorf("create cookie jar: %w", err)
	}

	transport := m.Transport

	if transport == nil {
		transport = http.DefaultTransport
	}

	// The transport is shared with the rest of the program so that
	// connections stay pooled across a rotation. Only the jar is new.
	client := &http.Client{
		Transport: transport,
		Jar:       jar,
	}

	m.Log.Printf(
		"[SESSION] Establishing session (%s)\n",
		reason,
	)

	if err := establishSession(
		ctx,
		client,
		m.sessionURL(),
		m.Log,
	); err != nil {
		return err
	}

	m.client = client
	m.generation++
	m.createdAt = time.Now()
	m.uses = 0
	m.successes = 0
	m.consecutive = 0
	m.rotations++

	return nil
}

// Succeeded reports that a request on this session worked.
func (m *SessionManager) Succeeded(handle sessionHandle) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if handle.generation != m.generation {
		return
	}

	m.consecutive = 0
	m.successes++
}

// Rejected reports that the service refused this session outright, for
// example with a transport level 401 or 403.
//
// The use count it reached is recorded so later sessions can rotate
// before reaching the same point.
func (m *SessionManager) Rejected(handle sessionHandle, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if handle.generation != m.generation || m.client == nil {
		return
	}

	m.noteLimitLocked()

	m.Log.Printf(
		"[SESSION] Discarding session after %d accepted requests: %s\n",
		m.successes,
		reason,
	)

	m.client = nil
}

// Failed reports a request failure that is not obviously session
// related. Only a run of them on one session is treated as evidence that
// the session, rather than the individual track, is the problem.
//
// It reports whether the caller should treat the failure as retryable on
// a fresh session.
func (m *SessionManager) Failed(handle sessionHandle) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	if handle.generation != m.generation || m.client == nil {
		return false
	}

	m.consecutive++

	if m.consecutive < consecutiveFailureThreshold {
		return false
	}

	m.noteLimitLocked()

	m.Log.Printf(
		"[SESSION] %d consecutive failures after %d uses; rotating session\n",
		m.consecutive,
		m.uses,
	)

	m.client = nil

	return true
}

// noteLimitLocked remembers how far the current session got before it
// stopped working, keeping the smallest figure seen.
func (m *SessionManager) noteLimitLocked() {
	if m.successes <= 1 {
		return
	}

	if m.observedLimit == 0 || m.successes < m.observedLimit {
		m.observedLimit = m.successes

		m.Log.Printf(
			"[SESSION] Learned service session limit: ~%d uses\n",
			m.observedLimit,
		)
	}
}

// Stats reports what the manager learned, for the end of run summary.
func (m *SessionManager) Stats() (rotations, observedLimit int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.rotations, m.observedLimit
}

// CheckResponse discards the session when the service signals that it no
// longer accepts it. It reports whether the session was discarded.
func (m *SessionManager) CheckResponse(
	handle sessionHandle,
	resp *http.Response,
	what string,
) bool {

	if resp == nil {
		return false
	}

	if resp.StatusCode != http.StatusUnauthorized &&
		resp.StatusCode != http.StatusForbidden {

		return false
	}

	m.Rejected(handle, fmt.Sprintf(
		"%s returned HTTP %d",
		what,
		resp.StatusCode,
	))

	return true
}

// establishSession performs the page fetch that makes the service issue
// its cookies.
func establishSession(
	ctx context.Context,
	client *http.Client,
	sessionURL string,
	log *Logger,
) error {

	ctx, cancel := context.WithTimeout(ctx, sessionTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		sessionURL,
		nil,
	)
	if err != nil {
		return permanent(fmt.Errorf(
			"create session request: %w",
			err,
		))
	}

	req.Header.Set(
		"Accept",
		"text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
	)

	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	req.Header.Set("User-Agent", browserUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return classifyTransportError(err, "session request failed")
	}

	// Drained in full so the connection returns to the idle pool.
	defer drainAndClose(resp)

	log.Printf(
		"[SESSION] Session HTTP status: %s\n",
		resp.Status,
	)

	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return classifyHTTPStatus(resp, nil, "session")
	}

	return nil
}
