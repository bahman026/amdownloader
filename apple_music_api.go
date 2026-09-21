package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Apple Music embeds only the first page of a playlist's tracks in the
// page HTML: 300 items, regardless of how long the playlist actually
// is. Everything past that is fetched by their web player from the
// catalog API, and the page tells us where to continue in
// data[0].data.nextIntent.
//
// Without following that pointer a 636 track playlist silently becomes a
// 300 track one, which looks exactly like the downloader stopping early.

// appleMusicAPIBase is a var so tests can point it at a local server.
var appleMusicAPIBase = "https://amp-api.music.apple.com"

// appleMusicPageLimit is how many items Apple embeds in the HTML.
const appleMusicPageLimit = 300

// maxPlaylistPages bounds pagination so a malformed next pointer cannot
// loop forever. At 100 items per page this allows 10,000 tracks.
const maxPlaylistPages = 100

// paginationDelay paces the catalog requests. A var so tests can zero it.
var paginationDelay = 100 * time.Millisecond

var (
	tokenOnce  sync.Once
	cachedTok  string
	cachedErr  error
	tokenRegex = regexp.MustCompile(
		`eyJ[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{20,}`,
	)
	bundleRegex = regexp.MustCompile(
		`src="(/assets/index~[^"]+\.js)"`,
	)
)

// appleMusicAPIResponse is one page of the catalog tracks endpoint.
type appleMusicAPIResponse struct {
	Next string `json:"next"`

	Data []struct {
		Attributes struct {
			Name             string `json:"name"`
			ArtistName       string `json:"artistName"`
			AlbumName        string `json:"albumName"`
			DurationInMillis int64  `json:"durationInMillis"`
			URL              string `json:"url"`

			Artwork struct {
				URL string `json:"url"`
			} `json:"artwork"`
		} `json:"attributes"`
	} `json:"data"`
}

// appleMusicToken returns the web player's public API token, fetching it
// from the player bundle the page itself references. It is read once per
// process.
func appleMusicToken(
	client *http.Client,
	pageHTML string,
) (string, error) {

	tokenOnce.Do(func() {
		match := bundleRegex.FindStringSubmatch(pageHTML)

		if len(match) < 2 {
			cachedErr = fmt.Errorf(
				"could not locate the Apple Music player bundle",
			)

			return
		}

		bundleURL := "https://music.apple.com" + match[1]

		req, err := http.NewRequest(http.MethodGet, bundleURL, nil)
		if err != nil {
			cachedErr = err

			return
		}

		req.Header.Set("User-Agent", browserUserAgent)

		resp, err := client.Do(req)
		if err != nil {
			cachedErr = fmt.Errorf("fetch player bundle: %w", err)

			return
		}

		defer drainAndClose(resp)

		if resp.StatusCode != http.StatusOK {
			cachedErr = fmt.Errorf(
				"player bundle returned HTTP %d",
				resp.StatusCode,
			)

			return
		}

		body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		if err != nil {
			cachedErr = fmt.Errorf("read player bundle: %w", err)

			return
		}

		token := tokenRegex.Find(body)

		if token == nil {
			cachedErr = fmt.Errorf(
				"no API token found in the player bundle",
			)

			return
		}

		cachedTok = string(token)
	})

	return cachedTok, cachedErr
}

// fetchRemainingPlaylistTracks follows the pagination pointer from the
// page until the playlist is exhausted.
//
// A failure here is reported but not fatal: the caller keeps the tracks
// that were embedded in the HTML rather than losing the whole playlist.
func fetchRemainingPlaylistTracks(
	client *http.Client,
	pageHTML string,
	nextURL string,
) ([]Track, error) {

	nextURL = strings.TrimSpace(nextURL)

	if nextURL == "" {
		return nil, nil
	}

	token, err := appleMusicToken(client, pageHTML)
	if err != nil {
		return nil, err
	}

	return fetchPlaylistTracksFrom(client, token, nextURL)
}

// fetchPlaylistTracksFrom walks the pagination chain starting at
// nextURL. Split out from the token lookup so it can be tested on its
// own.
func fetchPlaylistTracksFrom(
	client *http.Client,
	token string,
	nextURL string,
) ([]Track, error) {

	var tracks []Track

	for page := 0; nextURL != "" && page < maxPlaylistPages; page++ {

		req, err := http.NewRequest(
			http.MethodGet,
			appleMusicAPIBase+nextURL,
			nil,
		)
		if err != nil {
			return tracks, err
		}

		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Origin", "https://music.apple.com")
		req.Header.Set("Referer", "https://music.apple.com/")
		req.Header.Set("User-Agent", browserUserAgent)

		resp, err := client.Do(req)
		if err != nil {
			return tracks, fmt.Errorf(
				"playlist page request failed: %w",
				err,
			)
		}

		body, readErr := io.ReadAll(
			io.LimitReader(resp.Body, 16<<20),
		)

		drainAndClose(resp)

		if readErr != nil {
			return tracks, fmt.Errorf(
				"read playlist page: %w",
				readErr,
			)
		}

		if resp.StatusCode != http.StatusOK {
			return tracks, fmt.Errorf(
				"playlist page returned HTTP %d: %s",
				resp.StatusCode,
				truncate(strings.TrimSpace(string(body)), 200),
			)
		}

		var decoded appleMusicAPIResponse

		if err := json.Unmarshal(body, &decoded); err != nil {
			return tracks, fmt.Errorf(
				"decode playlist page: %w",
				err,
			)
		}

		for _, item := range decoded.Data {
			attributes := item.Attributes

			name := strings.TrimSpace(attributes.Name)
			artist := strings.TrimSpace(attributes.ArtistName)
			link := strings.TrimSpace(attributes.URL)

			// The resolver needs all three; anything else is not
			// something we could download.
			if name == "" || artist == "" || link == "" {
				continue
			}

			tracks = append(tracks, Track{
				Name:   name,
				Artist: artist,
				Album:  strings.TrimSpace(attributes.AlbumName),
				Link:   link,

				Duration: formatAppleMusicDuration(
					float64(attributes.DurationInMillis),
				),

				Thumb: normalizeArtworkURL(
					attributes.Artwork.URL,
				),
			})
		}

		nextURL = strings.TrimSpace(decoded.Next)

		// Be a considerate client: these pages are cheap, but there
		// is no reason to burst them.
		if nextURL != "" && paginationDelay > 0 {
			time.Sleep(paginationDelay)
		}
	}

	return tracks, nil
}
