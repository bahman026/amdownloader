package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// LRCLIB is the lyrics source. It is a public, no-key API, and it is
// asked for a duration so it can match the right recording rather than
// a cover or a remaster of a different length.
//
// Missing lyrics are an ordinary outcome, never a track failure.

const defaultLyricsBaseURL = "https://lrclib.net"

// durationTolerance is how far the matched recording may be from ours.
// LRCLIB itself allows a couple of seconds on /api/get; the same bound
// is applied when picking from search results.
const durationTolerance = 2.0

type LyricsProvider struct {
	Client   *http.Client
	Log      *Logger
	BaseURL  string
	Language string
}

func (p *LyricsProvider) baseURL() string {
	if strings.TrimSpace(p.BaseURL) != "" {
		return strings.TrimRight(p.BaseURL, "/")
	}

	return defaultLyricsBaseURL
}

func (p *LyricsProvider) language() string {
	if len(p.Language) == 3 {
		return p.Language
	}

	return "und"
}

// lrclibRecord is one entry from either endpoint.
type lrclibRecord struct {
	TrackName    string  `json:"trackName"`
	ArtistName   string  `json:"artistName"`
	Duration     float64 `json:"duration"`
	Instrumental bool    `json:"instrumental"`
	SyncedLyrics string  `json:"syncedLyrics"`
}

// Fetch returns timed lyrics for a track.
//
// A nil slice with a nil error means the track simply has none, which
// is not a failure. An error means the lookup itself went wrong.
func (p *LyricsProvider) Fetch(
	ctx context.Context,
	track Track,
) (Lyrics, error) {

	name := strings.TrimSpace(track.Name)
	artist := strings.TrimSpace(track.Artist)

	if name == "" || artist == "" {
		return Lyrics{}, nil
	}

	seconds := parseDurationSeconds(track.Duration)

	// Exact lookup first: with a duration this returns the matching
	// recording rather than a same-titled one.
	record, err := p.get(ctx, track, seconds)
	if err != nil {
		return Lyrics{}, err
	}

	if record == nil {
		// Fall back to search, which is looser, and pick the closest
		// duration that actually has synced lyrics.
		record, err = p.search(ctx, track, seconds)
		if err != nil {
			return Lyrics{}, err
		}
	}

	if record == nil || record.Instrumental {
		return Lyrics{}, nil
	}

	lines := ParseLRC(record.SyncedLyrics)

	if len(lines) == 0 {
		return Lyrics{}, nil
	}

	return Lyrics{Lines: lines, LRC: record.SyncedLyrics}, nil
}

func (p *LyricsProvider) get(
	ctx context.Context,
	track Track,
	seconds float64,
) (*lrclibRecord, error) {

	query := url.Values{}
	query.Set("track_name", track.Name)
	query.Set("artist_name", track.Artist)

	if album := strings.TrimSpace(track.Album); album != "" &&
		album != "Unknown Album" {

		query.Set("album_name", album)
	}

	if seconds > 0 {
		query.Set("duration", strconv.FormatInt(int64(seconds), 10))
	}

	body, status, err := p.do(ctx, "/api/get?"+query.Encode())
	if err != nil {
		return nil, err
	}

	// Not found is the normal "this track has no lyrics" answer.
	if status == http.StatusNotFound {
		return nil, nil
	}

	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("lrclib get returned HTTP %d", status)
	}

	var record lrclibRecord

	if err := json.Unmarshal(body, &record); err != nil {
		return nil, fmt.Errorf("decode lrclib response: %w", err)
	}

	if strings.TrimSpace(record.SyncedLyrics) == "" && !record.Instrumental {
		return nil, nil
	}

	return &record, nil
}

func (p *LyricsProvider) search(
	ctx context.Context,
	track Track,
	seconds float64,
) (*lrclibRecord, error) {

	query := url.Values{}
	query.Set("track_name", track.Name)
	query.Set("artist_name", track.Artist)

	body, status, err := p.do(ctx, "/api/search?"+query.Encode())
	if err != nil {
		return nil, err
	}

	if status == http.StatusNotFound {
		return nil, nil
	}

	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("lrclib search returned HTTP %d", status)
	}

	var records []lrclibRecord

	if err := json.Unmarshal(body, &records); err != nil {
		return nil, fmt.Errorf("decode lrclib search: %w", err)
	}

	var best *lrclibRecord
	bestDelta := math.MaxFloat64

	for i := range records {
		candidate := &records[i]

		if strings.TrimSpace(candidate.SyncedLyrics) == "" {
			continue
		}

		// Without a duration to compare, take the first usable hit.
		if seconds <= 0 {
			return candidate, nil
		}

		delta := math.Abs(candidate.Duration - seconds)

		if delta <= durationTolerance && delta < bestDelta {
			best = candidate
			bestDelta = delta
		}
	}

	return best, nil
}

func (p *LyricsProvider) do(
	ctx context.Context,
	path string,
) ([]byte, int, error) {

	// The pipeline passes a deadline, but a direct caller might not,
	// and the shared HTTP client has no timeout of its own. Without
	// this a lookup could hang indefinitely.
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, lyricsTimeout)
		defer cancel()
	}

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		p.baseURL()+path,
		nil,
	)
	if err != nil {
		return nil, 0, err
	}

	req.Header.Set("Accept", "application/json")

	// LRCLIB asks clients to identify themselves.
	req.Header.Set("User-Agent", "media-cli (https://github.com/)")

	client := p.Client

	if client == nil {
		client = http.DefaultClient
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}

	defer resp.Body.Close()

	body, err := readAndDrain(resp, 4<<20)
	if err != nil {
		return nil, resp.StatusCode, err
	}

	return body, resp.StatusCode, nil
}

// ---------------------------------------------------------------
// LRC parsing
// ---------------------------------------------------------------

var (
	lrcTimestamp = regexp.MustCompile(`\[(\d{1,3}):([0-5]?\d)(?:[.:](\d{1,3}))?\]`)
	lrcOffsetTag = regexp.MustCompile(`(?i)^\s*\[offset:\s*([+-]?\d+)\s*\]\s*$`)
)

// ParseLRC turns LRC text into timed lines.
//
// A line may carry several timestamps, meaning the same text is sung at
// each of them. Metadata tags are ignored, except offset, which shifts
// every timestamp as the format intends.
func ParseLRC(content string) []LyricLine {
	if strings.TrimSpace(content) == "" {
		return nil
	}

	var (
		lines  []LyricLine
		offset int64
	)

	for _, raw := range strings.Split(content, "\n") {
		raw = strings.TrimRight(raw, "\r")

		if match := lrcOffsetTag.FindStringSubmatch(raw); match != nil {
			if value, err := strconv.ParseInt(match[1], 10, 64); err == nil {
				offset = value
			}

			continue
		}

		stamps := lrcTimestamp.FindAllStringSubmatchIndex(raw, -1)

		if len(stamps) == 0 {
			continue
		}

		// Text is whatever follows the last leading timestamp.
		text := strings.TrimSpace(raw[stamps[len(stamps)-1][1]:])

		for _, stamp := range stamps {
			at := lrcStampToMillis(raw, stamp) + offset

			if at < 0 {
				at = 0
			}

			lines = append(lines, LyricLine{At: at, Text: text})
		}
	}

	sort.SliceStable(lines, func(i, j int) bool {
		return lines[i].At < lines[j].At
	})

	return lines
}

// lrcStampToMillis converts one matched timestamp to milliseconds.
func lrcStampToMillis(raw string, match []int) int64 {
	group := func(n int) string {
		start, end := match[2*n], match[2*n+1]

		if start < 0 {
			return ""
		}

		return raw[start:end]
	}

	minutes, _ := strconv.ParseInt(group(1), 10, 64)
	seconds, _ := strconv.ParseInt(group(2), 10, 64)

	var millis int64

	if fraction := group(3); fraction != "" {
		value, _ := strconv.ParseInt(fraction, 10, 64)

		// Centiseconds in most files, milliseconds in some.
		switch len(fraction) {
		case 1:
			millis = value * 100
		case 2:
			millis = value * 10
		default:
			millis = value
		}
	}

	return minutes*60_000 + seconds*1_000 + millis
}

// parseDurationSeconds reads the "m:ss" or "h:mm:ss" form the Apple
// Music parsers produce.
func parseDurationSeconds(value string) float64 {
	value = strings.TrimSpace(value)

	if value == "" {
		return 0
	}

	parts := strings.Split(value, ":")

	var total float64

	for _, part := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil {
			return 0
		}

		total = total*60 + float64(n)
	}

	return total
}
