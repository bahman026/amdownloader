package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func listFiles(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}

	names := make([]string, 0, len(entries))

	for _, e := range entries {
		names = append(names, e.Name())
	}

	sort.Strings(names)

	return names
}

// TestDuplicateTracksAreAllKept covers the confirmed data loss: the real
// album_details has three duplicated song names, and the old naming
// scheme turned 40 tracks into 36 files.
func TestDuplicateTracksAreAllKept(t *testing.T) {
	svc := newFakeService(t, 0)
	dir := t.TempDir()

	processor, _ := newTestPipeline(t, svc, dir)

	// Four entries, two of which are the very same song twice.
	tracks := []Track{
		{Index: 0, Name: "Masnavi", Artist: "Shajarian", Link: "https://x/1"},
		{Index: 1, Name: "Avaz Va Santoor", Artist: "Shajarian", Link: "https://x/2"},
		{Index: 2, Name: "Masnavi", Artist: "Shajarian", Link: "https://x/3"},
		{Index: 3, Name: "Avaz Va Santoor", Artist: "Shajarian", Link: "https://x/4"},
	}

	results := processor.Process(context.Background(), tracks)

	for _, r := range results {
		if r.Error != nil {
			t.Fatalf("track %d failed: %v", r.Track.Index, r.Error)
		}
	}

	files := listFiles(t, dir)

	if len(files) != 4 {
		t.Errorf("got %d files, want 4 (duplicates must not overwrite): %v", len(files), files)
	}

	for _, f := range files {
		if strings.HasSuffix(f, ".part") {
			t.Errorf("leftover partial file: %s", f)
		}
	}
}

// TestCompletedTracksAreSkippedWithoutNetwork proves a rerun costs
// nothing: not one request is made for a track already on disk.
func TestCompletedTracksAreSkippedWithoutNetwork(t *testing.T) {
	svc := newFakeService(t, 0)
	dir := t.TempDir()

	processor, _ := newTestPipeline(t, svc, dir)

	tracks := makeTracks(5)

	if results := processor.Process(context.Background(), tracks); len(results) != 5 {
		t.Fatalf("first run returned %d results", len(results))
	}

	firstSwd := svc.swdHits.Load()
	firstAlbum := svc.albumHits.Load()

	// Second run against the same directory.
	processor2, _ := newTestPipeline(t, svc, dir)
	results := processor2.Process(context.Background(), tracks)

	skipped := 0
	for _, r := range results {
		if r.Skipped {
			skipped++
		}

		if r.Error != nil {
			t.Errorf("track %d errored on rerun: %v", r.Track.Index, r.Error)
		}
	}

	if skipped != 5 {
		t.Errorf("rerun skipped %d of 5 tracks", skipped)
	}

	if got := svc.swdHits.Load(); got != firstSwd {
		t.Errorf("rerun made %d extra resolver requests, want 0", got-firstSwd)
	}

	if got := svc.albumHits.Load(); got != firstAlbum {
		t.Errorf("rerun established %d extra sessions, want 0", got-firstAlbum)
	}
}

// TestFailedDownloadLeavesNoUsableFile covers the rule that an
// incomplete transfer must never survive as a final file.
//
// The partial itself is deliberately kept so the next attempt can
// resume; it is removed once the retry budget is spent.
func TestFailedDownloadLeavesNoUsableFile(t *testing.T) {
	dir := t.TempDir()

	// Declares 10000 bytes then delivers 10 and hangs up.
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "10000")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("truncated!"))

			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}

			panic(http.ErrAbortHandler)
		}))
	defer server.Close()

	log := NewLogger(io.Discard)
	defer log.Close()

	session := &SessionManager{Transport: http.DefaultTransport, Log: log}
	session.SessionURL = server.URL

	downloader := &Downloader{
		Session:   session,
		Progress:  NewProgressManager(),
		OutputDir: dir,
		Log:       log,
	}

	track := Track{Index: 0, Name: "Broken", Artist: "Nobody"}
	plan := downloader.Plan(track)

	_, err := downloader.Download(
		context.Background(),
		track,
		server.URL+"/file.mp3",
		plan,
		".mp3",
	)

	if err == nil {
		t.Fatal("expected a truncated download to fail")
	}

	// The important guarantee: nothing usable was published.
	if _, statErr := os.Stat(plan.FinalPath(".mp3")); statErr == nil {
		t.Error("a truncated transfer was published as a final file")
	}

	// The partial is retained for the next attempt.
	if _, statErr := os.Stat(plan.PartPath(".mp3")); statErr != nil {
		t.Error("partial was discarded; a retry could not resume")
	}

	// Giving up clears it.
	downloader.DiscardPartial("x.mp3", plan)

	for _, f := range listFiles(t, dir) {
		t.Errorf("leftover after giving up: %s", f)
	}
}

// TestDownloadResumesFromPartial: a broken transfer must continue from
// where it stopped rather than refetching the whole file.
func TestDownloadResumesFromPartial(t *testing.T) {
	dir := t.TempDir()

	payload := []byte(strings.Repeat("A", 4000) + strings.Repeat("B", 6000))

	var calls atomic.Int64
	var secondRequestRange string
	var bytesSentSecond int

	mux := http.NewServeMux()
	mux.HandleFunc("/session", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	})

	mux.HandleFunc("/f.mp3", func(w http.ResponseWriter, r *http.Request) {
		{
			n := calls.Add(1)

			if n == 1 {
				// Full size promised, half delivered, then cut off.
				w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
				w.WriteHeader(http.StatusOK)
				w.Write(payload[:4000])

				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}

				panic(http.ErrAbortHandler)
			}

			secondRequestRange = r.Header.Get("Range")

			var from int
			fmt.Sscanf(secondRequestRange, "bytes=%d-", &from)

			rest := payload[from:]
			bytesSentSecond = len(rest)

			w.Header().Set("Content-Range",
				fmt.Sprintf("bytes %d-%d/%d", from, len(payload)-1, len(payload)))
			w.Header().Set("Content-Length", fmt.Sprint(len(rest)))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(rest)
		}
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	log := NewLogger(io.Discard)
	defer log.Close()

	session := &SessionManager{Transport: http.DefaultTransport, Log: log}
	session.SessionURL = server.URL + "/session"

	d := &Downloader{
		Session:   session,
		Progress:  NewProgressManager(),
		OutputDir: dir,
		Log:       log,
	}

	track := Track{Index: 0, Name: "Resumable", Artist: "Artist"}
	plan := d.Plan(track)

	// First attempt breaks.
	if _, err := d.Download(context.Background(), track,
		server.URL+"/f.mp3", plan, ".mp3"); err == nil {
		t.Fatal("expected the first attempt to fail")
	}

	// Second attempt resumes.
	total, err := d.Download(context.Background(), track,
		server.URL+"/f.mp3", plan, ".mp3")
	if err != nil {
		t.Fatalf("resume failed: %v", err)
	}

	if secondRequestRange != "bytes=4000-" {
		t.Errorf("Range header = %q, want bytes=4000-", secondRequestRange)
	}

	if bytesSentSecond != 6000 {
		t.Errorf("server resent %d bytes, want only the missing 6000", bytesSentSecond)
	}

	if total != int64(len(payload)) {
		t.Errorf("reported total = %d, want %d", total, len(payload))
	}

	got, err := os.ReadFile(plan.FinalPath(".mp3"))
	if err != nil {
		t.Fatalf("final file missing: %v", err)
	}

	if !bytes.Equal(got, payload) {
		t.Errorf("resumed file is corrupt: got %d bytes, want %d", len(got), len(payload))
	}

	if _, err := os.Stat(plan.PartPath(".mp3")); !os.IsNotExist(err) {
		t.Error(".part should be gone after a successful resume")
	}
}

// A server that ignores Range must still produce a correct file.
func TestDownloadHandlesServerIgnoringRange(t *testing.T) {
	dir := t.TempDir()

	payload := []byte(strings.Repeat("Z", 8000))

	var calls atomic.Int64

	mux2 := http.NewServeMux()
	mux2.HandleFunc("/session", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	})

	mux2.HandleFunc("/f.mp3", func(w http.ResponseWriter, r *http.Request) {
		{
			if calls.Add(1) == 1 {
				w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
				w.WriteHeader(http.StatusOK)
				w.Write(payload[:3000])

				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}

				panic(http.ErrAbortHandler)
			}

			// Range ignored: whole file, plain 200.
			w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
			w.WriteHeader(http.StatusOK)
			w.Write(payload)
		}
	})

	server := httptest.NewServer(mux2)
	defer server.Close()

	log := NewLogger(io.Discard)
	defer log.Close()

	session := &SessionManager{Transport: http.DefaultTransport, Log: log}
	session.SessionURL = server.URL + "/session"

	d := &Downloader{Session: session, Progress: NewProgressManager(),
		OutputDir: dir, Log: log}

	track := Track{Index: 0, Name: "Ignored", Artist: "Artist"}
	plan := d.Plan(track)

	d.Download(context.Background(), track, server.URL+"/f.mp3", plan, ".mp3")

	if _, err := d.Download(context.Background(), track,
		server.URL+"/f.mp3", plan, ".mp3"); err != nil {
		t.Fatalf("second attempt failed: %v", err)
	}

	got, err := os.ReadFile(plan.FinalPath(".mp3"))
	if err != nil {
		t.Fatal(err)
	}

	// Must be the file, not the partial with the file appended to it.
	if !bytes.Equal(got, payload) {
		t.Errorf("file is corrupt: %d bytes, want %d", len(got), len(payload))
	}
}

// TestPartFilesAreSweptOnStartup makes sure a stale partial from a
// killed run is neither trusted nor left behind.
func TestPartFilesAreSweptOnStartup(t *testing.T) {
	dir := t.TempDir()

	stale := filepath.Join(dir, "01 - Song 0 - Artist.mp3.part")

	if err := os.WriteFile(stale, []byte("half"), 0o644); err != nil {
		t.Fatal(err)
	}

	downloader := &Downloader{OutputDir: dir, Log: NewLogger(io.Discard)}

	index := downloader.NewCompletedIndex()

	done, _ := index.Claim(
		Track{Index: 0, Name: "Song 0", Artist: "Artist"},
		downloader.Plan(Track{Index: 0, Name: "Song 0", Artist: "Artist"}),
	)

	if done {
		t.Error("a .part file was counted as a completed download")
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("stale .part file was not swept")
	}
}

// TestPermanentResolverErrorIsNotRetried is the load rule: an in-band
// error is the service's real answer and must cost exactly one request.
func TestPermanentResolverErrorIsNotRetried(t *testing.T) {
	var swdCalls atomic.Int64

	mux := http.NewServeMux()

	mux.HandleFunc("/album.php", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "PHPSESSID", Value: "s1", Path: "/"})
		io.WriteString(w, "ok")
	})

	mux.HandleFunc("/swd.php", func(w http.ResponseWriter, r *http.Request) {
		swdCalls.Add(1)
		io.WriteString(w, `{"error":"403 Forbidden"}`)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	log := NewLogger(io.Discard)
	defer log.Close()

	session := &SessionManager{
		Transport:  http.DefaultTransport,
		SessionURL: server.URL + "/album.php",
		Log:        log,
	}

	resolver := &HTTPMediaResolver{
		Endpoint: server.URL + "/swd.php",
		Session:  session,
		Log:      log,
	}

	processor := &Processor{
		ResolveConcurrency:  1,
		SaveConcurrency:     1,
		DownloadConcurrency: 1,
		Resolver:            resolver,
		Downloader:          &Downloader{Session: session, OutputDir: t.TempDir(), Log: log},
		Log:                 log,
		ResolveRetry:        retryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond},
	}

	results := processor.Process(context.Background(), makeTracks(1))

	if len(results) != 1 || results[0].Error == nil {
		t.Fatal("expected the track to fail")
	}

	if got := swdCalls.Load(); got != 1 {
		t.Errorf("in-band error caused %d resolver requests, want exactly 1", got)
	}

	if !strings.Contains(results[0].Error.Error(), "403 Forbidden") {
		t.Errorf("error lost its cause: %v", results[0].Error)
	}
}

// TestTransientErrorIsRetried is the other half: a 503 must be retried.
func TestTransientErrorIsRetried(t *testing.T) {
	var swdCalls atomic.Int64

	mux := http.NewServeMux()

	mux.HandleFunc("/album.php", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "PHPSESSID", Value: "s1", Path: "/"})
		io.WriteString(w, "ok")
	})

	mux.HandleFunc("/swd.php", func(w http.ResponseWriter, r *http.Request) {
		if swdCalls.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, "busy")

			return
		}

		fmt.Fprint(w, `{"dlink":"https://cdn/x.m4a","status":"ok"}`)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	log := NewLogger(io.Discard)
	defer log.Close()

	session := &SessionManager{
		Transport:  http.DefaultTransport,
		SessionURL: server.URL + "/album.php",
		Log:        log,
	}

	resolver := &HTTPMediaResolver{
		Endpoint: server.URL + "/swd.php",
		Session:  session,
		Log:      log,
	}

	response, err := retryResolveForTest(resolver)

	if err != nil {
		t.Fatalf("transient failure was not absorbed: %v", err)
	}

	if response == "" {
		t.Fatal("expected a dlink")
	}

	if got := swdCalls.Load(); got != 3 {
		t.Errorf("made %d attempts, want 3", got)
	}
}

func retryResolveForTest(resolver *HTTPMediaResolver) (string, error) {
	processor := &Processor{
		Resolver:     resolver,
		Log:          resolver.Log,
		ResolveRetry: retryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond},
	}

	return processor.resolve(
		context.Background(),
		Track{Index: 0, Name: "N", Artist: "A", Link: "https://x/1"},
	)
}

// --- legacy filename detection -------------------------------------
//
// Files downloaded before the naming change are named after whatever
// the remote service returned, which strips punctuation. They must
// still be recognised so a rerun does not fetch them all again.

func touch(t *testing.T, dir, name string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyFilenamesAreRecognised(t *testing.T) {
	dir := t.TempDir()

	// Exactly how the old scheme wrote them: no index, punctuation
	// stripped by the service.
	touch(t, dir, "Rain Lyrics By Ali Moallem - Kayhan Kalhor Mohammad-Reza Shajarian.mp3")
	touch(t, dir, "Thinking Out Loud - Ed Sheeran.mp3")

	d := &Downloader{OutputDir: dir}
	index := d.NewCompletedIndex()

	cases := []struct {
		track Track
		want  bool
	}{
		// Parentheses, apostrophe and ampersand all differ from disk.
		{Track{Index: 2, Name: "Rain (Lyrics By Ali Mo'allem)",
			Artist: "Kayhan Kalhor & Mohammad-Reza Shajarian"}, true},
		{Track{Index: 0, Name: "Thinking Out Loud", Artist: "Ed Sheeran"}, true},
		{Track{Index: 1, Name: "Some Other Song", Artist: "Nobody"}, false},
	}

	for _, c := range cases {
		got, how := index.Claim(c.track, d.Plan(c.track))

		if got != c.want {
			t.Errorf("%q: claimed=%v want=%v", c.track.Name, got, c.want)
		}

		if got && how != "legacy naming" {
			t.Errorf("%q: matched via %q, want legacy naming", c.track.Name, how)
		}
	}
}

// The old scheme collapsed duplicates onto one file. One file on disk
// must satisfy exactly one of the duplicate tracks; the rest still
// download, under their own distinct names.
func TestLegacyDuplicatesClaimOnlyOnce(t *testing.T) {
	dir := t.TempDir()

	touch(t, dir, "Masnavi - Shajarian.mp3")

	d := &Downloader{OutputDir: dir}
	index := d.NewCompletedIndex()

	tracks := []Track{
		{Index: 0, Name: "Masnavi", Artist: "Shajarian"},
		{Index: 13, Name: "Masnavi", Artist: "Shajarian"},
		{Index: 25, Name: "Masnavi", Artist: "Shajarian"},
	}

	claimed := 0

	for _, tr := range tracks {
		if done, _ := index.Claim(tr, d.Plan(tr)); done {
			claimed++
		}
	}

	if claimed != 1 {
		t.Errorf("%d tracks claimed the single legacy file, want 1", claimed)
	}
}

// Current-scheme files match exactly, per index, so duplicates are
// each recognised independently.
func TestCurrentFilenamesMatchPerIndex(t *testing.T) {
	dir := t.TempDir()

	touch(t, dir, "01 - Masnavi - Shajarian.mp3")
	touch(t, dir, "14 - Masnavi - Shajarian.mp3")

	d := &Downloader{OutputDir: dir}
	index := d.NewCompletedIndex()

	first := Track{Index: 0, Name: "Masnavi", Artist: "Shajarian"}
	second := Track{Index: 13, Name: "Masnavi", Artist: "Shajarian"}
	third := Track{Index: 25, Name: "Masnavi", Artist: "Shajarian"}

	if done, how := index.Claim(first, d.Plan(first)); !done || how != "current naming" {
		t.Errorf("track 1: done=%v how=%q", done, how)
	}

	if done, how := index.Claim(second, d.Plan(second)); !done || how != "current naming" {
		t.Errorf("track 14: done=%v how=%q", done, how)
	}

	if done, _ := index.Claim(third, d.Plan(third)); done {
		t.Error("track 26 has no file but was claimed")
	}
}

func TestCanonicalKey(t *testing.T) {
	same := [][2]string{
		{"Rain (Lyrics By Ali Mo'allem)", "Rain Lyrics By Ali Moallem"},
		{"Kayhan Kalhor & Mohammad-Reza Shajarian", "Kayhan Kalhor Mohammad-Reza Shajarian"},
		{"Mohammad-Reza Shajarian", "Mohammadreza Shajarian"},
		{"Tasnife Saghi (feat. X)", "Tasnife Saghi feat. X"},
	}

	for _, pair := range same {
		if canonicalKey(pair[0]) != canonicalKey(pair[1]) {
			t.Errorf("%q and %q should share a key", pair[0], pair[1])
		}
	}

	// Accented letters are meaningful and must not be folded away.
	if canonicalKey("Âmadan") == canonicalKey("Bmadan") {
		t.Error("distinct names collided")
	}

	if canonicalKey("Song A") == canonicalKey("Song B") {
		t.Error("distinct names collided")
	}
}
