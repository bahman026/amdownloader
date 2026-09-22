package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// iTunesSearchURL is Apple's public catalogue search.
//
// It is used instead of scraping music.apple.com because it needs no
// key, no session and no HTML parsing, and because the field this whole
// program is built around -- trackViewUrl -- is exactly the
// /album/<slug>/<id>?i=<track id> link the resolver already knows how to
// take. A search result is therefore the same thing as a pasted link.
const iTunesSearchURL = "https://itunes.apple.com/search"

// iTunesLookupURL returns one album with its tracks. Each track comes
// back with the same trackViewUrl a search gives, so an album is just a
// list of ordinary single-track downloads.
const iTunesLookupURL = "https://itunes.apple.com/lookup"

const searchTimeout = 20 * time.Second

// maxSearchVariants bounds the near-spellings tried when a query finds
// nothing. Apple rate-limits this endpoint per address, so a search that
// found nothing must not turn into a burst of requests.
const maxSearchVariants = 3

// Filter markers. A query is words to search for, and optionally a
// filter applied to what comes back.
//
// They exist because the catalogue will not do this itself: every word
// sent to it has to match the same record, so "tanhaeia shadmehr"
// returns nothing at all even though "tanhaeia" and "shadmehr" each
// return plenty. Naming the artist separately means it narrows the
// results here instead of narrowing the search to nothing there.
const (
	artistMarker = "/artist"
	albumMarker  = "/album"
)

// searchFilterLimit is what a filtered search asks the catalogue for.
//
// The filter runs here, on what came back, so a filtered search needs a
// wide net to sieve: the twenty rows a plain search shows would leave
// almost nothing behind.
const searchFilterLimit = 200

// searchQuery is a query and its filters.
type searchQuery struct {
	Terms  string
	Artist string
	Album  string
}

// parseSearchQuery splits the words to search for from the filters.
func parseSearchQuery(raw string) searchQuery {
	var query searchQuery

	target := &query.Terms

	for _, word := range strings.Fields(raw) {
		switch strings.ToLower(word) {

		case artistMarker, "-artist", "--artist", "/by":
			target = &query.Artist

			continue

		case albumMarker, "-album", "--album":
			target = &query.Album

			continue
		}

		if *target == "" {
			*target = word

			continue
		}

		*target += " " + word
	}

	query.Terms = strings.TrimSpace(query.Terms)
	query.Artist = strings.TrimSpace(query.Artist)
	query.Album = strings.TrimSpace(query.Album)

	return query
}

func (q searchQuery) filtered() bool {
	return q.Artist != "" || q.Album != ""
}

// Describe renders the query for a message.
func (q searchQuery) Describe() string {
	parts := make([]string, 0, 3)

	if q.Terms != "" {
		parts = append(parts, fmt.Sprintf("%q", q.Terms))
	}

	if q.Artist != "" {
		label := "artist matching %q"

		// "for artist matching X" on its own; "for Y by artist
		// matching X" when there is a title as well.
		if q.Terms != "" {
			label = "by " + label
		}

		parts = append(parts, fmt.Sprintf(label, q.Artist))
	}

	if q.Album != "" {
		parts = append(parts, fmt.Sprintf("on album matching %q", q.Album))
	}

	if len(parts) == 0 {
		return `""`
	}

	return strings.Join(parts, " ")
}

// apply keeps the results the filters allow.
//
// Two passes: a word of the name starting with the filter, which is
// what "shad" for Shadmehr means, and failing that the filter anywhere
// in the name, which is what "shad" for Farshad means. The precise
// reading is preferred, and the loose one saves the search from coming
// back empty.
func (q searchQuery) apply(results []SearchResult) []SearchResult {
	if !q.filtered() {
		return results
	}

	if kept := q.keep(results, matchesFilter); len(kept) > 0 {
		return kept
	}

	return q.keep(results, matchesFilterLoosely)
}

func (q searchQuery) keep(
	results []SearchResult,
	match func(name, filter string) bool,
) []SearchResult {

	kept := make([]SearchResult, 0, len(results))

	for _, result := range results {
		if q.Artist != "" && !match(result.Artist, q.Artist) {
			continue
		}

		if q.Album != "" {
			// For an album row the album is its own name.
			subject := result.Album

			if result.IsAlbum() {
				subject = result.Name
			}

			if !match(subject, q.Album) {
				continue
			}
		}

		kept = append(kept, result)
	}

	return kept
}

// matchesFilter decides whether a name satisfies a filter word.
//
// A word of the name starting with the filter is what is wanted:
// "shad" should find "Shadmehr Aghili", and preferring that over a
// plain substring is what keeps it from also dragging in "Farshad".
// Substring matching is the fallback, used only when nothing matched
// the better way, because a filter typed from memory is often a
// fragment from the middle.
func matchesFilter(name, filter string) bool {
	want := looseKey(filter)

	if want == "" {
		return true
	}

	for _, word := range strings.Fields(name) {
		if strings.HasPrefix(looseKey(word), want) {
			return true
		}
	}

	return strings.HasPrefix(looseKey(name), want)
}

// matchesFilterLoosely is matchesFilter's second pass: anywhere in the
// name, not just at the start of a word.
func matchesFilterLoosely(name, filter string) bool {
	want := looseKey(filter)

	if want == "" {
		return true
	}

	return strings.Contains(looseKey(name), want)
}

// looseKey reduces a name to comparable letters: punctuation and case
// dropped, runs of one letter collapsed, so two romanisations of the
// same name land on the same key.
func looseKey(value string) string {
	return collapseRepeats(canonicalKey(value))
}

// titleMatches compares a song title against what was typed, allowing
// either to be a shortened form of the other: "tanhaeeia" reduces to
// "tanhaeia", which is the start of "tanhaeiam".
func titleMatches(title, terms string) bool {
	if strings.TrimSpace(terms) == "" {
		return true
	}

	want := looseKey(terms)
	have := looseKey(title)

	if want == "" || have == "" {
		return false
	}

	return strings.HasPrefix(have, want) ||
		strings.HasPrefix(want, have) ||
		strings.Contains(have, want)
}

// Result kinds. Apple's public catalogue API indexes songs and albums;
// it has no playlist entity, so a playlist can only enter this program
// as a pasted link.
const (
	kindTrack = "track"
	kindAlbum = "album"
)

// SearchResult is one catalogue match, already in the shape the download
// pipeline wants.
type SearchResult struct {
	// Kind is kindTrack or kindAlbum. An album is not downloadable as
	// it stands: its tracks are looked up when it is chosen.
	Kind string

	Name     string
	Artist   string
	Album    string
	Duration string
	Link     string
	Thumb    string

	Year     string
	Explicit bool

	// AlbumID and TrackCount carry an album result: the id is how its
	// tracks are looked up, the count is what the listing shows.
	AlbumID    int64
	TrackCount int
}

// Track converts a result into a pipeline track at the given position.
//
// The position matters: it becomes the "NN - " prefix of the filename,
// which is what keeps two picks from the same search from colliding.
func (r SearchResult) Track(index int) Track {
	album := strings.TrimSpace(r.Album)

	if album == "" {
		album = "Unknown Album"
	}

	return Track{
		Index:    index,
		Link:     r.Link,
		Name:     r.Name,
		Artist:   r.Artist,
		Duration: r.Duration,
		Thumb:    r.Thumb,
		Album:    album,
	}
}

// IsAlbum reports whether choosing this result means downloading a whole
// album rather than one track.
func (r SearchResult) IsAlbum() bool {
	return r.Kind == kindAlbum
}

// Detail is the fourth column of the listing: the album a track is on,
// or the size and year of an album.
func (r SearchResult) Detail() string {
	if !r.IsAlbum() {
		return r.Album
	}

	parts := make([]string, 0, 2)

	if r.TrackCount > 0 {
		parts = append(parts, fmt.Sprintf("%d tracks", r.TrackCount))
	}

	if r.Year != "" {
		parts = append(parts, r.Year)
	}

	return strings.Join(parts, ", ")
}

// SearchOutcome is what a search produced, and what it took to get it.
type SearchOutcome struct {
	Results []SearchResult

	// Query is the spelling that produced Results, which is not the
	// query typed if a fallback was what found them.
	Query string

	// Tried lists the fallbacks attempted, in order. Empty unless the
	// query as typed found nothing.
	Tried []string

	// Artist is set when the results came from an artist's catalogue
	// rather than from a search, which happens when a /artist filter
	// was given and searching found nothing.
	Artist string
}

// SongSearch queries the catalogue.
type SongSearch struct {
	Client *http.Client

	// Endpoint and LookupEndpoint override the production URLs. Empty
	// means iTunesSearchURL and iTunesLookupURL.
	Endpoint       string
	LookupEndpoint string

	// Country is the storefront to search. Results, and their links,
	// differ between storefronts.
	Country string

	Limit int
}

func (s *SongSearch) endpoint() string {
	if strings.TrimSpace(s.Endpoint) != "" {
		return s.Endpoint
	}

	return iTunesSearchURL
}

func (s *SongSearch) lookupEndpoint() string {
	if strings.TrimSpace(s.LookupEndpoint) != "" {
		return s.LookupEndpoint
	}

	return iTunesLookupURL
}

func (s *SongSearch) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}

	return http.DefaultClient
}

func (s *SongSearch) limit() int {
	if s.Limit <= 0 {
		return defaultSearchLimit
	}

	return s.Limit
}

// albumLimit is deliberately smaller than the song limit: albums are
// there so an album name finds its album, not to fill the screen.
func (s *SongSearch) albumLimit() int {
	quarter := s.limit() / 4

	if quarter < 3 {
		return 3
	}

	return quarter
}

func (s *SongSearch) country() string {
	country := strings.ToLower(strings.TrimSpace(s.Country))

	if len(country) != 2 {
		return defaultSearchCountry
	}

	return country
}

// itunesResponse is the part of the payload this program uses.
type itunesResponse struct {
	ResultCount int           `json:"resultCount"`
	Results     []itunesTrack `json:"results"`
}

type itunesArtist struct {
	ArtistID   int64  `json:"artistId"`
	ArtistName string `json:"artistName"`
}

type itunesTrack struct {
	WrapperType string `json:"wrapperType"`
	Kind        string `json:"kind"`

	TrackName         string  `json:"trackName"`
	ArtistName        string  `json:"artistName"`
	CollectionName    string  `json:"collectionName"`
	TrackViewURL      string  `json:"trackViewUrl"`
	CollectionViewURL string  `json:"collectionViewUrl"`
	ArtworkURL100     string  `json:"artworkUrl100"`
	TrackTimeMillis   float64 `json:"trackTimeMillis"`
	ReleaseDate       string  `json:"releaseDate"`
	TrackExplicitness string  `json:"trackExplicitness"`

	CollectionID int64 `json:"collectionId"`
	TrackCount   int   `json:"trackCount"`
	TrackNumber  int   `json:"trackNumber"`
	DiscNumber   int   `json:"discNumber"`
}

// Songs returns track matches for query.
func (s *SongSearch) Songs(
	ctx context.Context,
	query string,
) ([]SearchResult, error) {

	return s.songs(ctx, query, s.limit())
}

func (s *SongSearch) songs(
	ctx context.Context,
	query string,
	limit int,
) ([]SearchResult, error) {

	query = strings.TrimSpace(query)

	if query == "" {
		return nil, fmt.Errorf("nothing to search for")
	}

	params := url.Values{}

	params.Set("term", query)
	params.Set("entity", "song")
	params.Set("media", "music")
	params.Set("limit", strconv.Itoa(limit))
	params.Set("country", s.country())

	items, err := s.fetch(ctx, s.endpoint(), params)
	if err != nil {
		return nil, err
	}

	results := make([]SearchResult, 0, len(items))

	for _, item := range items {
		result, ok := songResult(item)

		if !ok {
			continue
		}

		results = append(results, result)
	}

	return results, nil
}

// Albums returns album matches for query.
//
// An album cannot be downloaded as it stands; AlbumTracks turns the
// chosen one into the list of tracks the pipeline runs.
func (s *SongSearch) Albums(
	ctx context.Context,
	query string,
) ([]SearchResult, error) {

	return s.albums(ctx, query, s.albumLimit())
}

func (s *SongSearch) albums(
	ctx context.Context,
	query string,
	limit int,
) ([]SearchResult, error) {

	query = strings.TrimSpace(query)

	if query == "" {
		return nil, fmt.Errorf("nothing to search for")
	}

	params := url.Values{}

	params.Set("term", query)
	params.Set("entity", "album")
	params.Set("media", "music")
	params.Set("limit", strconv.Itoa(limit))
	params.Set("country", s.country())

	items, err := s.fetch(ctx, s.endpoint(), params)
	if err != nil {
		return nil, err
	}

	results := make([]SearchResult, 0, len(items))

	for _, item := range items {
		if item.CollectionID == 0 ||
			strings.TrimSpace(item.CollectionName) == "" {

			continue
		}

		results = append(results, SearchResult{
			Kind:       kindAlbum,
			Name:       strings.TrimSpace(item.CollectionName),
			Artist:     strings.TrimSpace(item.ArtistName),
			Link:       cleanTrackViewURL(item.CollectionViewURL),
			Thumb:      searchArtworkURL(item.ArtworkURL100),
			Year:       releaseYear(item.ReleaseDate),
			AlbumID:    item.CollectionID,
			TrackCount: item.TrackCount,
		})
	}

	return results, nil
}

// lookupSongs fetches one collection or artist together with its songs.
func (s *SongSearch) lookupSongs(
	ctx context.Context,
	id int64,
) ([]itunesTrack, error) {

	if id <= 0 {
		return nil, fmt.Errorf("no id to look up")
	}

	params := url.Values{}

	params.Set("id", strconv.FormatInt(id, 10))
	params.Set("entity", "song")
	params.Set("limit", "200")
	params.Set("country", s.country())

	return s.fetch(ctx, s.lookupEndpoint(), params)
}

// artistIDs finds artists whose name matches what was typed.
//
// The catalogue ranks an exact name first and buries near ones, so the
// answer is taken as a list and narrowed here rather than trusting the
// first row.
func (s *SongSearch) artistIDs(
	ctx context.Context,
	name string,
	want int,
) ([]itunesArtist, error) {

	name = strings.TrimSpace(name)

	if name == "" {
		return nil, nil
	}

	params := url.Values{}

	params.Set("term", name)
	params.Set("entity", "musicArtist")
	params.Set("media", "music")
	params.Set("limit", strconv.Itoa(searchFilterLimit))
	params.Set("country", s.country())

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		s.endpoint()+"?"+params.Encode(),
		nil,
	)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", browserUserAgent)

	resp, err := s.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("artist search failed: %w", err)
	}

	defer resp.Body.Close()

	body, err := readAndDrain(resp, 8<<20)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf(
			"artist search failed with HTTP %d",
			resp.StatusCode,
		)
	}

	var payload struct {
		Results []itunesArtist `json:"results"`
	}

	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}

	var strict, loose []itunesArtist

	for _, artist := range payload.Results {
		if artist.ArtistID == 0 ||
			strings.TrimSpace(artist.ArtistName) == "" {

			continue
		}

		switch {

		case matchesFilter(artist.ArtistName, name):
			strict = append(strict, artist)

		case matchesFilterLoosely(artist.ArtistName, name):
			loose = append(loose, artist)
		}
	}

	found := append(strict, loose...)

	if len(found) > want {
		found = found[:want]
	}

	return found, nil
}

// byArtist lists an artist's own catalogue and keeps what the query
// asked for.
//
// This is what makes a filter worth having when the search itself comes
// up empty: the catalogue cannot find "Tanhaeiam" from "tanhaeeia", but
// it will hand over everything Shadmehr Aghili recorded, and the title
// is recognisable in that list.
func (s *SongSearch) byArtist(
	ctx context.Context,
	query searchQuery,
) ([]SearchResult, string, error) {

	artists, err := s.artistIDs(ctx, query.Artist, 3)

	if err != nil {
		return nil, "", err
	}

	if len(artists) == 0 {
		return nil, "", nil
	}

	seen := make(map[string]bool)
	seenName := make(map[string]bool)

	var results []SearchResult

	var named []string

	for _, artist := range artists {
		items, err := s.lookupSongs(ctx, artist.ArtistID)

		if err != nil {
			return nil, "", err
		}

		matched := 0

		for _, item := range items {
			result, ok := songResult(item)

			if !ok {
				continue
			}

			if !titleMatches(result.Name, query.Terms) {
				continue
			}

			if query.Album != "" &&
				!matchesFilter(result.Album, query.Album) {

				continue
			}

			if seen[result.Link] {
				continue
			}

			seen[result.Link] = true
			matched++

			results = append(results, result)
		}

		// The catalogue files one artist under several ids, so the
		// same name comes back more than once.
		if matched > 0 && !seenName[artist.ArtistName] {
			seenName[artist.ArtistName] = true

			named = append(named, artist.ArtistName)
		}
	}

	if len(results) > s.limit() {
		results = results[:s.limit()]
	}

	return results, strings.Join(named, ", "), nil
}

// AlbumTracks looks up every track on an album, in running order.
//
// The lookup hands back the album itself followed by its tracks, each
// with the same trackViewUrl a search gives, so nothing downstream has
// to know an album was involved.
func (s *SongSearch) AlbumTracks(
	ctx context.Context,
	albumID int64,
) ([]Track, error) {

	if albumID <= 0 {
		return nil, fmt.Errorf("no album id")
	}

	items, err := s.lookupSongs(ctx, albumID)
	if err != nil {
		return nil, err
	}

	type ordered struct {
		disc   int
		number int
		result SearchResult
	}

	var found []ordered

	for _, item := range items {
		result, ok := songResult(item)

		if !ok {
			continue
		}

		found = append(found, ordered{
			disc:   item.DiscNumber,
			number: item.TrackNumber,
			result: result,
		})
	}

	if len(found) == 0 {
		return nil, fmt.Errorf(
			"the catalogue lists no downloadable tracks for this album",
		)
	}

	// Running order, not the order the payload happened to arrive in:
	// the position becomes the "NN - " prefix on disk.
	sort.SliceStable(found, func(i, j int) bool {
		if found[i].disc != found[j].disc {
			return found[i].disc < found[j].disc
		}

		return found[i].number < found[j].number
	})

	tracks := make([]Track, 0, len(found))

	for i, item := range found {
		tracks = append(tracks, item.result.Track(i))
	}

	return tracks, nil
}

// songResult converts one payload entry into a track result, reporting
// whether it is something that can actually be downloaded.
func songResult(item itunesTrack) (SearchResult, bool) {
	// The endpoint mixes in other kinds when a term matches them.
	// Anything without a track link cannot be downloaded, so it has no
	// business in a list of things to pick from.
	if item.Kind != "" && item.Kind != "song" {
		return SearchResult{}, false
	}

	if item.WrapperType != "" && item.WrapperType != "track" {
		return SearchResult{}, false
	}

	link := cleanTrackViewURL(item.TrackViewURL)

	if link == "" || strings.TrimSpace(item.TrackName) == "" {
		return SearchResult{}, false
	}

	return SearchResult{
		Kind:     kindTrack,
		Name:     strings.TrimSpace(item.TrackName),
		Artist:   strings.TrimSpace(item.ArtistName),
		Album:    strings.TrimSpace(item.CollectionName),
		Duration: formatAppleMusicDuration(item.TrackTimeMillis),
		Link:     link,
		Thumb:    searchArtworkURL(item.ArtworkURL100),
		Year:     releaseYear(item.ReleaseDate),
		Explicit: strings.EqualFold(item.TrackExplicitness, "explicit"),
	}, true
}

// fetch performs one catalogue request.
func (s *SongSearch) fetch(
	ctx context.Context,
	endpoint string,
	params url.Values,
) ([]itunesTrack, error) {

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		endpoint+"?"+params.Encode(),
		nil,
	)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", browserUserAgent)

	resp, err := s.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("search request failed: %w", err)
	}

	defer resp.Body.Close()

	body, err := readAndDrain(resp, 8<<20)
	if err != nil {
		return nil, fmt.Errorf("read search response: %w", err)
	}

	switch {

	case resp.StatusCode == http.StatusForbidden,
		resp.StatusCode == http.StatusTooManyRequests:

		// Apple throttles this endpoint per address. Saying so is more
		// use than the status line, because waiting is the fix.
		return nil, fmt.Errorf(
			"search refused with HTTP %d; Apple rate-limits this "+
				"endpoint, so wait a minute and try again",
			resp.StatusCode,
		)

	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return nil, fmt.Errorf(
			"search failed with HTTP %d: %s",
			resp.StatusCode,
			strings.TrimSpace(truncate(string(body), 200)),
		)
	}

	var payload itunesResponse

	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("search returned unreadable JSON: %w", err)
	}

	return payload.Results, nil
}

// Search runs the query, and if it finds nothing, tries near spellings
// of it.
//
// The catalogue matches loosely but not evenly: "tanhaeeiam" finds
// "Tanhaeiam", while "tanhaeeia" finds nothing at all, even though
// "tanhaeia" finds five. A name written in a script the catalogue stores
// romanised has no single correct spelling, so the difference between a
// hit and nothing is often one doubled vowel. Rather than hand that back
// as "no results", the near spellings are tried and the one that worked
// is named in the output.
//
// Only a search that found nothing falls back, and it stops at the first
// spelling that finds something.
func (s *SongSearch) Search(
	ctx context.Context,
	query searchQuery,
) (SearchOutcome, error) {

	outcome := SearchOutcome{
		Query: query.Terms,
	}

	if query.Terms != "" {
		results, err := s.find(ctx, query.Terms, query)

		if err != nil {
			return outcome, err
		}

		if len(results) > 0 {
			outcome.Results = results

			return outcome, nil
		}

		for _, variant := range searchVariants(query.Terms) {
			outcome.Tried = append(outcome.Tried, variant)

			results, err := s.find(ctx, variant, query)

			if err != nil {
				return outcome, err
			}

			if len(results) > 0 {
				outcome.Results = results
				outcome.Query = variant

				return outcome, nil
			}
		}
	}

	// Nothing the catalogue's own search can reach. An artist filter
	// gives one more way in: ask for that artist, and read their
	// catalogue here.
	if query.Artist == "" {
		return outcome, nil
	}

	results, artist, err := s.byArtist(ctx, query)

	if err != nil {
		return outcome, err
	}

	outcome.Results = results
	outcome.Artist = artist

	return outcome, nil
}

// find runs one spelling against both catalogues.
//
// Tracks come first because a query is usually a song title; the albums
// follow so that typing an album name finds the album itself rather than
// whichever of its tracks happens to rank highest.
func (s *SongSearch) find(
	ctx context.Context,
	terms string,
	query searchQuery,
) ([]SearchResult, error) {

	trackLimit := s.limit()
	albumLimit := s.albumLimit()

	// A filter runs over what came back, so it needs a wide net to
	// sieve rather than the handful a listing shows.
	if query.filtered() {
		trackLimit = searchFilterLimit
		albumLimit = searchFilterLimit
	}

	tracks, err := s.songs(ctx, terms, trackLimit)
	if err != nil {
		return nil, err
	}

	albums, err := s.albums(ctx, terms, albumLimit)
	if err != nil {
		return nil, err
	}

	tracks = capResults(query.apply(tracks), s.limit())
	albums = capResults(query.apply(albums), s.albumLimit())

	return append(tracks, albums...), nil
}

func capResults(results []SearchResult, limit int) []SearchResult {
	if limit > 0 && len(results) > limit {
		return results[:limit]
	}

	return results
}

// searchVariants returns near spellings to try, closest first.
//
// Each one is a guess at how the catalogue might have written the same
// name, not a general typo search: the catalogue is what decides whether
// a guess was right, and the first one it answers wins.
func searchVariants(query string) []string {
	query = strings.Join(strings.Fields(query), " ")

	if query == "" {
		return nil
	}

	words := strings.Fields(query)

	seen := map[string]bool{strings.ToLower(query): true}

	var variants []string

	add := func(candidate string) {
		candidate = strings.Join(strings.Fields(candidate), " ")

		if candidate == "" {
			return
		}

		key := strings.ToLower(candidate)

		if seen[key] {
			return
		}

		seen[key] = true

		variants = append(variants, candidate)
	}

	// A doubled letter is the usual difference between two romanisations
	// of one name: "tanhaeeia" against the catalogue's "tanhaeia".
	add(collapseRepeats(query))

	// Then the longest word with its ending cut back, for a suffix the
	// catalogue spells differently. The longest word is used because it
	// is the one carrying the title; "the", "feat" and the like are not
	// what made the search fail.
	longest := longestWord(words)

	for _, cut := range []int{1, 2} {
		if shorter, ok := shortenWord(longest, cut); ok {
			add(strings.Replace(query, longest, shorter, 1))
		}
	}

	// Last, that word on its own: the rest of the query may be an
	// artist the catalogue files under another name entirely.
	if len(words) > 1 {
		add(longest)
	}

	if len(variants) > maxSearchVariants {
		variants = variants[:maxSearchVariants]
	}

	return variants
}

// collapseRepeats squeezes runs of the same letter down to one, so
// "tanhaeeia" becomes "tanhaeia".
//
// It mangles ordinary words -- "Bee Gees" becomes "Be Ges" -- which is
// why it is only ever a fallback for a query that already found nothing.
func collapseRepeats(value string) string {
	var out []rune

	var previous rune

	for _, r := range value {
		if unicode.IsLetter(r) &&
			len(out) > 0 &&
			unicode.ToLower(r) == unicode.ToLower(previous) {

			continue
		}

		out = append(out, r)
		previous = r
	}

	return string(out)
}

// longestWord returns the word carrying the most of the query.
func longestWord(words []string) string {
	longest := ""

	for _, word := range words {
		if len([]rune(word)) > len([]rune(longest)) {
			longest = word
		}
	}

	return longest
}

// shortenWord drops cut characters from the end, refusing to cut a word
// down to a stub that would match half the catalogue.
func shortenWord(word string, cut int) (string, bool) {
	runes := []rune(word)

	if len(runes)-cut < 4 {
		return "", false
	}

	return string(runes[:len(runes)-cut]), true
}

// appleMusicAlbumID extracts the album id from an /album/ URL.
//
// It is the last path segment: /<store>/album/<slug>/<album id>. A ?i=
// on the end names one track of that album, which is a different mode
// and is checked for before this is ever reached.
func appleMusicAlbumID(link string) int64 {
	parsed, err := url.Parse(strings.TrimSpace(link))
	if err != nil {
		return 0
	}

	if !strings.Contains(parsed.Path, "/album/") {
		return 0
	}

	base := parsed.Path[strings.LastIndex(parsed.Path, "/")+1:]

	if !isDigits(base) {
		return 0
	}

	id, err := strconv.ParseInt(base, 10, 64)
	if err != nil {
		return 0
	}

	return id
}

// appleMusicStorefront reads the two-letter store out of a link.
//
// A link copied from the Turkish store names ids that the US catalogue
// may not carry, so a lookup for it has to ask the store the link came
// from rather than whatever search_country happens to say.
func appleMusicStorefront(link string) string {
	parsed, err := url.Parse(strings.TrimSpace(link))
	if err != nil {
		return ""
	}

	segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")

	if len(segments) == 0 {
		return ""
	}

	store := strings.ToLower(segments[0])

	if len(store) != 2 {
		return ""
	}

	for _, r := range store {
		if r < 'a' || r > 'z' {
			return ""
		}
	}

	return store
}

// newSongSearch builds a searcher from the current settings.
func newSongSearch() *SongSearch {
	return &SongSearch{
		Client: &http.Client{
			Transport: newTransport(),
			Timeout:   searchTimeout,
		},

		Country: appSettings.SearchCountry,
		Limit:   appSettings.SearchLimit,
	}
}

// cleanTrackViewURL drops the affiliate parameter Apple appends.
//
// It changes nothing functionally -- the resolver only reads ?i= -- but
// it keeps album_details, the archive and anything printed to the
// terminal free of a tracking token.
func cleanTrackViewURL(raw string) string {
	raw = strings.TrimSpace(raw)

	if raw == "" {
		return ""
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}

	query := parsed.Query()

	query.Del("uo")
	query.Del("app")
	query.Del("at")

	parsed.RawQuery = query.Encode()

	return parsed.String()
}

// searchArtworkURL upgrades the 100px thumbnail to the 600px artwork the
// rest of the program embeds. The path segment is the size, so asking
// for a bigger one is a string substitution.
func searchArtworkURL(raw string) string {
	raw = strings.TrimSpace(raw)

	if raw == "" {
		return ""
	}

	for _, size := range []string{
		"100x100bb",
		"60x60bb",
		"30x30bb",
	} {
		if strings.Contains(raw, size) {
			return strings.Replace(raw, size, "600x600bb", 1)
		}
	}

	return raw
}

func releaseYear(value string) string {
	value = strings.TrimSpace(value)

	if len(value) < 4 {
		return ""
	}

	year := value[:4]

	if !isDigits(year) {
		return ""
	}

	return year
}

// parseSelection turns what was typed at the prompt into result indices.
//
// Accepts a number, several numbers, ranges, and "all". Indices are
// 1-based on the way in, because that is what the list shows, and
// 0-based on the way out, because that is what indexes the slice.
func parseSelection(input string, count int) ([]int, error) {
	input = strings.TrimSpace(strings.ToLower(input))

	if input == "" || input == "q" || input == "quit" {
		return nil, nil
	}

	if input == "a" || input == "all" || input == "*" {
		all := make([]int, 0, count)

		for i := 0; i < count; i++ {
			all = append(all, i)
		}

		return all, nil
	}

	fields := strings.FieldsFunc(input, func(r rune) bool {
		return r == ' ' || r == ',' || r == '\t'
	})

	seen := make(map[int]bool, len(fields))

	var chosen []int

	add := func(n int) error {
		if n < 1 || n > count {
			return fmt.Errorf(
				"there is no result %d (pick between 1 and %d)",
				n, count,
			)
		}

		if seen[n] {
			return nil
		}

		seen[n] = true
		chosen = append(chosen, n-1)

		return nil
	}

	for _, field := range fields {
		low, high, isRange := strings.Cut(field, "-")

		if !isRange {
			number, err := strconv.Atoi(field)
			if err != nil {
				return nil, fmt.Errorf(
					"%q is not a number, a range like 1-3, or \"all\"",
					field,
				)
			}

			if err := add(number); err != nil {
				return nil, err
			}

			continue
		}

		first, err := strconv.Atoi(strings.TrimSpace(low))
		if err != nil {
			return nil, fmt.Errorf("%q is not a range like 1-3", field)
		}

		last, err := strconv.Atoi(strings.TrimSpace(high))
		if err != nil {
			return nil, fmt.Errorf("%q is not a range like 1-3", field)
		}

		if first > last {
			first, last = last, first
		}

		for n := first; n <= last; n++ {
			if err := add(n); err != nil {
				return nil, err
			}
		}
	}

	return chosen, nil
}

// printSearchResults lists what was found.
//
// Tracks already in the archive are marked, so a choice is made knowing
// which of several releases of the same song is the one already held.
func printSearchResults(
	out io.Writer,
	results []SearchResult,
	archive *Archive,
) {

	// Rows are trimmed on the right: a column of trailing spaces is
	// invisible until someone copies a line out of the terminal.
	fmt.Fprintln(out, strings.TrimRight(fmt.Sprintf(
		"  %-3s %-6s %-30s %-21s %-25s %-6s",
		"#", "Type", "Title", "Artist", "Album / info", "Time",
	), " "))

	for i, result := range results {
		// Albums come after the tracks, so a blank line between the two
		// groups is what stops a search for an album name from burying
		// the album under twenty of its own tracks.
		if i > 0 && result.IsAlbum() && !results[i-1].IsAlbum() {
			fmt.Fprintln(out)
		}

		title := result.Name

		if result.Explicit {
			title += " [E]"
		}

		marker := ""

		// An album is marked only when every one of its tracks is
		// already held, which is the only case where choosing it would
		// download nothing at all.
		if !result.IsAlbum() && archive.Has(result.Track(i)) {
			marker = "  already downloaded"
		}

		fmt.Fprintln(out, strings.TrimRight(fmt.Sprintf(
			"  %-3d %-6s %-30s %-21s %-25s %-6s%s",
			i+1,
			result.Kind,
			truncate(title, 30),
			truncate(result.Artist, 21),
			truncate(result.Detail(), 25),
			result.Duration,
			marker,
		), " "))
	}
}

// validateSelection rejects a pick the download side cannot honour.
//
// An album goes into a folder of its own and loose tracks go into the
// download directory flat, so one selection cannot be both. Rather than
// quietly downloading half of what was asked for, this says so and the
// prompt comes round again.
func validateSelection(results []SearchResult, chosen []int) error {
	albums := 0

	for _, index := range chosen {
		if results[index].IsAlbum() {
			albums++
		}
	}

	if albums == 0 {
		return nil
	}

	if albums == len(chosen) && albums == 1 {
		return nil
	}

	return fmt.Errorf(
		"an album has to be picked on its own, not alongside other rows",
	)
}

// promptSelection asks which results to download.
//
// A parse error is reported and the prompt repeats, because the list is
// still on screen and retyping one number is cheaper than running the
// search again. End of input, an empty line or "q" cancels.
func promptSelection(
	in io.Reader,
	out io.Writer,
	count int,
) []int {

	reader := bufio.NewReader(in)

	for {
		fmt.Fprint(out, "> ")

		line, err := reader.ReadString('\n')

		if err != nil && strings.TrimSpace(line) == "" {
			fmt.Fprintln(out)

			return nil
		}

		chosen, parseErr := parseSelection(line, count)

		if parseErr != nil {
			fmt.Fprintf(out, "  %v\n", parseErr)

			if err != nil {
				return nil
			}

			continue
		}

		return chosen
	}
}

// runSearch is the search mode: a query in, a chosen track downloaded.
func runSearch(raw string) {
	query := parseSearchQuery(raw)

	if query.Terms == "" && query.Artist == "" && query.Album == "" {
		printSearchUsage()

		return
	}

	searcher := newSongSearch()

	fmt.Printf(
		"Searching Apple Music (%s) for %s...\n\n",
		strings.ToUpper(searcher.country()),
		query.Describe(),
	)

	ctx, cancel := context.WithTimeout(
		context.Background(),
		searchTimeout,
	)
	defer cancel()

	outcome, err := searcher.Search(ctx, query)

	if err != nil {
		fmt.Printf("ERROR: %v\n", err)

		return
	}

	results := outcome.Results

	if len(results) == 0 {
		fmt.Printf("Nothing found for %s.\n", query.Describe())

		if len(outcome.Tried) > 0 {
			fmt.Printf(
				"Also tried: %s\n",
				strings.Join(quoteAll(outcome.Tried), ", "),
			)
		}

		if query.Artist != "" {
			fmt.Printf(
				"No artist the catalogue lists as %q has a song like that "+
					"either.\n",
				query.Artist,
			)
		}

		fmt.Println()

		if query.Artist == "" {
			fmt.Println("Name the artist to search their catalogue directly:")
			fmt.Println("  media-cli <title> /artist <artist name>")
		} else {
			fmt.Println("Try more of the artist's name, or a different")
			fmt.Println(
				"storefront:  media-cli settings set search_country de",
			)
		}

		return
	}

	// What is about to be printed is not literally what was asked for,
	// and saying so is the difference between a helpful fallback and a
	// confusing one.
	switch {

	// Nothing was asked for but the artist, so nothing failed: this is
	// a browse, not a fallback.
	case outcome.Artist != "" && query.Terms == "":
		fmt.Printf("Songs by %s:\n\n", outcome.Artist)

	case outcome.Artist != "":
		fmt.Printf(
			"No match for %s; showing songs by %s.\n\n",
			query.Describe(),
			outcome.Artist,
		)

	case outcome.Query != query.Terms:
		fmt.Printf(
			"No match for %q; showing results for %q.\n\n",
			query.Terms,
			outcome.Query,
		)
	}

	// Read-only here: the pipeline loads its own copy when a download
	// actually starts. A failure only costs the "already downloaded"
	// marker, so it is noted and ignored.
	var archive *Archive

	if appSettings.Archive {
		if loaded, err := LoadArchive(appSettings.ArchiveFile); err == nil {
			archive = loaded
		}
	}

	printSearchResults(os.Stdout, results, archive)

	fmt.Println()

	if !isTerminal(os.Stdin) {
		fmt.Println("Nothing was selected: input is not a terminal.")
		fmt.Println("Run this again from a terminal, or pass a link directly.")

		return
	}

	fmt.Printf(
		"Pick a number (1-%d), several like 1 3 5, a range like 1-3,\n",
		len(results),
	)
	fmt.Println("\"a\" for all, or Enter to cancel.")

	var chosen []int

	for {
		chosen = promptSelection(os.Stdin, os.Stdout, len(results))

		if len(chosen) == 0 {
			fmt.Println("Nothing selected.")

			return
		}

		if err := validateSelection(results, chosen); err != nil {
			fmt.Printf("  %v\n", err)

			continue
		}

		break
	}

	fmt.Println()

	if results[chosen[0]].IsAlbum() {
		downloadAlbum(ctx, searcher, results[chosen[0]])

		return
	}

	tracks := make([]Track, 0, len(chosen))

	for _, index := range chosen {
		tracks = append(tracks, results[index].Track(len(tracks)))
	}

	for _, track := range tracks {
		fmt.Printf(
			"Selected: %s - %s (%s)\n",
			track.Name,
			track.Artist,
			track.Album,
		)
	}

	fmt.Println()

	downloadTracks(tracks, appSettings.OutputDir, query.Terms, "")
}

// downloadAlbum turns a chosen album into its tracks and runs them.
//
// They land in a folder named after the album, the same as a playlist
// does, because an album is a set that belongs together -- unlike the
// loose picks from a search, which go into the download directory flat.
func downloadAlbum(
	ctx context.Context,
	searcher *SongSearch,
	album SearchResult,
) {

	fmt.Printf(
		"Album: %s - %s\n",
		album.Name,
		album.Artist,
	)

	tracks, err := searcher.AlbumTracks(ctx, album.AlbumID)

	if err != nil {
		fmt.Printf("ERROR: %v\n", err)

		return
	}

	fmt.Printf("Tracks: %d\n\n", len(tracks))

	albumName := strings.TrimSpace(album.Name)

	if albumName == "" {
		albumName = strings.TrimSpace(tracks[0].Album)
	}

	if albumName == "" {
		albumName = "Unknown Album"
	}

	downloadTracks(
		tracks,
		filepath.Join(appSettings.OutputDir, safeFilename(albumName)),
		albumName,
		album.Thumb,
	)
}

// downloadTracks writes the manifest and runs the pipeline.
func downloadTracks(
	tracks []Track,
	downloadDir string,
	title string,
	thumb string,
) {

	if thumb == "" {
		thumb = tracks[0].Thumb
	}

	// Written for the same reason single-song mode writes it: a bare
	// `media-cli` can then replay this without searching again.
	if err := SaveAlbumDetails(
		&AppleMusicPlaylist{
			Name:   title,
			Album:  tracks[0].Album,
			Thumb:  thumb,
			Date:   fmt.Sprintf("%d song(s)", len(tracks)),
			Tracks: tracks,
		},
		"album_details",
	); err != nil {
		fmt.Printf("ERROR: failed to save album_details: %v\n", err)

		return
	}

	fmt.Println("album_details saved to ./album_details")
	fmt.Println()

	if err := os.MkdirAll(downloadDir, 0o755); err != nil {
		fmt.Printf(
			"ERROR: failed to create download directory: %v\n",
			err,
		)

		return
	}

	processTracks(tracks, downloadDir)
}

// quoteAll renders a list of queries for a one-line message.
func quoteAll(values []string) []string {
	quoted := make([]string, 0, len(values))

	for _, value := range values {
		quoted = append(quoted, fmt.Sprintf("%q", value))
	}

	return quoted
}

func printSearchUsage() {
	fmt.Println("Usage:")
	fmt.Println("  media-cli <title>                     search and pick")
	fmt.Println("  media-cli <title> /artist <name>      narrow by artist")
	fmt.Println("  media-cli /artist <name>              browse an artist")
	fmt.Println("  media-cli search <words>              the same, explicitly")
	fmt.Println()
	fmt.Println("Anything that is not a link and not a command is a search.")
	fmt.Println("`search` is only needed for a query that would otherwise")
	fmt.Println("read as a command, such as `media-cli search settings`.")
	fmt.Println()
	fmt.Println("Storefront and result count: search_country, search_limit")
}
