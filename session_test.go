package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeService stands in for aaplmusicdownloader.com.
type fakeService struct {
	server *httptest.Server

	mu sync.Mutex

	// cookiesSeen records, per endpoint, whether a session cookie was
	// presented on every request to it.
	cookiesSeen map[string][]string

	albumHits   atomic.Int64
	swdHits     atomic.Int64
	saveHits    atomic.Int64
	savedHits   atomic.Int64
	sourceHits  atomic.Int64
	sessionsMin atomic.Int64

	// usesPerSession counts requests carrying each session id.
	usesPerSession map[string]int

	// limit, when > 0, makes the service reject a session with 403
	// once it has been used more than limit times.
	limit int

	payload []byte
}

func newFakeService(t *testing.T, limit int) *fakeService {
	t.Helper()

	svc := &fakeService{
		cookiesSeen:    make(map[string][]string),
		usesPerSession: make(map[string]int),
		limit:          limit,
		payload:        []byte(strings.Repeat("audio-bytes!", 1024)),
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/album.php", func(w http.ResponseWriter, r *http.Request) {
		svc.albumHits.Add(1)

		id := fmt.Sprintf("sess-%d", svc.sessionsMin.Add(1))

		http.SetCookie(w, &http.Cookie{
			Name:  "PHPSESSID",
			Value: id,
			Path:  "/",
		})

		io.WriteString(w, "<html>album</html>")
	})

	// guard records the cookie for an endpoint and applies the limit.
	guard := func(name string, w http.ResponseWriter, r *http.Request) bool {
		cookie, err := r.Cookie("PHPSESSID")

		value := ""
		if err == nil {
			value = cookie.Value
		}

		svc.mu.Lock()
		svc.cookiesSeen[name] = append(svc.cookiesSeen[name], value)

		over := false
		if value != "" {
			svc.usesPerSession[value]++
			over = svc.limit > 0 && svc.usesPerSession[value] > svc.limit
		}
		svc.mu.Unlock()

		if value == "" {
			// A real PHP app would mint a fresh session here.
			svc.sessionsMin.Add(1)
		}

		if over {
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, "session limit reached")

			return false
		}

		return true
	}

	mux.HandleFunc("/swd.php", func(w http.ResponseWriter, r *http.Request) {
		svc.swdHits.Add(1)

		if !guard("swd", w, r) {
			return
		}

		_ = r.ParseForm()

		fmt.Fprintf(
			w,
			`{"dlink":"%s/source/%s.m4a","status":"ok","comments":""}`,
			svc.server.URL,
			r.Form.Get("song_name"),
		)
	})

	mux.HandleFunc("/saveid3.php", func(w http.ResponseWriter, r *http.Request) {
		svc.saveHits.Add(1)

		if !guard("saveid3", w, r) {
			return
		}

		_ = r.ParseForm()

		fmt.Fprintf(
			w,
			"%s - %s.mp3",
			r.Form.Get("name"),
			r.Form.Get("artist"),
		)
	})

	// The AAC source the resolver points at, used by transcode mode.
	mux.HandleFunc("/source/", func(w http.ResponseWriter, r *http.Request) {
		svc.sourceHits.Add(1)

		w.Header().Set("Content-Length", fmt.Sprint(len(svc.payload)))
		w.Write(svc.payload)
	})

	mux.HandleFunc("/saved/", func(w http.ResponseWriter, r *http.Request) {
		svc.savedHits.Add(1)

		if !guard("saved", w, r) {
			return
		}

		w.Header().Set("Content-Length", fmt.Sprint(len(svc.payload)))
		w.Write(svc.payload)
	})

	svc.server = httptest.NewServer(mux)
	t.Cleanup(svc.server.Close)

	return svc
}

// cookielessRequests counts requests that arrived with no session cookie.
func (s *fakeService) cookielessRequests() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make(map[string]int)

	for endpoint, values := range s.cookiesSeen {
		for _, v := range values {
			if v == "" {
				out[endpoint]++
			}
		}
	}

	return out
}

func (s *fakeService) distinctSessions() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.usesPerSession)
}

func newTestPipeline(t *testing.T, svc *fakeService, dir string) (*Processor, *SessionManager) {
	t.Helper()

	log := NewLogger(io.Discard)
	t.Cleanup(log.Close)

	session := &SessionManager{
		Transport:  http.DefaultTransport,
		SessionURL: svc.server.URL + "/album.php",
		Log:        log,
	}

	resolver := &HTTPMediaResolver{
		Endpoint: svc.server.URL + "/swd.php",
		Session:  session,
		Log:      log,
	}

	downloader := &Downloader{
		Session:      session,
		Progress:     NewProgressManager(),
		OutputDir:    dir,
		Log:          log,
		SaveID3URL:   svc.server.URL + "/saveid3.php",
		SavedBaseURL: svc.server.URL + "/saved/",
	}

	return &Processor{
		ResolveConcurrency:  3,
		SaveConcurrency:     3,
		DownloadConcurrency: 4,
		Resolver:            resolver,
		Downloader:          downloader,
		OutputDir:           dir,
		Log:                 log,
		ResolveRetry:        retryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond},
		SaveRetry:           retryPolicy{MaxAttempts: 2, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond},
		DownloadRetry:       retryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond},
	}, session
}

func makeTracks(n int) []Track {
	tracks := make([]Track, 0, n)

	for i := 0; i < n; i++ {
		tracks = append(tracks, Track{
			Index:  i,
			Name:   fmt.Sprintf("Song %d", i),
			Artist: "Artist",
			Album:  "Album",
			Link:   fmt.Sprintf("https://music.apple.com/x/%d?i=%d", i, i),
		})
	}

	return tracks
}

// TestSessionEstablishedOnce is the core of the performance fix: one
// album.php fetch for the whole run, not one per track.
func TestSessionEstablishedOnce(t *testing.T) {
	svc := newFakeService(t, 0)
	dir := t.TempDir()

	processor, _ := newTestPipeline(t, svc, dir)

	results := processor.Process(context.Background(), makeTracks(20))

	for _, r := range results {
		if r.Error != nil {
			t.Fatalf("track %d failed: %v", r.Track.Index, r.Error)
		}
	}

	if got := svc.albumHits.Load(); got != 1 {
		t.Errorf("album.php fetched %d times, want 1 for a 20 track run", got)
	}

	if got := svc.swdHits.Load(); got != 20 {
		t.Errorf("swd.php hit %d times, want 20", got)
	}
}

// TestEveryEndpointCarriesTheSession is the session-exhaustion fix:
// saveid3.php and the generated-file download previously went out with
// no cookie at all, making the service mint an orphan session each time.
func TestEveryEndpointCarriesTheSession(t *testing.T) {
	svc := newFakeService(t, 0)
	dir := t.TempDir()

	processor, _ := newTestPipeline(t, svc, dir)

	processor.Process(context.Background(), makeTracks(10))

	for endpoint, count := range svc.cookielessRequests() {
		t.Errorf("%s received %d request(s) with no session cookie", endpoint, count)
	}

	if got := svc.distinctSessions(); got != 1 {
		t.Errorf("service saw %d distinct sessions, want 1", got)
	}
}

// TestSessionRotatesOnServiceLimit covers the limitation that used to be
// cleared by hand: the service refuses a session after N uses, and the
// client must recover on its own and still finish every track.
func TestSessionRotatesOnServiceLimit(t *testing.T) {
	svc := newFakeService(t, 9)
	dir := t.TempDir()

	processor, session := newTestPipeline(t, svc, dir)

	results := processor.Process(context.Background(), makeTracks(12))

	failed := 0
	for _, r := range results {
		if r.Error != nil {
			failed++
			t.Logf("track %d: %v", r.Track.Index, r.Error)
		}
	}

	if failed != 0 {
		t.Errorf("%d tracks failed despite the limit being recoverable", failed)
	}

	rotations, observed := session.Stats()

	if rotations < 2 {
		t.Errorf("expected the session to rotate, got %d session(s)", rotations)
	}

	if observed == 0 {
		t.Error("expected the service limit to be learned, got 0")
	}

	t.Logf("sessions=%d learned limit=%d", rotations, observed)
}
