package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

// All lyric text in these tests is synthetic placeholder text.

func TestParseLRC(t *testing.T) {
	input := "" +
		"[ar:Some Artist]\n" +
		"[ti:Some Title]\n" +
		"[length:03:21]\n" +
		"[00:01.00]line one\n" +
		"[00:12.34]line two\n" +
		"[01:00.5]line three\n" +
		"[02:03.456]line four\n" +
		"\n" +
		"not a timed line\n"

	got := ParseLRC(input)

	want := []LyricLine{
		{At: 1000, Text: "line one"},
		{At: 12340, Text: "line two"},
		{At: 60500, Text: "line three"},
		{At: 123456, Text: "line four"},
	}

	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d: %+v", len(got), len(want), got)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d: got %+v want %+v", i, got[i], want[i])
		}
	}
}

// One line can carry several timestamps, meaning it repeats.
func TestParseLRCRepeatedTimestamps(t *testing.T) {
	got := ParseLRC("[00:10.00][01:10.00][02:10.00]repeating line\n")

	if len(got) != 3 {
		t.Fatalf("got %d lines, want 3", len(got))
	}

	for i, at := range []int64{10000, 70000, 130000} {
		if got[i].At != at {
			t.Errorf("line %d at %d, want %d", i, got[i].At, at)
		}

		if got[i].Text != "repeating line" {
			t.Errorf("line %d text = %q", i, got[i].Text)
		}
	}
}

func TestParseLRCOffsetAndOrdering(t *testing.T) {
	got := ParseLRC("[offset:-500]\n[00:05.00]b\n[00:01.00]a\n")

	if len(got) != 2 {
		t.Fatalf("got %d lines", len(got))
	}

	// Sorted by time, with the offset applied.
	if got[0].Text != "a" || got[0].At != 500 {
		t.Errorf("first = %+v, want {500 a}", got[0])
	}

	if got[1].Text != "b" || got[1].At != 4500 {
		t.Errorf("second = %+v, want {4500 b}", got[1])
	}
}

func TestParseLRCEmptyAndGarbage(t *testing.T) {
	for _, input := range []string{"", "   ", "no timestamps here", "[ar:x]\n[ti:y]"} {
		if got := ParseLRC(input); len(got) != 0 {
			t.Errorf("%q produced %d lines", input, len(got))
		}
	}
}

func TestParseDurationSeconds(t *testing.T) {
	cases := map[string]float64{
		"4:24":    264,
		"0:30":    30,
		"1:02:03": 3723,
		"":        0,
		"abc":     0,
	}

	for input, want := range cases {
		if got := parseDurationSeconds(input); got != want {
			t.Errorf("%q = %v, want %v", input, got, want)
		}
	}
}

// fakeLRCLIB serves the two endpoints the provider uses.
func fakeLRCLIB(t *testing.T, synced map[string]string, durations map[string]float64) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()

	mux.HandleFunc("/api/get", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("track_name")

		lyrics, ok := synced[name]

		if !ok {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"code":404,"name":"TrackNotFound"}`)

			return
		}

		json.NewEncoder(w).Encode(map[string]any{
			"trackName":    name,
			"duration":     durations[name],
			"syncedLyrics": lyrics,
		})
	})

	mux.HandleFunc("/api/search", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("track_name")

		out := []map[string]any{}

		if lyrics, ok := synced[name]; ok {
			out = append(out, map[string]any{
				"trackName":    name,
				"duration":     durations[name],
				"syncedLyrics": lyrics,
			})
		}

		json.NewEncoder(w).Encode(out)
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return server
}

func TestLyricsProviderFetch(t *testing.T) {
	server := fakeLRCLIB(t,
		map[string]string{"Known": "[00:01.00]placeholder line\n"},
		map[string]float64{"Known": 264})

	p := &LyricsProvider{
		Client:  server.Client(),
		Log:     NewLogger(io.Discard),
		BaseURL: server.URL,
	}

	got, err := p.Fetch(context.Background(), Track{
		Name: "Known", Artist: "Artist", Duration: "4:24"})

	if err != nil {
		t.Fatalf("fetch failed: %v", err)
	}

	if len(got.Lines) != 1 || got.Lines[0].At != 1000 {
		t.Errorf("got %+v", got.Lines)
	}

	// The raw LRC must survive for the USLT frame.
	if got.LRC == "" {
		t.Error("raw LRC text was dropped; USLT would be empty")
	}
}

// A track with no lyrics is a normal outcome, not an error.
func TestLyricsProviderMissingIsNotAnError(t *testing.T) {
	server := fakeLRCLIB(t, map[string]string{}, map[string]float64{})

	p := &LyricsProvider{
		Client:  server.Client(),
		Log:     NewLogger(io.Discard),
		BaseURL: server.URL,
	}

	got, err := p.Fetch(context.Background(), Track{
		Name: "Unknown", Artist: "Artist", Duration: "3:00"})

	if err != nil {
		t.Errorf("missing lyrics reported as an error: %v", err)
	}

	if len(got.Lines) != 0 {
		t.Errorf("expected no lines, got %d", len(got.Lines))
	}
}

// Search results too far from our duration are rejected.
func TestLyricsProviderRejectsWrongDuration(t *testing.T) {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/get", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	mux.HandleFunc("/api/search", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]any{{
			"trackName":    "Song",
			"duration":     400.0, // our track is 264s
			"syncedLyrics": "[00:01.00]placeholder\n",
		}})
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	p := &LyricsProvider{
		Client:  server.Client(),
		Log:     NewLogger(io.Discard),
		BaseURL: server.URL,
	}

	got, err := p.Fetch(context.Background(), Track{
		Name: "Song", Artist: "Artist", Duration: "4:24"})

	if err != nil {
		t.Fatal(err)
	}

	if len(got.Lines) != 0 {
		t.Errorf("accepted a recording 136s adrift: %+v", got.Lines)
	}
}

// The whole pipeline: lyrics are embedded when present, and their
// absence never fails a track.
func TestPipelineEmbedsLyricsAndToleratesMissing(t *testing.T) {
	svc := newFakeService(t, 0)
	dir := t.TempDir()

	lrclib := fakeLRCLIB(t,
		map[string]string{"Song 0": "[00:00.50]placeholder one\n[00:02.00]placeholder two\n"},
		map[string]float64{"Song 0": 0})

	processor, _ := newTestPipeline(t, svc, dir)

	processor.Lyrics = &LyricsProvider{
		Client:   lrclib.Client(),
		Log:      NewLogger(io.Discard),
		BaseURL:  lrclib.URL,
		Language: "eng",
	}
	processor.LyricsConcurrency = 2

	results := processor.Process(context.Background(), makeTracks(3))

	if len(results) != 3 {
		t.Fatalf("got %d results", len(results))
	}

	statuses := map[string]string{}

	for _, r := range results {
		if r.Error != nil {
			t.Errorf("track %d failed: %v", r.Track.Index, r.Error)
		}

		statuses[r.Track.Name] = r.Lyrics
	}

	if statuses["Song 0"] != "synced" {
		t.Errorf("Song 0 lyrics status = %q, want synced", statuses["Song 0"])
	}

	if statuses["Song 1"] != "none" {
		t.Errorf("Song 1 lyrics status = %q, want none", statuses["Song 1"])
	}

	// The file with lyrics carries the frame; the others do not.
	if !HasSyncedLyrics(filepath.Join(dir, "01 - Song 0 - Artist.mp3")) {
		t.Error("Song 0 has no SYLT frame")
	}

	if HasSyncedLyrics(filepath.Join(dir, "02 - Song 1 - Artist.mp3")) {
		t.Error("Song 1 should have no SYLT frame")
	}
}

// A broken lyrics service must not fail downloads.
func TestPipelineSurvivesLyricsOutage(t *testing.T) {
	svc := newFakeService(t, 0)
	dir := t.TempDir()

	broken := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
	defer broken.Close()

	processor, _ := newTestPipeline(t, svc, dir)

	processor.Lyrics = &LyricsProvider{
		Client:  broken.Client(),
		Log:     NewLogger(io.Discard),
		BaseURL: broken.URL,
	}

	results := processor.Process(context.Background(), makeTracks(3))

	for _, r := range results {
		if r.Error != nil {
			t.Errorf("track %d failed because of lyrics: %v", r.Track.Index, r.Error)
		}

		if r.Lyrics != "lookup failed" {
			t.Errorf("track %d status = %q", r.Track.Index, r.Lyrics)
		}
	}

	// Every file still landed.
	if n := len(listFiles(t, dir)); n != 3 {
		t.Errorf("got %d files, want 3", n)
	}
}

// Transcode mode must still embed lyrics.
//
// The lyrics stage used to re-derive the filename from item.saved,
// which is empty in transcode mode because saveid3 is skipped. The
// extension came out as "." and the stage looked for a file that did
// not exist, so every transcoded track silently lost its lyrics.
func TestTranscodeModeStillEmbedsLyrics(t *testing.T) {
	svc := newFakeService(t, 0)
	dir := t.TempDir()

	lrclib := fakeLRCLIB(t,
		map[string]string{"Song 0": "[00:00.50]placeholder one\n[00:02.00]placeholder two\n"},
		map[string]float64{"Song 0": 0})

	bin, _ := fakeFFmpeg(t, "ok")

	processor, _ := newTestPipeline(t, svc, dir)
	processor.AudioMode = audioModeTranscode
	processor.Transcoder = &Transcoder{Bitrate: 320, Path: bin}
	processor.Lyrics = &LyricsProvider{
		Client:   lrclib.Client(),
		Log:      NewLogger(io.Discard),
		BaseURL:  lrclib.URL,
		Language: "eng",
	}
	processor.LyricsConcurrency = 2

	results := processor.Process(context.Background(), makeTracks(2))

	statuses := map[string]string{}

	for _, r := range results {
		if r.Error != nil {
			t.Fatalf("track %d failed: %v", r.Track.Index, r.Error)
		}

		statuses[r.Track.Name] = r.Lyrics
	}

	if statuses["Song 0"] != "synced" {
		t.Errorf("Song 0 status = %q, want synced", statuses["Song 0"])
	}

	if !HasSyncedLyrics(filepath.Join(dir, "01 - Song 0 - Artist.mp3")) {
		t.Error("transcoded file has no lyrics frame")
	}
}

// filepath.Ext returns "." for an empty or dot-only name, which is not
// an extension. Treating it as one produced paths like "Track." that
// matched nothing on disk.
func TestExtensionNeverBareDot(t *testing.T) {
	for _, in := range []string{"", ".", "   "} {
		if got := cleanExtension(in); got != "" {
			t.Errorf("cleanExtension(%q) = %q, want empty", in, got)
		}
	}

	if got := extensionFromURL(""); got != "" {
		t.Errorf("extensionFromURL(\"\") = %q, want empty", got)
	}

	d := &Downloader{}

	// The transcode path passes an empty filename and empty URL.
	if got := d.extensionFor("", ""); got != ".mp3" {
		t.Errorf("extensionFor(\"\",\"\") = %q, want .mp3", got)
	}

	if got := d.extensionFor("Track - Artist.mp3", ""); got != ".mp3" {
		t.Errorf("got %q, want .mp3", got)
	}

	if got := d.extensionFor("Track - Artist.m4a", ""); got != ".m4a" {
		t.Errorf("got %q, want .m4a", got)
	}
}
