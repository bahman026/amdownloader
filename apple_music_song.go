package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type appleMusicSongItem struct {
	ArtistName string `json:"artistName"`

	Artwork struct {
		Dictionary struct {
			URL string `json:"url"`
		} `json:"dictionary"`
		URL string `json:"url"`
	} `json:"artwork"`

	ContentDescriptor struct {
		Identifiers struct {
			StoreAdamID string `json:"storeAdamID"`
			URL         string `json:"url"`
		} `json:"identifiers"`

		Kind string `json:"kind"`
		URL  string `json:"url"`
	} `json:"contentDescriptor"`

	Duration int64 `json:"duration"`

	SubtitleLinks []struct {
		Title string `json:"title"`
	} `json:"subtitleLinks"`

	TertiaryLinks []struct {
		Title string `json:"title"`
	} `json:"tertiaryLinks"`

	Title string `json:"title"`
}

func FetchAppleMusicSong(
	songURL string,
) (Track, error) {

	var track Track

	parsedURL, err := url.Parse(songURL)
	if err != nil {
		return track, fmt.Errorf(
			"invalid Apple Music URL: %w",
			err,
		)
	}

	trackID := strings.TrimSpace(
		parsedURL.Query().Get("i"),
	)

	if trackID == "" {
		return track, fmt.Errorf(
			"Apple Music song URL does not contain ?i=TRACK_ID",
		)
	}

	fmt.Printf(
		"Requesting: %s\n",
		songURL,
	)

	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	req, err := http.NewRequest(
		http.MethodGet,
		songURL,
		nil,
	)
	if err != nil {
		return track, fmt.Errorf(
			"create Apple Music request: %w",
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

	resp, err := client.Do(req)
	if err != nil {
		return track, fmt.Errorf(
			"Apple Music request failed: %w",
			err,
		)
	}
	defer resp.Body.Close()

	fmt.Printf(
		"Apple Music HTTP status: %s\n",
		resp.Status,
	)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(
			io.LimitReader(resp.Body, 4096),
		)

		return track, fmt.Errorf(
			"Apple Music HTTP %d: %s; body=%q",
			resp.StatusCode,
			resp.Status,
			string(body),
		)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return track, fmt.Errorf(
			"read Apple Music response: %w",
			err,
		)
	}

	// Apple Music embeds the page data as a large JSON object
	// inside the HTML. The Single Song page does not use the
	// same serialized-server-data structure as the playlist page.
	jsonData, err := extractAppleMusicPageJSON(body)
	if err != nil {
		return track, err
	}

	var root map[string]interface{}

	if err := json.Unmarshal(jsonData, &root); err != nil {
		return track, fmt.Errorf(
			"decode Apple Music page JSON: %w",
			err,
		)
	}

	item, found := findAppleMusicSong(
		root,
		trackID,
	)

	if !found {
		return track, fmt.Errorf(
			"could not find Apple Music song ID %s",
			trackID,
		)
	}

	artist := strings.TrimSpace(
		item.ArtistName,
	)

	if artist == "" &&
		len(item.SubtitleLinks) > 0 {

		artist = strings.TrimSpace(
			item.SubtitleLinks[0].Title,
		)
	}

	album := ""

	if len(item.TertiaryLinks) > 0 {
		album = strings.TrimSpace(
			item.TertiaryLinks[0].Title,
		)
	}

	// A single-song page carries no tertiaryLinks on the song itself;
	// the containing album is a sibling item on the page instead.
	// Without this the ID3 album tag was always "Unknown Album".
	if album == "" {
		album = findAppleMusicAlbumTitle(root)
	}

	thumb := item.Artwork.Dictionary.URL

	if thumb == "" {
		thumb = item.Artwork.URL
	}

	if thumb != "" {
		thumb = normalizeArtworkURL(thumb)
	}

	track = Track{
		Index:    0,
		Link:     item.ContentDescriptor.Identifiers.URL,
		Name:     cleanHTML(item.Title),
		Artist:   cleanHTML(artist),
		Duration: formatAppleMusicDuration(float64(item.Duration)),
		Thumb:    thumb,
		Album:    cleanHTML(album),
	}

	if track.Link == "" {
		track.Link = item.ContentDescriptor.URL
	}

	if track.Name == "" {
		return track, fmt.Errorf(
			"Apple Music returned an empty song title",
		)
	}

	if track.Artist == "" {
		return track, fmt.Errorf(
			"Apple Music returned an empty artist",
		)
	}

	if track.Link == "" {
		return track, fmt.Errorf(
			"Apple Music returned an empty song URL",
		)
	}

	if track.Album == "" {
		track.Album = "Unknown Album"
	}

	fmt.Println()

	fmt.Printf(
		"[DEBUG] Found song ID: %s\n",
		trackID,
	)

	fmt.Printf(
		"[DEBUG] Name: %s\n",
		track.Name,
	)

	fmt.Printf(
		"[DEBUG] Artist: %s\n",
		track.Artist,
	)

	fmt.Printf(
		"[DEBUG] Album: %s\n",
		track.Album,
	)

	return track, nil
}

func extractAppleMusicPageJSON(
	body []byte,
) ([]byte, error) {

	html := string(body)

	/*
		Apple's Single Song page contains a large JSON object
		inside the HTML.

		We locate the beginning using a known field that is
		present in the page data and then determine the JSON
		object boundaries by balancing braces.
	*/

	candidates := []string{
		`{"data":[`,
		`{"data":{`,
		`{"storefrontId":`,
		`{"meta":`,
	}

	start := -1

	for _, candidate := range candidates {

		position := strings.Index(
			html,
			candidate,
		)

		if position != -1 {
			start = position
			break
		}
	}

	if start == -1 {

		// Fallback: locate the canonical URL and walk backwards
		// to the beginning of the containing JSON object.
		marker := `"canonicalURL":`

		position := strings.Index(
			html,
			marker,
		)

		if position == -1 {
			return nil, fmt.Errorf(
				"Apple Music page JSON could not be located",
			)
		}

		start = findJSONStart(
			html,
			position,
		)
	}

	if start == -1 {
		return nil, fmt.Errorf(
			"could not determine Apple Music JSON start",
		)
	}

	end := findJSONEnd(
		html,
		start,
	)

	if end == -1 {
		return nil, fmt.Errorf(
			"could not determine Apple Music JSON end",
		)
	}

	jsonText := strings.TrimSpace(
		html[start:end],
	)

	if jsonText == "" {
		return nil, fmt.Errorf(
			"Apple Music page JSON is empty",
		)
	}

	// Verify that what we extracted is actually valid JSON.
	var test interface{}

	if err := json.Unmarshal(
		[]byte(jsonText),
		&test,
	); err != nil {
		return nil, fmt.Errorf(
			"Apple Music embedded JSON is invalid: %w",
			err,
		)
	}

	return []byte(jsonText), nil
}

func findJSONStart(
	value string,
	position int,
) int {

	if position > len(value) {
		position = len(value)
	}

	depth := 0
	inString := false
	escaped := false

	for i := position; i >= 0; i-- {

		char := value[i]

		if char == '"' && !escaped {
			inString = !inString
		}

		if inString {
			if char == '\\' && !escaped {
				escaped = true
			} else {
				escaped = false
			}

			continue
		}

		if char == '}' {
			depth++
		}

		if char == '{' {

			if depth == 0 {
				return i
			}

			depth--
		}
	}

	return -1
}

func findJSONEnd(
	value string,
	start int,
) int {

	if start < 0 || start >= len(value) {
		return -1
	}

	depth := 0
	inString := false
	escaped := false

	for i := start; i < len(value); i++ {

		char := value[i]

		if inString {

			if escaped {
				escaped = false
				continue
			}

			if char == '\\' {
				escaped = true
				continue
			}

			if char == '"' {
				inString = false
			}

			continue
		}

		if char == '"' {
			inString = true
			continue
		}

		switch char {

		case '{':
			depth++

		case '}':
			depth--

			if depth == 0 {
				return i + 1
			}
		}
	}

	return -1
}

func findAppleMusicSong(
	value interface{},
	trackID string,
) (appleMusicSongItem, bool) {

	switch current := value.(type) {

	case map[string]interface{}:

		// Check if this object itself represents the song.
		if song, ok := convertSongItem(current); ok {

			if song.ContentDescriptor.Identifiers.StoreAdamID ==
				trackID {

				return song, true
			}
		}

		for _, child := range current {

			result, found := findAppleMusicSong(
				child,
				trackID,
			)

			if found {
				return result, true
			}
		}

	case []interface{}:

		for _, child := range current {

			result, found := findAppleMusicSong(
				child,
				trackID,
			)

			if found {
				return result, true
			}
		}
	}

	return appleMusicSongItem{}, false
}

// findAppleMusicAlbumTitle returns the title of the album item on the
// page, which is how a single-song page names the album the track
// belongs to.
func findAppleMusicAlbumTitle(value interface{}) string {

	switch current := value.(type) {

	case map[string]interface{}:

		if getNestedString(
			current,
			"contentDescriptor",
			"kind",
		) == "album" {

			if title := getString(current, "title"); title != "" {
				return title
			}
		}

		for _, child := range current {
			if title := findAppleMusicAlbumTitle(child); title != "" {
				return title
			}
		}

	case []interface{}:

		for _, child := range current {
			if title := findAppleMusicAlbumTitle(child); title != "" {
				return title
			}
		}
	}

	return ""
}

func convertSongItem(
	value map[string]interface{},
) (appleMusicSongItem, bool) {

	rawID := getNestedString(
		value,
		"contentDescriptor",
		"identifiers",
		"storeAdamID",
	)

	if rawID == "" {
		return appleMusicSongItem{}, false
	}

	rawKind := getNestedString(
		value,
		"contentDescriptor",
		"kind",
	)

	if rawKind != "" && rawKind != "song" {
		return appleMusicSongItem{}, false
	}

	rawTitle, ok := value["title"].(string)

	if !ok || strings.TrimSpace(rawTitle) == "" {
		return appleMusicSongItem{}, false
	}

	item := appleMusicSongItem{
		Title: rawTitle,
		ArtistName: getString(
			value,
			"artistName",
		),
	}

	item.ContentDescriptor.Identifiers.StoreAdamID =
		rawID

	item.ContentDescriptor.Identifiers.URL =
		getNestedString(
			value,
			"contentDescriptor",
			"identifiers",
			"url",
		)

	item.ContentDescriptor.Kind =
		rawKind

	item.ContentDescriptor.URL =
		getNestedString(
			value,
			"contentDescriptor",
			"url",
		)

	if artwork, ok := value["artwork"].(map[string]interface{}); ok {

		item.Artwork.URL =
			getString(
				artwork,
				"url",
			)

		if dictionary, ok :=
			artwork["dictionary"].(map[string]interface{}); ok {

			item.Artwork.Dictionary.URL =
				getString(
					dictionary,
					"url",
				)
		}
	}

	if duration, ok :=
		value["duration"].(float64); ok {

		item.Duration = int64(duration)
	}

	if subtitles, ok :=
		value["subtitleLinks"].([]interface{}); ok {

		for _, subtitle := range subtitles {

			object, ok :=
				subtitle.(map[string]interface{})

			if !ok {
				continue
			}

			title := getString(
				object,
				"title",
			)

			if title != "" {
				item.SubtitleLinks =
					append(
						item.SubtitleLinks,
						struct {
							Title string `json:"title"`
						}{
							Title: title,
						},
					)
			}
		}
	}

	if tertiary, ok :=
		value["tertiaryLinks"].([]interface{}); ok {

		for _, link := range tertiary {

			object, ok :=
				link.(map[string]interface{})

			if !ok {
				continue
			}

			title := getString(
				object,
				"title",
			)

			if title != "" {
				item.TertiaryLinks =
					append(
						item.TertiaryLinks,
						struct {
							Title string `json:"title"`
						}{
							Title: title,
						},
					)
			}
		}
	}

	return item, true
}

func getString(
	value map[string]interface{},
	key string,
) string {

	result, ok := value[key].(string)

	if !ok {
		return ""
	}

	return strings.TrimSpace(result)
}

func getNestedString(
	value map[string]interface{},
	keys ...string,
) string {

	current := interface{}(value)

	for _, key := range keys {

		object, ok :=
			current.(map[string]interface{})

		if !ok {
			return ""
		}

		current = object[key]
	}

	result, ok := current.(string)

	if !ok {
		return ""
	}

	return strings.TrimSpace(result)
}
