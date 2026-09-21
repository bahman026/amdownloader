package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeCatalog serves paginated playlist tracks like the Apple Music
// catalog API: `pages` pages of `per` tracks each.
func fakeCatalog(t *testing.T, pages, per int) (*httptest.Server, *atomic.Int64) {
	t.Helper()

	var requests atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)

			if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(w, `{"errors":[{"title":"bad token"}]}`)

				return
			}

			offset := 0
			fmt.Sscanf(r.URL.Query().Get("offset"), "%d", &offset)

			page := offset / per

			type item struct {
				Attributes struct {
					Name             string `json:"name"`
					ArtistName       string `json:"artistName"`
					AlbumName        string `json:"albumName"`
					DurationInMillis int64  `json:"durationInMillis"`
					URL              string `json:"url"`
					Artwork          struct {
						URL string `json:"url"`
					} `json:"artwork"`
				} `json:"attributes"`
			}

			out := struct {
				Next string `json:"next,omitempty"`
				Data []item `json:"data"`
			}{}

			for i := 0; i < per; i++ {
				var it item
				n := offset + i
				it.Attributes.Name = fmt.Sprintf("Song %d", n)
				it.Attributes.ArtistName = "Artist"
				it.Attributes.AlbumName = "Album"
				it.Attributes.DurationInMillis = 172054
				it.Attributes.URL = fmt.Sprintf("https://music.apple.com/tr/album/x/1?i=%d", n)
				it.Attributes.Artwork.URL = "https://is1.mzstatic.com/a/{w}x{h}{c}.{f}"
				out.Data = append(out.Data, it)
			}

			if page+1 < pages {
				out.Next = fmt.Sprintf("/tracks?offset=%d", offset+per)
			}

			json.NewEncoder(w).Encode(out)
		}))

	t.Cleanup(server.Close)

	return server, &requests
}

// TestPlaylistPaginationIsFollowed is the ~300 track bug: Apple embeds
// only the first page in the HTML, and everything past it was invisible.
func TestPlaylistPaginationIsFollowed(t *testing.T) {
	server, requests := fakeCatalog(t, 4, 100)

	old := appleMusicAPIBase
	appleMusicAPIBase = server.URL
	oldDelay := paginationDelay
	paginationDelay = 0
	t.Cleanup(func() { appleMusicAPIBase = old; paginationDelay = oldDelay })

	tracks, err := fetchPlaylistTracksFrom(
		&http.Client{Timeout: 10 * time.Second},
		"test-token",
		"/tracks?offset=0",
	)

	if err != nil {
		t.Fatalf("pagination failed: %v", err)
	}

	if len(tracks) != 400 {
		t.Errorf("got %d tracks across the chain, want 400", len(tracks))
	}

	if got := requests.Load(); got != 4 {
		t.Errorf("made %d requests, want 4", got)
	}

	// Field mapping.
	first := tracks[0]

	if first.Name != "Song 0" || first.Artist != "Artist" || first.Album != "Album" {
		t.Errorf("bad mapping: %+v", first)
	}

	if !strings.Contains(first.Link, "?i=0") {
		t.Errorf("link must keep the ?i= form the resolver needs: %q", first.Link)
	}

	if first.Duration != "2:52" {
		t.Errorf("duration = %q, want 2:52", first.Duration)
	}

	// Artwork templates must be expanded, not passed through raw.
	if strings.Contains(first.Thumb, "{w}") || strings.Contains(first.Thumb, "{f}") {
		t.Errorf("artwork template not normalised: %q", first.Thumb)
	}
}

// TestPaginationIsBounded stops a malformed next pointer looping forever.
func TestPaginationIsBounded(t *testing.T) {
	var requests atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			// Always claims there is another page.
			fmt.Fprint(w, `{"next":"/tracks?offset=1","data":[]}`)
		}))
	defer server.Close()

	old := appleMusicAPIBase
	appleMusicAPIBase = server.URL
	oldDelay := paginationDelay
	paginationDelay = 0
	t.Cleanup(func() { appleMusicAPIBase = old; paginationDelay = oldDelay })

	_, err := fetchPlaylistTracksFrom(
		&http.Client{Timeout: 10 * time.Second},
		"test-token",
		"/tracks?offset=0",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := requests.Load(); got > maxPlaylistPages {
		t.Errorf("pagination unbounded: %d requests", got)
	}
}

// TestPaginationKeepsPartialResultsOnFailure: a mid-chain failure must
// not throw away the tracks already collected.
func TestPaginationKeepsPartialResultsOnFailure(t *testing.T) {
	var requests atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			if requests.Add(1) == 1 {
				fmt.Fprint(w, `{"next":"/tracks?offset=1","data":[
                    {"attributes":{"name":"Kept","artistName":"A","url":"https://x/?i=1"}}]}`)

				return
			}

			w.WriteHeader(http.StatusInternalServerError)
		}))
	defer server.Close()

	old := appleMusicAPIBase
	appleMusicAPIBase = server.URL
	oldDelay := paginationDelay
	paginationDelay = 0
	t.Cleanup(func() { appleMusicAPIBase = old; paginationDelay = oldDelay })

	tracks, err := fetchPlaylistTracksFrom(
		&http.Client{Timeout: 10 * time.Second},
		"test-token",
		"/tracks?offset=0",
	)

	if err == nil {
		t.Fatal("expected the second page to report an error")
	}

	if len(tracks) != 1 || tracks[0].Name != "Kept" {
		t.Errorf("partial results were discarded: %+v", tracks)
	}
}

// TestNextIntentIsParsed pins the pointer the page hands us.
func TestNextIntentIsParsed(t *testing.T) {
	// Shape copied from a real Apple Music playlist page:
	// a top-level object with "data", each entry wrapping another "data".
	page := `<html><head>
<script type="application/json" id="serialized-server-data">
{"data":[{"data":{"sections":[{"items":[]}],
"nextIntent":{"$kind":"PlaylistPaginationIntent",
"url":"/v1/catalog/tr/playlists/pl.abc/tracks?l=en-GB&offset=300"}}}]}
</script></head></html>`

	parsed, err := extractAppleMusicPage(page)
	if err != nil {
		t.Fatalf("extract failed: %v", err)
	}

	got := parsed.Data[0].Data.NextIntent.URL

	if !strings.Contains(got, "offset=300") {
		t.Errorf("nextIntent.url not parsed, got %q", got)
	}

	if parsed.Data[0].Data.NextIntent.Kind != "PlaylistPaginationIntent" {
		t.Errorf("nextIntent.$kind not parsed")
	}
}

// TestTracksWithoutRequiredFieldsAreSkipped: the resolver needs name,
// artist and link.
func TestTracksWithoutRequiredFieldsAreSkipped(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":[
                {"attributes":{"name":"Good","artistName":"A","url":"https://x/?i=1"}},
                {"attributes":{"name":"","artistName":"A","url":"https://x/?i=2"}},
                {"attributes":{"name":"NoArtist","artistName":"","url":"https://x/?i=3"}},
                {"attributes":{"name":"NoLink","artistName":"A","url":""}}]}`)
		}))
	defer server.Close()

	old := appleMusicAPIBase
	appleMusicAPIBase = server.URL
	oldDelay := paginationDelay
	paginationDelay = 0
	t.Cleanup(func() { appleMusicAPIBase = old; paginationDelay = oldDelay })

	tracks, err := fetchPlaylistTracksFrom(
		&http.Client{Timeout: 10 * time.Second},
		"test-token",
		"/tracks",
	)
	if err != nil {
		t.Fatal(err)
	}

	if len(tracks) != 1 || tracks[0].Name != "Good" {
		t.Errorf("expected only the complete track, got %+v", tracks)
	}
}
