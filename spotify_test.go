package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIsSpotifyURL(t *testing.T) {
	spotify := []string{
		"https://open.spotify.com/track/4PTG3Z6ehGkBFwjybzWkR8",
		"https://open.spotify.com/intl-de/album/6N9PS4QXF1D0OWPk0Sxtb4",
		"spotify:track:4PTG3Z6ehGkBFwjybzWkR8",
	}

	other := []string{
		"https://music.apple.com/us/album/x/1?i=2",
		"Je suis fan",
		"",
	}

	for _, link := range spotify {
		if !isSpotifyURL(link) {
			t.Errorf("%q should be a Spotify link", link)
		}
	}

	for _, link := range other {
		if isSpotifyURL(link) {
			t.Errorf("%q is not a Spotify link", link)
		}
	}
}

func TestParseSpotifyURL(t *testing.T) {
	cases := []struct {
		link string
		kind string
		id   string
		ok   bool
	}{
		{
			link: "https://open.spotify.com/track/4PTG3Z6ehGkBFwjybzWkR8",
			kind: "track",
			id:   "4PTG3Z6ehGkBFwjybzWkR8",
			ok:   true,
		},
		{
			// What the desktop app copies.
			link: "https://open.spotify.com/intl-de/album/6N9PS4QXF1D0OWPk0Sxtb4?si=abc",
			kind: "album",
			id:   "6N9PS4QXF1D0OWPk0Sxtb4",
			ok:   true,
		},
		{
			link: "https://open.spotify.com/playlist/37i9dQZF1DXcBWIGoYBM5M",
			kind: "playlist",
			id:   "37i9dQZF1DXcBWIGoYBM5M",
			ok:   true,
		},
		{
			link: "spotify:track:4PTG3Z6ehGkBFwjybzWkR8",
			kind: "track",
			id:   "4PTG3Z6ehGkBFwjybzWkR8",
			ok:   true,
		},
		{
			// An artist page names no tracks to download.
			link: "https://open.spotify.com/artist/0gxyHStUsqpMadRV0Di1Qt",
			ok:   false,
		},
		{link: "https://music.apple.com/us/album/x/1", ok: false},
	}

	for _, c := range cases {
		kind, id, ok := parseSpotifyURL(c.link)

		if ok != c.ok || kind != c.kind || id != c.id {
			t.Errorf(
				"parseSpotifyURL(%q) = %q, %q, %v; want %q, %q, %v",
				c.link, kind, id, ok, c.kind, c.id, c.ok,
			)
		}
	}
}

// spotifyPage wraps state the way the embed player does.
func spotifyPage(entity string) string {
	return `<html><body><script id="__NEXT_DATA__" type="application/json">` +
		`{"props":{"pageProps":{"state":{"data":{"entity":` + entity +
		`}}}}}</script></body></html>`
}

const spotifyTrackEntity = `{
  "type": "track",
  "name": "Never Gonna Give You Up",
  "title": "Never Gonna Give You Up",
  "duration": 213573,
  "artists": [{"name": "Rick Astley"}]
}`

// A track list names its artist as a subtitle, not as an artists list.
const spotifyAlbumEntity = `{
  "type": "album",
  "name": "Whenever You Need Somebody",
  "trackList": [
    {"title": "Never Gonna Give You Up", "subtitle": "Rick Astley", "duration": 213573},
    {"title": "Together Forever", "subtitle": "Rick Astley", "duration": 205533},
    {"title": "", "subtitle": "Nobody", "duration": 1000}
  ]
}`

func TestParseSpotifyPageTrack(t *testing.T) {
	entity, err := parseSpotifyPage(spotifyPage(spotifyTrackEntity), "track")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if entity.Kind != "track" || entity.Name != "Never Gonna Give You Up" {
		t.Errorf("entity = %+v", entity)
	}

	if len(entity.Tracks) != 1 {
		t.Fatalf("got %d tracks, want 1", len(entity.Tracks))
	}

	track := entity.Tracks[0]

	if track.Artist != "Rick Astley" {
		t.Errorf("artist = %q", track.Artist)
	}

	// The duration is what tells the right recording from a cover.
	if track.Millis != 213573 {
		t.Errorf("duration = %d", track.Millis)
	}
}

func TestParseSpotifyPageTrackList(t *testing.T) {
	entity, err := parseSpotifyPage(spotifyPage(spotifyAlbumEntity), "album")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if entity.Kind != "album" || entity.Name != "Whenever You Need Somebody" {
		t.Errorf("entity = %+v", entity)
	}

	// The nameless row is not a track anyone can look for.
	if len(entity.Tracks) != 2 {
		t.Fatalf("got %d tracks, want 2", len(entity.Tracks))
	}

	if entity.Tracks[0].Artist != "Rick Astley" {
		t.Errorf("subtitle was not read as the artist: %+v", entity.Tracks[0])
	}
}

func TestParseSpotifyPageRejectsWhatItCannotRead(t *testing.T) {
	if _, err := parseSpotifyPage("<html>nothing here</html>", "track"); err == nil {
		t.Error("a page with no state should be an error")
	}

	if _, err := parseSpotifyPage(spotifyPage(`{"type":"track"}`), "track"); err == nil {
		t.Error("a page naming no tracks should be an error")
	}
}

func TestSpotifyReaderReadsALink(t *testing.T) {
	var asked string

	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			asked = r.URL.Path

			io.WriteString(w, spotifyPage(spotifyTrackEntity))
		},
	))

	defer server.Close()

	reader := &SpotifyReader{
		Client:   server.Client(),
		EmbedURL: server.URL,
	}

	entity, err := reader.Read(
		context.Background(),
		"https://open.spotify.com/track/4PTG3Z6ehGkBFwjybzWkR8",
	)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if asked != "/track/4PTG3Z6ehGkBFwjybzWkR8" {
		t.Errorf("fetched %q", asked)
	}

	if len(entity.Tracks) != 1 {
		t.Errorf("got %d tracks", len(entity.Tracks))
	}
}

func TestSpotifyReaderRejectsOtherLinks(t *testing.T) {
	reader := &SpotifyReader{}

	if _, err := reader.Read(
		context.Background(),
		"https://music.apple.com/us/album/x/1",
	); err == nil {
		t.Error("an Apple Music link is not a Spotify link")
	}
}

func TestFirstArtist(t *testing.T) {
	cases := map[string]string{
		"Rick Astley":                "Rick Astley",
		"Lady Gaga, Bradley Cooper":  "Lady Gaga",
		"Lady Gaga & Bradley Cooper": "Lady Gaga",
		"Drake feat. 21 Savage":      "Drake",
		"Someone ft. Another":        "Someone",
	}

	for input, want := range cases {
		if got := firstArtist(input); got != want {
			t.Errorf("firstArtist(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestMatchScore covers what separates the same recording from one that
// merely shares a name.
func TestMatchScore(t *testing.T) {
	want := SpotifyTrack{
		Name:   "Never Gonna Give You Up",
		Artist: "Rick Astley",
		Millis: 213573,
	}

	exact := SearchResult{
		Kind:   kindTrack,
		Name:   "Never Gonna Give You Up",
		Artist: "Rick Astley",
		Millis: 213000,
	}

	if got := matchScore(exact, want); got < minMatchScore {
		t.Errorf("the same recording scored %d, below the floor", got)
	}

	// A cover: right title, wrong artist.
	cover := exact
	cover.Artist = "Some Tribute Band"

	if got := matchScore(cover, want); got != 0 {
		t.Errorf("a different artist scored %d, want 0", got)
	}

	// Right title and artist, but half a minute longer: an extended
	// mix, a live take, something else.
	long := exact
	long.Millis = 213573 + 45000

	if got := matchScore(long, want); got != 0 {
		t.Errorf("a recording 45s longer scored %d, want 0", got)
	}

	// Unrelated title.
	if got := matchScore(SearchResult{
		Name:   "Shallow",
		Artist: "Rick Astley",
		Millis: 213573,
	}, want); got != 0 {
		t.Errorf("an unrelated title scored %d, want 0", got)
	}

	// A remaster keeps the title as a prefix and the length; it is the
	// same recording for this purpose.
	remaster := exact
	remaster.Name = "Never Gonna Give You Up (2022 Remaster)"

	if got := matchScore(remaster, want); got < minMatchScore {
		t.Errorf("a remaster scored %d, below the floor", got)
	}
}

// TestMatchTrackPrefersTheClosestRecording: several entries share the
// title, and the duration is what decides between them.
func TestMatchTrackPrefersTheClosestRecording(t *testing.T) {
	payload := `{
      "resultCount": 3,
      "results": [
        {
          "wrapperType":"track","kind":"song",
          "trackName":"Never Gonna Give You Up (Live)",
          "artistName":"Rick Astley",
          "collectionName":"Live",
          "trackViewUrl":"https://music.apple.com/us/album/live/1?i=11",
          "trackTimeMillis": 260000
        },
        {
          "wrapperType":"track","kind":"song",
          "trackName":"Never Gonna Give You Up",
          "artistName":"Rick Astley",
          "collectionName":"Whenever You Need Somebody",
          "trackViewUrl":"https://music.apple.com/us/album/whenever/2?i=22",
          "trackTimeMillis": 213000
        },
        {
          "wrapperType":"track","kind":"song",
          "trackName":"Never Gonna Give You Up",
          "artistName":"Karaoke Crowd",
          "collectionName":"Karaoke Hits",
          "trackViewUrl":"https://music.apple.com/us/album/karaoke/3?i=33",
          "trackTimeMillis": 213000
        }
      ]
    }`

	catalogue := &fakeCatalogue{
		songs: map[string]string{
			"Never Gonna Give You Up Rick Astley": payload,
		},
	}

	matches, err := catalogue.search(t).MatchTrack(
		context.Background(),
		SpotifyTrack{
			Name:   "Never Gonna Give You Up",
			Artist: "Rick Astley",
			Millis: 213573,
		},
	)
	if err != nil {
		t.Fatalf("match: %v", err)
	}

	if len(matches) == 0 {
		t.Fatal("nothing matched")
	}

	best := matches[0]

	if best.Album != "Whenever You Need Somebody" {
		t.Errorf("best match is %q by %q, want the studio recording",
			best.Album, best.Artist)
	}

	// The karaoke version is by someone else and must not be offered at
	// all.
	for _, match := range matches {
		if match.Artist == "Karaoke Crowd" {
			t.Error("a different artist was offered as a match")
		}
	}
}

func TestMatchTrackFindsNothingRatherThanGuessing(t *testing.T) {
	catalogue := &fakeCatalogue{}

	matches, err := catalogue.search(t).MatchTrack(
		context.Background(),
		SpotifyTrack{Name: "Nothing At All", Artist: "Nobody"},
	)
	if err != nil {
		t.Fatalf("match: %v", err)
	}

	if len(matches) != 0 {
		t.Errorf("got %d matches for a song that is not there", len(matches))
	}
}

// TestMatchedTrackIsDownloadable is the join: whatever the matcher
// returns has to be something the pipeline can actually fetch.
func TestMatchedTrackIsDownloadable(t *testing.T) {
	catalogue := &fakeCatalogue{
		songs: map[string]string{"Je suis fan Alice et Moi": searchPayload},
	}

	matches, err := catalogue.search(t).MatchTrack(
		context.Background(),
		SpotifyTrack{
			Name:   "Je suis fan",
			Artist: "Alice et Moi",
			Millis: 168029,
		},
	)
	if err != nil {
		t.Fatalf("match: %v", err)
	}

	if len(matches) == 0 {
		t.Fatal("nothing matched")
	}

	track := matches[0].Track(0)

	if !isAppleMusicSongURL(track.Link) {
		t.Errorf("matched link is not downloadable: %s", track.Link)
	}

	if got := trackIdentity(track); !strings.HasPrefix(got, "am:") {
		t.Errorf("matched track has no archive key: %s", got)
	}
}
