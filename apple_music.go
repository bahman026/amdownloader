package main

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

type AppleMusicPlaylist struct {
	Name   string
	Album  string
	Thumb  string
	Date   string
	Tracks []Track
}

type AppleMusicValue struct {
	Raw json.RawMessage
}

func (v *AppleMusicValue) UnmarshalJSON(
	data []byte,
) error {
	v.Raw = append(
		v.Raw[:0],
		data...,
	)

	return nil
}

func (v AppleMusicValue) String() string {
	raw := strings.TrimSpace(
		string(v.Raw),
	)

	if raw == "" || raw == "null" {
		return ""
	}

	var str string

	if err := json.Unmarshal(
		v.Raw,
		&str,
	); err == nil {
		return str
	}

	var number float64

	if err := json.Unmarshal(
		v.Raw,
		&number,
	); err == nil {
		return fmt.Sprintf(
			"%g",
			number,
		)
	}

	return ""
}

func (v AppleMusicValue) Number() float64 {
	raw := strings.TrimSpace(
		string(v.Raw),
	)

	if raw == "" || raw == "null" {
		return 0
	}

	var number float64

	if err := json.Unmarshal(
		v.Raw,
		&number,
	); err == nil {
		return number
	}

	return 0
}

type appleMusicPage struct {
	Data []struct {
		Data struct {
			SEOData struct {
				SchemaContent struct {
					Name  string `json:"name"`
					Image string `json:"image"`
				} `json:"schemaContent"`
			} `json:"seoData"`

			Sections []struct {
				Items []json.RawMessage `json:"items"`
			} `json:"sections"`
		} `json:"data"`
	} `json:"data"`
}

type appleMusicItem struct {
	Title    string          `json:"title"`
	Artist   string          `json:"artistName"`
	Duration AppleMusicValue `json:"duration"`

	ContentDescriptor *struct {
		Identifiers struct {
			StoreAdamID string `json:"storeAdamID"`
			URL         string `json:"url"`
		} `json:"identifiers"`

		Kind string `json:"kind"`
		URL  string `json:"url"`
	} `json:"contentDescriptor"`

	Artwork *struct {
		Dictionary struct {
			URL string `json:"url"`
		} `json:"dictionary"`
	} `json:"artwork"`

	SubtitleLinks []struct {
		Title string `json:"title"`
	} `json:"subtitleLinks"`

	TertiaryLinks []struct {
		Title string `json:"title"`
	} `json:"tertiaryLinks"`

	TrackNumber AppleMusicValue `json:"trackNumber"`
}

func FetchAppleMusicPlaylist(
	playlistURL string,
) (*AppleMusicPlaylist, error) {

	parsedURL, err := url.Parse(
		playlistURL,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"invalid playlist URL: %w",
			err,
		)
	}

	if parsedURL.Host != "music.apple.com" {
		return nil, fmt.Errorf(
			"URL is not an Apple Music URL",
		)
	}

	fmt.Printf(
		"Requesting: %s\n",
		playlistURL,
	)

	req, err := http.NewRequest(
		http.MethodGet,
		playlistURL,
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"create request: %w",
			err,
		)
	}

	req.Header.Set(
		"User-Agent",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) "+
			"AppleWebKit/537.36 (KHTML, like Gecko) "+
			"Chrome/153.0.0.0 Safari/537.36",
	)

	req.Header.Set(
		"Accept",
		"text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
	)

	req.Header.Set(
		"Accept-Language",
		"en-US,en;q=0.9",
	)

	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf(
			"request Apple Music playlist: %w",
			err,
		)
	}

	defer resp.Body.Close()

	fmt.Printf(
		"Apple Music HTTP status: %s\n",
		resp.Status,
	)

	if resp.StatusCode < 200 ||
		resp.StatusCode >= 300 {

		return nil, fmt.Errorf(
			"Apple Music returned HTTP %d",
			resp.StatusCode,
		)
	}

	body, err := io.ReadAll(
		io.LimitReader(
			resp.Body,
			30*1024*1024,
		),
	)
	if err != nil {
		return nil, fmt.Errorf(
			"read Apple Music page: %w",
			err,
		)
	}

	htmlBody := string(body)

	page, err := extractAppleMusicPage(
		htmlBody,
	)
	if err != nil {
		return nil, err
	}

	playlist := &AppleMusicPlaylist{}

	// ----------------------------------------
	// Playlist metadata
	// ----------------------------------------

	for _, pageData := range page.Data {

		if playlist.Name == "" {
			playlist.Name =
				pageData.Data.SEOData.
					SchemaContent.Name
		}

		if playlist.Thumb == "" {
			playlist.Thumb =
				pageData.Data.SEOData.
					SchemaContent.Image
		}
	}

	if playlist.Name == "" {
		playlist.Name = extractMetaContent(
			htmlBody,
			`<meta[^>]+property=["']og:title["'][^>]+content=["']([^"']+)["']`,
		)
	}

	if playlist.Thumb == "" {
		playlist.Thumb = extractMetaContent(
			htmlBody,
			`<meta[^>]+property=["']og:image["'][^>]+content=["']([^"']+)["']`,
		)
	}

	playlist.Name = html.UnescapeString(
		playlist.Name,
	)

	playlist.Thumb = html.UnescapeString(
		playlist.Thumb,
	)

	playlist.Thumb = normalizeArtworkURL(
		playlist.Thumb,
	)

	// ----------------------------------------
	// Tracks
	// ----------------------------------------

	var parsedTracks []Track

	for _, pageData := range page.Data {

		for sectionIndex, section :=
			range pageData.Data.Sections {

			fmt.Printf(
				"Section %d: %d items\n",
				sectionIndex,
				len(section.Items),
			)

			for itemIndex, rawItem :=
				range section.Items {

				track, ok :=
					parseAppleMusicItem(
						rawItem,
					)

				if !ok {

					if sectionIndex == 1 {

						var debugItem struct {
							Title      string `json:"title"`
							ArtistName string `json:"artistName"`

							ContentDescriptor *struct {
								Kind string `json:"kind"`
								URL  string `json:"url"`

								Identifiers struct {
									URL         string `json:"url"`
									StoreAdamID string `json:"storeAdamID"`
								} `json:"identifiers"`
							} `json:"contentDescriptor"`
						}

						if err := json.Unmarshal(
							rawItem,
							&debugItem,
						); err == nil {

							kind := ""

							if debugItem.ContentDescriptor != nil {
								kind =
									debugItem.ContentDescriptor.Kind
							}

							fmt.Printf(
								"[DEBUG] REJECTED section=1 item=%d title=%q artist=%q kind=%q\n",
								itemIndex,
								debugItem.Title,
								debugItem.ArtistName,
								kind,
							)
						}
					}

					continue
				}

				if sectionIndex == 1 {

					fmt.Printf(
						"[DEBUG] Parsed section=1 item=%d: %s - %s\n",
						itemIndex,
						track.Name,
						track.Artist,
					)

					parsedTracks = append(
						parsedTracks,
						track,
					)
				}
			}
		}
	}

	fmt.Printf(
		"[DEBUG] Parsed tracks: %d\n",
		len(parsedTracks),
	)

	// ----------------------------------------
	// Do NOT deduplicate tracks.
	//
	// A playlist can intentionally contain
	// the same song more than once.
	// ----------------------------------------

	playlist.Tracks = parsedTracks

	if len(playlist.Tracks) == 0 {
		return nil, fmt.Errorf(
			"no tracks found in Apple Music page",
		)
	}

	for i := range playlist.Tracks {
		playlist.Tracks[i].Index = i
	}

	// ----------------------------------------
	// Playlist name is the download album/folder
	//
	// Do NOT use the first track's album here.
	// A playlist can contain tracks from many
	// different albums.
	// ----------------------------------------

	playlist.Album = strings.TrimSpace(
		playlist.Name,
	)

	if playlist.Album == "" {
		playlist.Album = "Unknown Album"
	}

	// ----------------------------------------
	// Playlist information
	// ----------------------------------------

	playlist.Date = fmt.Sprintf(
		"%d songs",
		len(playlist.Tracks),
	)

	return playlist, nil
}

func extractAppleMusicPage(
	htmlBody string,
) (*appleMusicPage, error) {

	re := regexp.MustCompile(
		`(?is)<script[^>]*id=["']serialized-server-data["'][^>]*>(.*?)</script>`,
	)

	match := re.FindStringSubmatch(
		htmlBody,
	)

	if len(match) < 2 {
		return nil, fmt.Errorf(
			"serialized-server-data was not found",
		)
	}

	raw := strings.TrimSpace(
		match[1],
	)

	if raw == "" {
		return nil, fmt.Errorf(
			"serialized-server-data is empty",
		)
	}

	var page appleMusicPage

	if err := json.Unmarshal(
		[]byte(raw),
		&page,
	); err != nil {

		return nil, fmt.Errorf(
			"decode serialized-server-data: %w",
			err,
		)
	}

	if len(page.Data) == 0 {
		return nil, fmt.Errorf(
			"serialized-server-data contains no data",
		)
	}

	return &page, nil
}

func parseAppleMusicItem(
	raw json.RawMessage,
) (Track, bool) {

	var item appleMusicItem

	if err := json.Unmarshal(
		raw,
		&item,
	); err != nil {
		return Track{}, false
	}

	if item.ContentDescriptor == nil {
		return Track{}, false
	}

	if item.ContentDescriptor.Kind != "" &&
		item.ContentDescriptor.Kind != "song" {

		return Track{}, false
	}

	name := strings.TrimSpace(
		item.Title,
	)

	if name == "" {
		return Track{}, false
	}

	artist := strings.TrimSpace(
		item.Artist,
	)

	if artist == "" &&
		len(item.SubtitleLinks) > 0 {

		artist =
			strings.TrimSpace(
				item.SubtitleLinks[0].Title,
			)
	}

	if artist == "" {
		return Track{}, false
	}

	album := ""

	if len(item.TertiaryLinks) > 0 {

		album =
			strings.TrimSpace(
				item.TertiaryLinks[0].Title,
			)
	}

	trackURL := strings.TrimSpace(
		item.ContentDescriptor.
			Identifiers.
			URL,
	)

	if trackURL == "" {

		trackURL =
			strings.TrimSpace(
				item.ContentDescriptor.URL,
			)
	}

	if trackURL == "" {
		return Track{}, false
	}

	duration := formatAppleMusicDuration(
		item.Duration.Number(),
	)

	thumb := ""

	if item.Artwork != nil {

		thumb =
			strings.TrimSpace(
				item.Artwork.Dictionary.URL,
			)
	}

	thumb =
		normalizeArtworkURL(
			thumb,
		)

	name = html.UnescapeString(
		name,
	)

	artist = html.UnescapeString(
		artist,
	)

	album = html.UnescapeString(
		album,
	)

	trackURL = html.UnescapeString(
		trackURL,
	)

	return Track{
		Name:     name,
		Artist:   artist,
		Album:    album,
		Link:     trackURL,
		Duration: duration,
		Thumb:    thumb,
	}, true
}

func formatAppleMusicDuration(
	milliseconds float64,
) string {

	if milliseconds <= 0 {
		return ""
	}

	totalSeconds := int64(
		milliseconds / 1000,
	)

	hours := totalSeconds / 3600

	minutes :=
		(totalSeconds % 3600) / 60

	seconds :=
		totalSeconds % 60

	if hours > 0 {

		return fmt.Sprintf(
			"%d:%02d:%02d",
			hours,
			minutes,
			seconds,
		)
	}

	return fmt.Sprintf(
		"%d:%02d",
		minutes,
		seconds,
	)
}

func normalizeArtworkURL(
	thumb string,
) string {

	thumb = strings.ReplaceAll(
		thumb,
		"{w}",
		"600",
	)

	thumb = strings.ReplaceAll(
		thumb,
		"{h}",
		"600",
	)

	thumb = strings.ReplaceAll(
		thumb,
		"{f}",
		"jpg",
	)

	thumb = strings.ReplaceAll(
		thumb,
		"{c}",
		"bb",
	)

	return thumb
}

func extractMetaContent(
	body string,
	pattern string,
) string {

	re := regexp.MustCompile(
		`(?is)` + pattern,
	)

	match := re.FindStringSubmatch(
		body,
	)

	if len(match) < 2 {
		return ""
	}

	return match[1]
}

func SaveAlbumDetails(
	playlist *AppleMusicPlaylist,
	filename string,
) error {

	if playlist == nil {
		return fmt.Errorf(
			"playlist is nil",
		)
	}

	if strings.TrimSpace(filename) == "" {
		return fmt.Errorf(
			"album_details filename is empty",
		)
	}

	file, err := os.Create(
		filename,
	)
	if err != nil {
		return fmt.Errorf(
			"create album_details: %w",
			err,
		)
	}

	defer file.Close()

	if _, err := fmt.Fprintln(
		file,
		"{",
	); err != nil {
		return err
	}

	if _, err := fmt.Fprintln(
		file,
		"album_details: {",
	); err != nil {
		return err
	}

	for _, track := range playlist.Tracks {

		if _, err := fmt.Fprintf(
			file,
			"%d: {\n",
			track.Index,
		); err != nil {
			return err
		}

		if _, err := fmt.Fprintf(
			file,
			"link: %q,\n",
			track.Link,
		); err != nil {
			return err
		}

		if _, err := fmt.Fprintf(
			file,
			"name: %q,\n",
			track.Name,
		); err != nil {
			return err
		}

		if _, err := fmt.Fprintf(
			file,
			"artist: %q,\n",
			track.Artist,
		); err != nil {
			return err
		}

		if _, err := fmt.Fprintf(
			file,
			"duration: %q,\n",
			track.Duration,
		); err != nil {
			return err
		}

		if _, err := fmt.Fprintf(
			file,
			"thumb: %q,\n",
			track.Thumb,
		); err != nil {
			return err
		}

		if _, err := fmt.Fprintf(
			file,
			"album: %q\n",
			track.Album,
		); err != nil {
			return err
		}

		if _, err := fmt.Fprintln(
			file,
			"},",
		); err != nil {
			return err
		}
	}

	if _, err := fmt.Fprintf(
		file,
		"album: %q\n",
		playlist.Album,
	); err != nil {
		return err
	}

	if _, err := fmt.Fprintln(
		file,
		"artist: \"\"",
	); err != nil {
		return err
	}

	if _, err := fmt.Fprintf(
		file,
		"thumb: %q\n",
		playlist.Thumb,
	); err != nil {
		return err
	}

	if _, err := fmt.Fprintf(
		file,
		"date: %q\n",
		playlist.Date,
	); err != nil {
		return err
	}

	if _, err := fmt.Fprintf(
		file,
		"count: %d\n",
		len(playlist.Tracks),
	); err != nil {
		return err
	}

	if _, err := fmt.Fprintln(
		file,
		"}",
	); err != nil {
		return err
	}

	if _, err := fmt.Fprintln(
		file,
		"}",
	); err != nil {
		return err
	}

	return nil
}

func PrintAppleMusicPlaylist(
	playlist *AppleMusicPlaylist,
) {

	fmt.Println()
	fmt.Println(
		"========================================",
	)

	fmt.Println(
		"Apple Music Playlist",
	)

	fmt.Println(
		"========================================",
	)

	fmt.Printf(
		"Playlist: %s\n",
		playlist.Name,
	)

	fmt.Printf(
		"Album:    %s\n",
		playlist.Album,
	)

	fmt.Printf(
		"Songs:    %d\n",
		len(playlist.Tracks),
	)

	fmt.Println()

	fmt.Println(
		"album_details: {",
	)

	for _, track := range playlist.Tracks {

		fmt.Printf(
			"%d: {\n",
			track.Index,
		)

		fmt.Printf(
			"link: %q,\n",
			track.Link,
		)

		fmt.Printf(
			"name: %q,\n",
			track.Name,
		)

		fmt.Printf(
			"artist: %q,\n",
			track.Artist,
		)

		fmt.Printf(
			"duration: %q,\n",
			track.Duration,
		)

		fmt.Printf(
			"thumb: %q,\n",
			track.Thumb,
		)

		fmt.Printf(
			"album: %q\n",
			track.Album,
		)

		fmt.Println("},")
	}

	fmt.Printf(
		"album: %q\n",
		playlist.Album,
	)

	fmt.Println(
		"artist: \"\"",
	)

	fmt.Printf(
		"thumb: %q\n",
		playlist.Thumb,
	)

	fmt.Printf(
		"date: %q\n",
		playlist.Date,
	)

	fmt.Printf(
		"count: %d\n",
		len(playlist.Tracks),
	)

	fmt.Println("}")
	fmt.Println("}")
}