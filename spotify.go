package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// spotifyEmbedURL is the player page behind every share link.
//
// Nothing here downloads from Spotify, and nothing here could: its audio
// is encrypted and its API needs credentials. What this page does give,
// to anyone, is what a link refers to -- the titles, the artists and the
// durations. That is enough to find the same recording in the Apple
// Music catalogue, which is what the rest of this program knows how to
// download. A Spotify link is therefore a way of saying which songs are
// wanted, not a source of them.
const spotifyEmbedURL = "https://open.spotify.com/embed"

const spotifyTimeout = 30 * time.Second

// matchDelay paces the catalogue lookups a playlist needs.
//
// Apple rate-limits search per address, and a fifty track playlist is
// fifty searches. Spacing them costs half a minute and is the difference
// between a playlist that resolves and one that is refused halfway.
const matchDelay = 600 * time.Millisecond

// SpotifyTrack is one song named by a Spotify link.
type SpotifyTrack struct {
	Name   string
	Artist string
	Millis int64
}

// SpotifyEntity is what a link points at.
type SpotifyEntity struct {
	Kind   string
	Name   string
	Tracks []SpotifyTrack
}

var spotifyNextData = regexp.MustCompile(
	`(?s)<script id="__NEXT_DATA__" type="application/json">(.*?)</script>`,
)

var spotifyLinkPattern = regexp.MustCompile(
	`(?i)(?:open\.spotify\.com/(?:intl-[a-z]{2}/)?|spotify:)(track|album|playlist)[:/]([A-Za-z0-9]+)`,
)

// isSpotifyURL reports whether the argument is a Spotify link.
func isSpotifyURL(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))

	return strings.Contains(value, "open.spotify.com") ||
		strings.HasPrefix(value, "spotify:")
}

// parseSpotifyURL pulls the kind and id out of a share link.
//
// It accepts the web link, the localised /intl-xx/ form the app copies,
// and the spotify:track:<id> URI.
func parseSpotifyURL(value string) (kind string, id string, ok bool) {
	match := spotifyLinkPattern.FindStringSubmatch(strings.TrimSpace(value))

	if match == nil {
		return "", "", false
	}

	return strings.ToLower(match[1]), match[2], true
}

// SpotifyReader reads what a link refers to.
type SpotifyReader struct {
	Client *http.Client

	// EmbedURL overrides the production page. Empty means the real one.
	EmbedURL string
}

func (r *SpotifyReader) embedURL() string {
	if strings.TrimSpace(r.EmbedURL) != "" {
		return r.EmbedURL
	}

	return spotifyEmbedURL
}

func (r *SpotifyReader) client() *http.Client {
	if r.Client != nil {
		return r.Client
	}

	return http.DefaultClient
}

// spotifyPayload is the part of the embedded state this program reads.
type spotifyPayload struct {
	Props struct {
		PageProps struct {
			State struct {
				Data struct {
					Entity spotifyEntity `json:"entity"`
				} `json:"data"`
			} `json:"state"`
		} `json:"pageProps"`
	} `json:"props"`
}

// spotifyEntity covers both shapes the page uses: the top level entity
// names its artists in a list, while a row of a track list carries the
// same thing as a subtitle.
type spotifyEntity struct {
	Type     string `json:"type"`
	Name     string `json:"name"`
	Title    string `json:"title"`
	Subtitle string `json:"subtitle"`
	Duration int64  `json:"duration"`

	Artists []struct {
		Name string `json:"name"`
	} `json:"artists"`

	TrackList []spotifyEntity `json:"trackList"`
}

func (e spotifyEntity) trackName() string {
	if name := strings.TrimSpace(e.Name); name != "" {
		return name
	}

	return strings.TrimSpace(e.Title)
}

func (e spotifyEntity) artist() string {
	names := make([]string, 0, len(e.Artists))

	for _, artist := range e.Artists {
		if name := strings.TrimSpace(artist.Name); name != "" {
			names = append(names, name)
		}
	}

	if len(names) > 0 {
		return strings.Join(names, ", ")
	}

	return strings.TrimSpace(e.Subtitle)
}

// Read fetches the link and returns what it names.
func (r *SpotifyReader) Read(
	ctx context.Context,
	link string,
) (SpotifyEntity, error) {

	kind, id, ok := parseSpotifyURL(link)

	if !ok {
		return SpotifyEntity{}, fmt.Errorf(
			"that is not a Spotify track, album or playlist link",
		)
	}

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		fmt.Sprintf("%s/%s/%s", r.embedURL(), kind, id),
		nil,
	)
	if err != nil {
		return SpotifyEntity{}, err
	}

	req.Header.Set("User-Agent", browserUserAgent)
	req.Header.Set("Accept", "text/html,*/*")

	resp, err := r.client().Do(req)
	if err != nil {
		return SpotifyEntity{}, fmt.Errorf("fetch Spotify page: %w", err)
	}

	defer resp.Body.Close()

	body, err := readAndDrain(resp, 8<<20)
	if err != nil {
		return SpotifyEntity{}, err
	}

	if resp.StatusCode == http.StatusNotFound {
		return SpotifyEntity{}, fmt.Errorf(
			"Spotify has no %s with that id, or it is private",
			kind,
		)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return SpotifyEntity{}, fmt.Errorf(
			"Spotify returned HTTP %d",
			resp.StatusCode,
		)
	}

	return parseSpotifyPage(string(body), kind)
}

// parseSpotifyPage reads the state the player page carries.
func parseSpotifyPage(html string, kind string) (SpotifyEntity, error) {
	match := spotifyNextData.FindStringSubmatch(html)

	if match == nil {
		return SpotifyEntity{}, fmt.Errorf(
			"could not read that page; Spotify may have changed it",
		)
	}

	var payload spotifyPayload

	if err := json.Unmarshal([]byte(match[1]), &payload); err != nil {
		return SpotifyEntity{}, fmt.Errorf(
			"could not read that page: %w",
			err,
		)
	}

	entity := payload.Props.PageProps.State.Data.Entity

	found := SpotifyEntity{
		Kind: strings.TrimSpace(entity.Type),
		Name: entity.trackName(),
	}

	if found.Kind == "" {
		found.Kind = kind
	}

	if len(entity.TrackList) > 0 {
		for _, item := range entity.TrackList {
			name := item.trackName()

			if name == "" {
				continue
			}

			found.Tracks = append(found.Tracks, SpotifyTrack{
				Name:   name,
				Artist: item.artist(),
				Millis: item.Duration,
			})
		}
	} else if found.Name != "" {
		found.Tracks = append(found.Tracks, SpotifyTrack{
			Name:   found.Name,
			Artist: entity.artist(),
			Millis: entity.Duration,
		})
	}

	if len(found.Tracks) == 0 {
		return found, fmt.Errorf(
			"that link names no tracks this program can read",
		)
	}

	return found, nil
}

// ---------------------------------------------------------------
// Matching a Spotify track to the Apple Music catalogue
// ---------------------------------------------------------------

// matchScore rates an Apple Music result as the same recording.
//
// Nothing here is certain: two services' catalogues disagree about
// punctuation, featured artists and which remaster is "the" track. So
// the three things that can be compared are scored and the best total
// wins, with a floor below which nothing is claimed as a match.
//
// Duration carries the most weight of the three, because a title and an
// artist will also match a cover, a live take or a sped-up edit, and
// those are the matches worth refusing.
func matchScore(candidate SearchResult, want SpotifyTrack) int {
	title := looseKey(candidate.Name)
	wanted := looseKey(want.Name)

	if title == "" || wanted == "" {
		return 0
	}

	score := 0

	switch {

	case title == wanted:
		score += 4

	case strings.HasPrefix(title, wanted), strings.HasPrefix(wanted, title):
		score += 3

	case strings.Contains(title, wanted), strings.Contains(wanted, title):
		score += 1

	default:
		return 0
	}

	switch {

	case want.Artist == "":
		// Nothing to check against; the title has to carry it.

	case matchesFilter(candidate.Artist, firstArtist(want.Artist)):
		score += 3

	case matchesFilterLoosely(candidate.Artist, firstArtist(want.Artist)):
		score += 1

	default:
		// A different artist means a different recording, whatever the
		// title says.
		return 0
	}

	if want.Millis > 0 && candidate.Millis > 0 {
		gap := want.Millis - candidate.Millis

		if gap < 0 {
			gap = -gap
		}

		switch {

		case gap <= 2000:
			score += 4

		case gap <= 5000:
			score += 2

		case gap > 20000:
			// Far too far apart to be the same recording.
			return 0
		}
	}

	return score
}

// minMatchScore is the floor for claiming two entries are the same
// recording: a title that matches outright and an artist that agrees,
// or a near title with the duration to back it up.
const minMatchScore = 6

func firstArtist(value string) string {
	for _, separator := range []string{",", "&", " feat", " ft"} {
		if index := strings.Index(
			strings.ToLower(value),
			separator,
		); index > 0 {
			value = value[:index]
		}
	}

	return strings.TrimSpace(value)
}

// MatchTrack finds the Apple Music entry for a Spotify track.
func (s *SongSearch) MatchTrack(
	ctx context.Context,
	want SpotifyTrack,
) ([]SearchResult, error) {

	terms := strings.TrimSpace(want.Name + " " + firstArtist(want.Artist))

	candidates, err := s.songs(ctx, terms, s.limit())

	if err != nil {
		return nil, err
	}

	// The two catalogues spell things differently often enough that the
	// combined query finds nothing; the title alone, narrowed by the
	// artist here, is the second way in.
	if len(candidates) == 0 {
		query := searchQuery{
			Terms:  want.Name,
			Artist: firstArtist(want.Artist),
		}

		candidates, err = s.find(ctx, want.Name, query)

		if err != nil {
			return nil, err
		}
	}

	type scored struct {
		result SearchResult
		score  int
	}

	var ranked []scored

	for _, candidate := range candidates {
		if candidate.IsAlbum() {
			continue
		}

		if score := matchScore(candidate, want); score >= minMatchScore {
			ranked = append(ranked, scored{candidate, score})
		}
	}

	// Stable by score, so equal matches keep the catalogue's own order,
	// which puts the original release above the compilations.
	for i := 1; i < len(ranked); i++ {
		for j := i; j > 0 && ranked[j].score > ranked[j-1].score; j-- {
			ranked[j], ranked[j-1] = ranked[j-1], ranked[j]
		}
	}

	results := make([]SearchResult, 0, len(ranked))

	for _, item := range ranked {
		results = append(results, item.result)
	}

	return results, nil
}

// ---------------------------------------------------------------
// The Spotify mode
// ---------------------------------------------------------------

// runSpotify takes a Spotify link and downloads the Apple Music
// equivalent of what it names.
func runSpotify(link string) {
	reader := &SpotifyReader{
		Client: &http.Client{
			Transport: newTransport(),
			Timeout:   spotifyTimeout,
		},
	}

	ctx, cancel := context.WithTimeout(
		context.Background(),
		spotifyTimeout,
	)
	defer cancel()

	entity, err := reader.Read(ctx, link)

	if err != nil {
		fmt.Printf("ERROR: %v\n", err)

		return
	}

	fmt.Printf(
		"Spotify %s: %s (%d track(s))\n",
		entity.Kind,
		entity.Name,
		len(entity.Tracks),
	)

	// Said plainly, because it is the thing a person would reasonably
	// have assumed otherwise.
	fmt.Println(
		"Spotify itself is not the source: each track is matched " +
			"against the",
	)
	fmt.Println("Apple Music catalogue and downloaded from there.")
	fmt.Println()

	searcher := newSongSearch()

	matchCtx, cancelMatch := context.WithCancel(context.Background())
	defer cancelMatch()

	results, missing := matchSpotifyTracks(
		matchCtx,
		searcher,
		entity.Tracks,
	)

	if len(missing) > 0 {
		fmt.Println()
		fmt.Printf("Not found on Apple Music (%d):\n", len(missing))

		for _, track := range missing {
			fmt.Printf("  %s - %s\n", track.Name, track.Artist)
		}
	}

	if len(results) == 0 {
		fmt.Println()
		fmt.Println("Nothing to download.")

		return
	}

	fmt.Println()

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

		return
	}

	fmt.Printf(
		"Pick a number (1-%d), several like 1 3 5, a range like 1-3,\n",
		len(results),
	)
	fmt.Println("\"a\" for all, or Enter to cancel.")

	chosen := promptSelection(os.Stdin, os.Stdout, len(results))

	if len(chosen) == 0 {
		fmt.Println("Nothing selected.")

		return
	}

	tracks := make([]Track, 0, len(chosen))

	for _, index := range chosen {
		tracks = append(tracks, results[index].Track(len(tracks)))
	}

	fmt.Println()

	// A single track goes into the download directory like any other
	// single; a named set keeps its name, the way a playlist does.
	downloadDir := appSettings.OutputDir
	title := entity.Name

	if entity.Kind != "track" && strings.TrimSpace(entity.Name) != "" {
		downloadDir = filepath.Join(
			appSettings.OutputDir,
			safeFilename(entity.Name),
		)
	}

	downloadTracks(tracks, downloadDir, title, "")
}

// matchSpotifyTracks resolves each Spotify track to its Apple Music
// counterpart, reporting progress as it goes.
//
// The lookups are sequential and spaced: Apple rate-limits search, and
// fifty parallel queries would be refused outright.
func matchSpotifyTracks(
	ctx context.Context,
	searcher *SongSearch,
	wanted []SpotifyTrack,
) ([]SearchResult, []SpotifyTrack) {

	var results []SearchResult

	var missing []SpotifyTrack

	seen := make(map[string]bool)

	for i, want := range wanted {
		if ctx.Err() != nil {
			fmt.Println("Interrupted.")

			break
		}

		if i > 0 {
			time.Sleep(matchDelay)
		}

		fmt.Printf(
			"[%3d/%d] %s - %s ... ",
			i+1,
			len(wanted),
			truncate(want.Name, 34),
			truncate(want.Artist, 24),
		)

		matches, err := searcher.MatchTrack(ctx, want)

		if err != nil {
			fmt.Printf("lookup failed (%v)\n", err)

			// A refusal is about the address, not this track, so
			// everything after it would fail the same way.
			if strings.Contains(err.Error(), "rate-limit") {
				fmt.Println()
				fmt.Println(
					"Stopping here; wait a minute and run it again.",
				)

				break
			}

			missing = append(missing, want)

			continue
		}

		if len(matches) == 0 {
			fmt.Println("not found")

			missing = append(missing, want)

			continue
		}

		best := matches[0]

		if seen[best.Link] {
			fmt.Println("already in this list")

			continue
		}

		seen[best.Link] = true

		fmt.Printf("%s - %s\n", best.Name, best.Artist)

		results = append(results, best)
	}

	return results, missing
}
