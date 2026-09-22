package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// searchPayload is a trimmed copy of a real itunes.apple.com response,
// including the kinds and gaps the endpoint mixes in.
const searchPayload = `{
  "resultCount": 4,
  "results": [
    {
      "wrapperType": "track",
      "kind": "song",
      "artistName": "Alice et Moi",
      "collectionName": "DRAMA",
      "trackName": "Je suis fan",
      "trackViewUrl": "https://music.apple.com/us/album/je-suis-fan/1562865138?i=1562865145&uo=4",
      "artworkUrl100": "https://is1-ssl.mzstatic.com/image/thumb/Music124/v4/50/ae/4b/x.jpg/100x100bb.jpg",
      "releaseDate": "2021-01-15T08:00:00Z",
      "trackExplicitness": "notExplicit",
      "trackTimeMillis": 168029
    },
    {
      "wrapperType": "track",
      "kind": "song",
      "artistName": "Someone Else",
      "collectionName": "Je suis fan - Single",
      "trackName": "Je suis fan (Remix)",
      "trackViewUrl": "https://music.apple.com/us/album/je-suis-fan/1540802931?i=1540802935&uo=4",
      "artworkUrl100": "https://is1-ssl.mzstatic.com/image/thumb/Music124/v4/b9/eb/06/y.jpg/100x100bb.jpg",
      "releaseDate": "2020-11-06T12:00:00Z",
      "trackExplicitness": "explicit",
      "trackTimeMillis": 245000
    },
    {
      "wrapperType": "track",
      "kind": "music-video",
      "artistName": "Alice et Moi",
      "trackName": "Je suis fan (Video)",
      "trackViewUrl": "https://music.apple.com/us/music-video/je-suis-fan/999?uo=4",
      "trackTimeMillis": 168029
    },
    {
      "wrapperType": "track",
      "kind": "song",
      "artistName": "No Link",
      "trackName": "Unreachable",
      "trackTimeMillis": 100000
    }
  ]
}`

// newSearchServer serves payload and records the query it was asked.
func newSearchServer(
	t *testing.T,
	status int,
	payload string,
) (*SongSearch, *url.Values) {

	t.Helper()

	asked := &url.Values{}

	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			*asked = r.URL.Query()

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)

			io.WriteString(w, payload)
		},
	))

	t.Cleanup(server.Close)

	return &SongSearch{
		Client:   server.Client(),
		Endpoint: server.URL,
	}, asked
}

func TestSearchParsesResults(t *testing.T) {
	search, _ := newSearchServer(t, http.StatusOK, searchPayload)

	results, err := search.Songs(context.Background(), "je suis fan")
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	// The music video has no downloadable track link, and the last
	// entry has no link at all: neither belongs in a list to pick from.
	if len(results) != 2 {
		t.Fatalf("got %d results, want the 2 usable ones: %+v", len(results), results)
	}

	first := results[0]

	if first.Name != "Je suis fan" || first.Artist != "Alice et Moi" {
		t.Errorf("wrong name or artist: %+v", first)
	}

	if first.Album != "DRAMA" {
		t.Errorf("album = %q, want DRAMA", first.Album)
	}

	if first.Duration != "2:48" {
		t.Errorf("duration = %q, want 2:48", first.Duration)
	}

	if first.Year != "2021" {
		t.Errorf("year = %q, want 2021", first.Year)
	}

	if first.Explicit {
		t.Error("first result is not explicit")
	}

	if !results[1].Explicit {
		t.Error("second result is explicit and should be marked")
	}

	// Artwork must come back at the size the tagger embeds, not the
	// 100px thumbnail the endpoint hands out.
	if !strings.Contains(first.Thumb, "600x600bb") {
		t.Errorf("artwork not upgraded: %s", first.Thumb)
	}

	// The affiliate parameter is dropped; the track id is not.
	if strings.Contains(first.Link, "uo=4") {
		t.Errorf("tracking parameter kept: %s", first.Link)
	}
}

// TestSearchResultIsDownloadable is the join between the two halves: a
// result has to be something the existing single-song path accepts, and
// something the archive can key on.
func TestSearchResultIsDownloadable(t *testing.T) {
	search, _ := newSearchServer(t, http.StatusOK, searchPayload)

	results, err := search.Songs(context.Background(), "je suis fan")
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	for _, result := range results {
		if !isAppleMusicSongURL(result.Link) {
			t.Errorf("link is not a song URL: %s", result.Link)
		}

		track := result.Track(0)

		if got := trackIdentity(track); !strings.HasPrefix(got, "am:") {
			t.Errorf("no Apple Music id behind %q: %s", result.Name, got)
		}
	}
}

func TestSearchAsksForSongsOnly(t *testing.T) {
	search, asked := newSearchServer(t, http.StatusOK, searchPayload)

	search.Country = "DE"
	search.Limit = 7

	if _, err := search.Songs(context.Background(), "je suis fan"); err != nil {
		t.Fatalf("search: %v", err)
	}

	want := map[string]string{
		"term":    "je suis fan",
		"entity":  "song",
		"media":   "music",
		"limit":   "7",
		"country": "de",
	}

	for key, value := range want {
		if got := asked.Get(key); got != value {
			t.Errorf("%s = %q, want %q", key, got, value)
		}
	}
}

func TestSearchDefaultsWhenUnconfigured(t *testing.T) {
	search, asked := newSearchServer(t, http.StatusOK, searchPayload)

	// Country left empty and limit zero: the request must still be
	// valid rather than asking for 0 results in no storefront.
	if _, err := search.Songs(context.Background(), "anything"); err != nil {
		t.Fatalf("search: %v", err)
	}

	if got := asked.Get("country"); got != defaultSearchCountry {
		t.Errorf("country = %q, want %q", got, defaultSearchCountry)
	}

	if got := asked.Get("limit"); got != fmt.Sprint(defaultSearchLimit) {
		t.Errorf("limit = %q, want %d", got, defaultSearchLimit)
	}
}

func TestSearchExplainsRateLimiting(t *testing.T) {
	search, _ := newSearchServer(t, http.StatusForbidden, "denied")

	_, err := search.Songs(context.Background(), "je suis fan")

	if err == nil {
		t.Fatal("a 403 should be an error")
	}

	if !strings.Contains(err.Error(), "rate-limit") {
		t.Errorf("error does not mention the cause: %v", err)
	}
}

func TestSearchRejectsUnreadableJSON(t *testing.T) {
	search, _ := newSearchServer(t, http.StatusOK, "<html>nope</html>")

	if _, err := search.Songs(context.Background(), "je suis fan"); err == nil {
		t.Fatal("unreadable JSON should be an error")
	}
}

func TestSearchRefusesAnEmptyQuery(t *testing.T) {
	search, _ := newSearchServer(t, http.StatusOK, searchPayload)

	if _, err := search.Songs(context.Background(), "   "); err == nil {
		t.Fatal("an empty query should be an error, not a request")
	}
}

func TestParseSelection(t *testing.T) {
	cases := []struct {
		input string
		want  []int
		fails bool
	}{
		{input: "1", want: []int{0}},
		{input: "3", want: []int{2}},
		{input: "1 3 5", want: []int{0, 2, 4}},
		{input: "1,3,5", want: []int{0, 2, 4}},
		{input: "2-4", want: []int{1, 2, 3}},
		{input: "4-2", want: []int{1, 2, 3}},
		{input: "1 2-3 1", want: []int{0, 1, 2}},
		{input: "  2  ", want: []int{1}},
		{input: "a", want: []int{0, 1, 2, 3, 4}},
		{input: "all", want: []int{0, 1, 2, 3, 4}},
		{input: "", want: nil},
		{input: "q", want: nil},
		{input: "0", fails: true},
		{input: "6", fails: true},
		{input: "2-9", fails: true},
		{input: "banana", fails: true},
		{input: "1-", fails: true},
	}

	for _, c := range cases {
		got, err := parseSelection(c.input, 5)

		if c.fails {
			if err == nil {
				t.Errorf("parseSelection(%q) should have failed, got %v", c.input, got)
			}

			continue
		}

		if err != nil {
			t.Errorf("parseSelection(%q): %v", c.input, err)

			continue
		}

		if fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("parseSelection(%q) = %v, want %v", c.input, got, c.want)
		}
	}
}

// TestPromptSelectionRetries: the list is still on screen, so a typo
// costs one line, not the whole search.
func TestPromptSelectionRetries(t *testing.T) {
	var out bytes.Buffer

	chosen := promptSelection(
		strings.NewReader("9\nbanana\n2\n"),
		&out,
		3,
	)

	if fmt.Sprint(chosen) != fmt.Sprint([]int{1}) {
		t.Errorf("chose %v, want [1]", chosen)
	}

	if !strings.Contains(out.String(), "there is no result 9") {
		t.Errorf("the mistake was not explained: %q", out.String())
	}
}

func TestPromptSelectionCancels(t *testing.T) {
	for _, input := range []string{"\n", "q\n", ""} {
		var out bytes.Buffer

		if chosen := promptSelection(
			strings.NewReader(input),
			&out,
			3,
		); len(chosen) != 0 {
			t.Errorf("input %q selected %v, want nothing", input, chosen)
		}
	}
}

// TestPromptSelectionStopsAtEndOfInput guards against a prompt that
// spins forever when stdin closes mid-answer.
func TestPromptSelectionStopsAtEndOfInput(t *testing.T) {
	var out bytes.Buffer

	done := make(chan []int, 1)

	go func() {
		done <- promptSelection(strings.NewReader("99"), &out, 3)
	}()

	select {
	case chosen := <-done:
		if len(chosen) != 0 {
			t.Errorf("selected %v after a rejected answer with no more input", chosen)
		}

	case <-context.Background().Done():
	}
}

func TestSearchResultBecomesTrack(t *testing.T) {
	result := SearchResult{
		Name:     "Je suis fan",
		Artist:   "Alice et Moi",
		Duration: "2:48",
		Link:     "https://music.apple.com/us/album/je-suis-fan/1?i=2",
		Thumb:    "https://example.test/600x600bb.jpg",
	}

	track := result.Track(3)

	if track.Index != 3 {
		t.Errorf("index = %d, want 3", track.Index)
	}

	// An album is written into the ID3 tag and used for the folder
	// name, so it can never be empty.
	if track.Album != "Unknown Album" {
		t.Errorf("album = %q, want Unknown Album", track.Album)
	}

	if track.Name != result.Name || track.Link != result.Link {
		t.Errorf("track does not carry the result: %+v", track)
	}
}

// TestSearchListingMarksWhatIsAlreadyDownloaded: several releases of one
// song look identical in a list, so the one already held has to say so.
func TestSearchListingMarksWhatIsAlreadyDownloaded(t *testing.T) {
	path := archivePath(t)

	archive, err := LoadArchive(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	results := []SearchResult{
		{
			Name:   "Je suis fan",
			Artist: "Alice et Moi",
			Album:  "DRAMA",
			Link:   "https://music.apple.com/us/album/x/1?i=1562865145",
		},
		{
			Name:   "Je suis fan",
			Artist: "Alice et Moi",
			Album:  "Je suis fan - Single",
			Link:   "https://music.apple.com/us/album/y/2?i=1540802935",
		},
	}

	if err := archive.Add(newArchiveEntry(
		results[1].Track(0), "", 0, audioModeService, 128,
	)); err != nil {
		t.Fatalf("add: %v", err)
	}

	var out bytes.Buffer

	printSearchResults(&out, results, archive)

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")

	if len(lines) != 3 {
		t.Fatalf("expected a header and 2 rows, got %d lines", len(lines))
	}

	if strings.Contains(lines[1], "already downloaded") {
		t.Errorf("row 1 is not in the archive: %q", lines[1])
	}

	if !strings.Contains(lines[2], "already downloaded") {
		t.Errorf("row 2 is in the archive and should say so: %q", lines[2])
	}

	// Marking must not consume the record. The pipeline loads its own
	// copy from file when the download starts, so that is the copy the
	// check has to be made against.
	reloaded, err := LoadArchive(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}

	if !reloaded.Claim(results[1].Track(0)) {
		t.Error("listing the results spent the archive record")
	}
}

// TestLooksLikeLink keeps a mistyped URL out of the search path, where
// it would come back as a list of unrelated songs.
func TestLooksLikeLink(t *testing.T) {
	links := []string{
		"https://music.apple.com/us/album/x/1?i=2",
		"http://example.test/thing",
		"music.apple.com/us/album/x/1",
	}

	queries := []string{
		"Je suis fan",
		"alice et moi drama",
		"2001 dr dre",
	}

	for _, value := range links {
		if !looksLikeLink(value) {
			t.Errorf("%q should be treated as a link", value)
		}
	}

	for _, value := range queries {
		if looksLikeLink(value) {
			t.Errorf("%q should be treated as a search", value)
		}
	}
}

// ---------------------------------------------------------------
// Near-spelling fallback
// ---------------------------------------------------------------

func TestCollapseRepeats(t *testing.T) {
	cases := map[string]string{
		"tanhaeeia":  "tanhaeia",
		"tanhaeeiam": "tanhaeiam",
		"aaa":        "a",
		"Bee Gees":   "Be Ges",
		"shallow":    "shalow",
		"abc":        "abc",
		"":           "",
	}

	for input, want := range cases {
		if got := collapseRepeats(input); got != want {
			t.Errorf("collapseRepeats(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestSearchVariants(t *testing.T) {
	// The reported case: the catalogue spells it "tanhaeia", and the
	// collapsed spelling has to be the first thing tried.
	variants := searchVariants("tanhaeeia")

	if len(variants) == 0 || variants[0] != "tanhaeia" {
		t.Fatalf("variants for tanhaeeia = %v, want tanhaeia first", variants)
	}

	if len(variants) > maxSearchVariants {
		t.Errorf("%d variants would mean %d extra requests", len(variants), len(variants))
	}

	// The original spelling is never retried, and nothing repeats.
	seen := map[string]bool{"tanhaeeia": true}

	for _, variant := range variants {
		if seen[variant] {
			t.Errorf("variant %q is a repeat", variant)
		}

		seen[variant] = true
	}

	// A short word has no stub to cut down to.
	if got := searchVariants("fan"); len(got) != 0 {
		t.Errorf("variants for a short word = %v, want none", got)
	}

	// On a phrase, the longest word is the one worked on, and the
	// phrase's other words are kept.
	phrase := searchVariants("tanhaeeia siavash")

	if len(phrase) == 0 || !strings.Contains(phrase[0], "siavash") {
		t.Errorf("phrase variants dropped the rest of the query: %v", phrase)
	}
}

// fakeCatalogue answers per term, so a fallback can be told apart from
// the original query.
type fakeCatalogue struct {
	songs   map[string]string
	albums  map[string]string
	artists map[string]string
	lookup  map[string]string

	terms []string
}

func (c *fakeCatalogue) search(t *testing.T) *SongSearch {
	t.Helper()

	const empty = `{"resultCount":0,"results":[]}`

	handler := func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()

		w.Header().Set("Content-Type", "application/json")

		if id := query.Get("id"); id != "" {
			payload, ok := c.lookup[id]

			if !ok {
				payload = empty
			}

			io.WriteString(w, payload)

			return
		}

		term := query.Get("term")

		source := c.songs

		switch query.Get("entity") {

		case "album":
			source = c.albums

		case "musicArtist":
			source = c.artists

		default:
			c.terms = append(c.terms, term)
		}

		payload, ok := source[term]

		if !ok {
			payload = empty
		}

		io.WriteString(w, payload)
	}

	server := httptest.NewServer(http.HandlerFunc(handler))

	t.Cleanup(server.Close)

	return &SongSearch{
		Client:         server.Client(),
		Endpoint:       server.URL,
		LookupEndpoint: server.URL,
	}
}

// TestSearchFallsBackToANearSpelling is the reported bug: "tanhaeeia"
// found nothing while "tanhaeia" found five.
func TestSearchFallsBackToANearSpelling(t *testing.T) {
	catalogue := &fakeCatalogue{
		songs: map[string]string{
			"tanhaeia": searchPayload,
		},
	}

	search := catalogue.search(t)

	outcome, err := search.Search(context.Background(), searchQuery{Terms: "tanhaeeia"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	if len(outcome.Results) == 0 {
		t.Fatal("the near spelling was never tried")
	}

	if outcome.Query != "tanhaeia" {
		t.Errorf("results came from %q, want tanhaeia", outcome.Query)
	}

	// What was tried has to be reportable: the list shown is not
	// literally what was asked for.
	if len(outcome.Tried) == 0 || outcome.Tried[0] != "tanhaeia" {
		t.Errorf("tried = %v, want tanhaeia first", outcome.Tried)
	}

	// It stops as soon as something is found.
	if len(catalogue.terms) != 2 {
		t.Errorf("made %d song queries (%v), want 2", len(catalogue.terms), catalogue.terms)
	}
}

func TestSearchDoesNotFallBackWhenTheQueryWorks(t *testing.T) {
	catalogue := &fakeCatalogue{
		songs: map[string]string{
			"je suis fan": searchPayload,
		},
	}

	search := catalogue.search(t)

	outcome, err := search.Search(context.Background(), searchQuery{Terms: "je suis fan"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	if outcome.Query != "je suis fan" || len(outcome.Tried) != 0 {
		t.Errorf("a query that worked was second-guessed: %+v", outcome.Tried)
	}

	if len(catalogue.terms) != 1 {
		t.Errorf("made %d song queries, want 1", len(catalogue.terms))
	}
}

func TestSearchGivesUpAfterTheVariants(t *testing.T) {
	catalogue := &fakeCatalogue{}

	search := catalogue.search(t)

	outcome, err := search.Search(context.Background(), searchQuery{Terms: "nothingmatchesthis"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	if len(outcome.Results) != 0 {
		t.Fatal("expected no results")
	}

	// Bounded: Apple rate-limits this endpoint, so a dead query must
	// not turn into a burst.
	if len(catalogue.terms) > 1+maxSearchVariants {
		t.Errorf("made %d queries, want at most %d", len(catalogue.terms), 1+maxSearchVariants)
	}

	if outcome.Query != "nothingmatchesthis" {
		t.Errorf("query = %q, want the original back", outcome.Query)
	}
}

// ---------------------------------------------------------------
// Albums
// ---------------------------------------------------------------

const albumSearchPayload = `{
  "resultCount": 1,
  "results": [
    {
      "wrapperType": "collection",
      "collectionType": "Album",
      "collectionId": 1562865138,
      "artistName": "Alice et Moi",
      "collectionName": "DRAMA",
      "collectionViewUrl": "https://music.apple.com/us/album/drama/1562865138?uo=4",
      "artworkUrl100": "https://is1-ssl.mzstatic.com/image/thumb/x.jpg/100x100bb.jpg",
      "trackCount": 16,
      "releaseDate": "2021-05-21T07:00:00Z"
    }
  ]
}`

// albumLookupPayload is the album followed by its tracks, out of order,
// as the endpoint can return them.
const albumLookupPayload = `{
  "resultCount": 4,
  "results": [
    {
      "wrapperType": "collection",
      "collectionType": "Album",
      "collectionId": 1562865138,
      "artistName": "Alice et Moi",
      "collectionName": "DRAMA",
      "collectionViewUrl": "https://music.apple.com/us/album/drama/1562865138?uo=4",
      "trackCount": 3
    },
    {
      "wrapperType": "track",
      "kind": "song",
      "trackName": "Third",
      "artistName": "Alice et Moi",
      "collectionName": "DRAMA",
      "trackViewUrl": "https://music.apple.com/us/album/drama/1562865138?i=3&uo=4",
      "discNumber": 1,
      "trackNumber": 3,
      "trackTimeMillis": 180000
    },
    {
      "wrapperType": "track",
      "kind": "song",
      "trackName": "First",
      "artistName": "Alice et Moi",
      "collectionName": "DRAMA",
      "trackViewUrl": "https://music.apple.com/us/album/drama/1562865138?i=1&uo=4",
      "discNumber": 1,
      "trackNumber": 1,
      "trackTimeMillis": 120000
    },
    {
      "wrapperType": "track",
      "kind": "song",
      "trackName": "Second",
      "artistName": "Alice et Moi",
      "collectionName": "DRAMA",
      "trackViewUrl": "https://music.apple.com/us/album/drama/1562865138?i=2&uo=4",
      "discNumber": 1,
      "trackNumber": 2,
      "trackTimeMillis": 150000
    }
  ]
}`

func TestAlbumSearchResults(t *testing.T) {
	catalogue := &fakeCatalogue{
		albums: map[string]string{"drama": albumSearchPayload},
	}

	results, err := catalogue.search(t).Albums(context.Background(), "drama")
	if err != nil {
		t.Fatalf("albums: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("got %d albums, want 1", len(results))
	}

	album := results[0]

	if !album.IsAlbum() || album.Kind != kindAlbum {
		t.Errorf("result is not marked as an album: %+v", album)
	}

	if album.AlbumID != 1562865138 {
		t.Errorf("album id = %d", album.AlbumID)
	}

	if got := album.Detail(); got != "16 tracks, 2021" {
		t.Errorf("detail = %q, want \"16 tracks, 2021\"", got)
	}
}

// TestSearchShowsTracksThenAlbums: both kinds come back from one query,
// tracks first so a song search is not pushed down the screen.
func TestSearchShowsTracksThenAlbums(t *testing.T) {
	catalogue := &fakeCatalogue{
		songs:  map[string]string{"drama": searchPayload},
		albums: map[string]string{"drama": albumSearchPayload},
	}

	outcome, err := catalogue.search(t).Search(context.Background(), searchQuery{Terms: "drama"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	if len(outcome.Results) != 3 {
		t.Fatalf("got %d results, want 2 tracks and 1 album", len(outcome.Results))
	}

	if outcome.Results[0].IsAlbum() || outcome.Results[1].IsAlbum() {
		t.Error("tracks should come first")
	}

	if !outcome.Results[2].IsAlbum() {
		t.Error("the album should come last")
	}
}

func TestAlbumTracksAreInRunningOrder(t *testing.T) {
	catalogue := &fakeCatalogue{
		lookup: map[string]string{"1562865138": albumLookupPayload},
	}

	tracks, err := catalogue.search(t).AlbumTracks(context.Background(), 1562865138)
	if err != nil {
		t.Fatalf("album tracks: %v", err)
	}

	if len(tracks) != 3 {
		t.Fatalf("got %d tracks, want 3 (the album itself is not a track)", len(tracks))
	}

	want := []string{"First", "Second", "Third"}

	for i, track := range tracks {
		if track.Name != want[i] {
			t.Errorf("track %d is %q, want %q", i, track.Name, want[i])
		}

		// The position is what becomes the "NN - " prefix on disk.
		if track.Index != i {
			t.Errorf("track %q has index %d, want %d", track.Name, track.Index, i)
		}

		if !isAppleMusicSongURL(track.Link) {
			t.Errorf("track %q has an undownloadable link: %s", track.Name, track.Link)
		}
	}
}

func TestAlbumTracksReportsAnEmptyAlbum(t *testing.T) {
	catalogue := &fakeCatalogue{}

	if _, err := catalogue.search(t).AlbumTracks(
		context.Background(),
		999,
	); err == nil {
		t.Fatal("an album with no tracks should be an error, not an empty run")
	}
}

func TestValidateSelection(t *testing.T) {
	results := []SearchResult{
		{Kind: kindTrack},
		{Kind: kindTrack},
		{Kind: kindAlbum},
		{Kind: kindAlbum},
	}

	cases := []struct {
		name   string
		chosen []int
		fails  bool
	}{
		{name: "one track", chosen: []int{0}},
		{name: "several tracks", chosen: []int{0, 1}},
		{name: "one album", chosen: []int{2}},
		{name: "album with a track", chosen: []int{0, 2}, fails: true},
		{name: "two albums", chosen: []int{2, 3}, fails: true},
	}

	for _, c := range cases {
		err := validateSelection(results, c.chosen)

		if c.fails && err == nil {
			t.Errorf("%s should be rejected", c.name)
		}

		if !c.fails && err != nil {
			t.Errorf("%s should be allowed: %v", c.name, err)
		}
	}
}

func TestAppleMusicAlbumID(t *testing.T) {
	cases := map[string]int64{
		"https://music.apple.com/us/album/drama/1562865138":    1562865138,
		"https://music.apple.com/tr/album/rasputin/1553504894": 1553504894,
		"https://music.apple.com/us/playlist/x/pl.abc":         0,
		"not a url": 0,
	}

	for link, want := range cases {
		if got := appleMusicAlbumID(link); got != want {
			t.Errorf("appleMusicAlbumID(%q) = %d, want %d", link, got, want)
		}
	}
}

func TestAppleMusicStorefront(t *testing.T) {
	cases := map[string]string{
		"https://music.apple.com/tr/album/x/1": "tr",
		"https://music.apple.com/us/album/x/1": "us",
		"https://music.apple.com/album/x/1":    "",
		"":                                     "",
	}

	for link, want := range cases {
		if got := appleMusicStorefront(link); got != want {
			t.Errorf("appleMusicStorefront(%q) = %q, want %q", link, got, want)
		}
	}
}

// TestAlbumURLIsItsOwnMode keeps the three link shapes apart.
func TestAlbumURLIsItsOwnMode(t *testing.T) {
	album := "https://music.apple.com/us/album/drama/1562865138"
	song := "https://music.apple.com/us/album/drama/1562865138?i=1562865145"
	playlist := "https://music.apple.com/us/playlist/name/pl.abc123"

	if !isAppleMusicAlbumURL(album) || isAppleMusicSongURL(album) {
		t.Error("a bare album link is the album mode")
	}

	if isAppleMusicAlbumURL(song) || !isAppleMusicSongURL(song) {
		t.Error("a ?i= link is the single-song mode")
	}

	if isAppleMusicAlbumURL(playlist) || !isAppleMusicPlaylistURL(playlist) {
		t.Error("a playlist link is the playlist mode")
	}
}

// ---------------------------------------------------------------
// Filters
// ---------------------------------------------------------------

func TestParseSearchQuery(t *testing.T) {
	cases := []struct {
		raw    string
		terms  string
		artist string
		album  string
	}{
		{raw: "tanhaeeia /artist shad", terms: "tanhaeeia", artist: "shad"},
		{
			raw:    "tanhaeeia /artist shadmehr aghili",
			terms:  "tanhaeeia",
			artist: "shadmehr aghili",
		},
		{
			raw:    "je suis fan /artist alice et moi",
			terms:  "je suis fan",
			artist: "alice et moi",
		},
		{raw: "/artist shadmehr", artist: "shadmehr"},
		{raw: "shallow /album a star is born", terms: "shallow", album: "a star is born"},
		{
			raw:    "shallow /ARTIST gaga /album born",
			terms:  "shallow",
			artist: "gaga",
			album:  "born",
		},
		{raw: "--artist gaga", artist: "gaga"},
		{raw: "just a title", terms: "just a title"},
		{raw: "   ", terms: ""},
	}

	for _, c := range cases {
		got := parseSearchQuery(c.raw)

		if got.Terms != c.terms || got.Artist != c.artist || got.Album != c.album {
			t.Errorf(
				"parseSearchQuery(%q) = %+v, want terms=%q artist=%q album=%q",
				c.raw, got, c.terms, c.artist, c.album,
			)
		}
	}
}

// TestMatchesFilter: "shad" is meant to reach Shadmehr, and a word
// starting with it is the reading that does that without also dragging
// in every Farshad.
func TestMatchesFilter(t *testing.T) {
	if !matchesFilter("Shadmehr Aghili", "shad") {
		t.Error("shad should match Shadmehr Aghili")
	}

	if matchesFilter("Farshad", "shad") {
		t.Error("shad should not match Farshad on the precise pass")
	}

	if !matchesFilterLoosely("Farshad", "shad") {
		t.Error("shad should match Farshad on the loose pass")
	}

	// Punctuation and doubled letters must not break a match: the two
	// spellings of one name have to land together.
	if !matchesFilter("Mohammad-Reza Shajarian", "mohammadreza") {
		t.Error("punctuation should not block a filter")
	}

	if !matchesFilter("Shadmehr Aghili", "aghili") {
		t.Error("a filter should match any word of the name")
	}
}

func TestFilterPrefersPreciseMatches(t *testing.T) {
	results := []SearchResult{
		{Kind: kindTrack, Name: "A", Artist: "Farshad"},
		{Kind: kindTrack, Name: "B", Artist: "Shadmehr Aghili"},
	}

	query := searchQuery{Artist: "shad"}

	kept := query.apply(results)

	if len(kept) != 1 || kept[0].Artist != "Shadmehr Aghili" {
		t.Fatalf("precise pass kept %+v, want only Shadmehr", kept)
	}

	// With no precise match left, the loose pass is what saves the
	// search from coming back empty.
	loose := query.apply(results[:1])

	if len(loose) != 1 || loose[0].Artist != "Farshad" {
		t.Errorf("loose pass kept %+v, want Farshad", loose)
	}
}

func TestTitleMatches(t *testing.T) {
	// The reported case: what was typed reduces to the start of what
	// the catalogue calls it.
	if !titleMatches("Tanhaeiam", "tanhaeeia") {
		t.Error("tanhaeeia should match Tanhaeiam")
	}

	if !titleMatches("Tanhaeiam", "tanhaeeiam") {
		t.Error("tanhaeeiam should match Tanhaeiam")
	}

	if titleMatches("Shallow", "tanhaeeia") {
		t.Error("unrelated titles should not match")
	}

	// No title asked for: browsing an artist keeps everything.
	if !titleMatches("Anything", "") {
		t.Error("an empty query should keep every title")
	}
}

// TestSearchFiltersResultsByArtist covers the plain case: the search
// finds things, and the filter narrows them here, because sending the
// artist to the catalogue as another word finds nothing at all.
func TestSearchFiltersResultsByArtist(t *testing.T) {
	catalogue := &fakeCatalogue{
		songs: map[string]string{"je suis fan": searchPayload},
	}

	outcome, err := catalogue.search(t).Search(
		context.Background(),
		searchQuery{Terms: "je suis fan", Artist: "alice"},
	)
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	if len(outcome.Results) != 1 {
		t.Fatalf("got %d results, want only the Alice et Moi one", len(outcome.Results))
	}

	if outcome.Results[0].Artist != "Alice et Moi" {
		t.Errorf("kept %q", outcome.Results[0].Artist)
	}
}

const artistPayload = `{
  "resultCount": 2,
  "results": [
    {"wrapperType":"artist","artistId":1,"artistName":"Shad"},
    {"wrapperType":"artist","artistId":272085442,"artistName":"Shadmehr Aghili"}
  ]
}`

const artistCataloguePayload = `{
  "resultCount": 3,
  "results": [
    {"wrapperType":"artist","artistId":272085442,"artistName":"Shadmehr Aghili"},
    {
      "wrapperType": "track",
      "kind": "song",
      "trackName": "Tanhaeiam",
      "artistName": "Shadmehr Aghili",
      "collectionName": "Tajrobeh Kon",
      "trackViewUrl": "https://music.apple.com/us/album/tajrobeh-kon/1?i=42&uo=4",
      "trackTimeMillis": 244000
    },
    {
      "wrapperType": "track",
      "kind": "song",
      "trackName": "Taghdir",
      "artistName": "Shadmehr Aghili",
      "collectionName": "Taghdir - Single",
      "trackViewUrl": "https://music.apple.com/us/album/taghdir/2?i=43&uo=4",
      "trackTimeMillis": 284000
    }
  ]
}`

// TestSearchFallsBackToTheArtistCatalogue is the second half of the
// reported problem: no spelling of the title reaches the song through
// search, but the artist's own catalogue has it.
func TestSearchFallsBackToTheArtistCatalogue(t *testing.T) {
	catalogue := &fakeCatalogue{
		artists: map[string]string{"shadmehr": artistPayload},
		lookup: map[string]string{
			"1":         `{"resultCount":0,"results":[]}`,
			"272085442": artistCataloguePayload,
		},
	}

	outcome, err := catalogue.search(t).Search(
		context.Background(),
		searchQuery{Terms: "tanhaeeia", Artist: "shadmehr"},
	)
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	if len(outcome.Results) != 1 {
		t.Fatalf("got %d results, want the one matching title", len(outcome.Results))
	}

	if outcome.Results[0].Name != "Tanhaeiam" {
		t.Errorf("found %q, want Tanhaeiam", outcome.Results[0].Name)
	}

	// The listing has to say where these came from: they are not what
	// was searched for.
	if outcome.Artist != "Shadmehr Aghili" {
		t.Errorf("outcome.Artist = %q, want Shadmehr Aghili", outcome.Artist)
	}

	if !isAppleMusicSongURL(outcome.Results[0].Link) {
		t.Errorf("catalogue result is not downloadable: %s", outcome.Results[0].Link)
	}
}

// TestArtistBrowseListsEverything: no title, just an artist.
func TestArtistBrowseListsEverything(t *testing.T) {
	catalogue := &fakeCatalogue{
		artists: map[string]string{"shadmehr": artistPayload},
		lookup: map[string]string{
			"1":         `{"resultCount":0,"results":[]}`,
			"272085442": artistCataloguePayload,
		},
	}

	outcome, err := catalogue.search(t).Search(
		context.Background(),
		searchQuery{Artist: "shadmehr"},
	)
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	if len(outcome.Results) != 2 {
		t.Fatalf("got %d songs, want the artist's 2", len(outcome.Results))
	}

	// The album row of the lookup is not a song and must not be listed.
	for _, result := range outcome.Results {
		if result.IsAlbum() {
			t.Errorf("an album leaked into the song list: %+v", result)
		}
	}
}

// TestArtistFilterWithNoSuchArtist gives up rather than inventing one.
func TestArtistFilterWithNoSuchArtist(t *testing.T) {
	catalogue := &fakeCatalogue{}

	outcome, err := catalogue.search(t).Search(
		context.Background(),
		searchQuery{Terms: "tanhaeeia", Artist: "nobodyatall"},
	)
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	if len(outcome.Results) != 0 || outcome.Artist != "" {
		t.Errorf("expected nothing, got %+v", outcome)
	}
}
