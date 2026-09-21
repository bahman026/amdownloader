package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const resolverEndpoint = "https://aaplmusicdownloader.com/api/composer/swd.php"

const browserUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) " +
	"AppleWebKit/537.36 (KHTML, like Gecko) " +
	"Chrome/153.0.0.0 Safari/537.36"

// Per-stage deadlines.
//
// These replace the single 30 minute client timeout, which meant a stuck
// resolve held a slot for half an hour. Each stage is now bounded by what
// that stage should plausibly take.
const (
	sessionTimeout  = 60 * time.Second
	resolveTimeout  = 2 * time.Minute
	saveTimeout     = 5 * time.Minute
	downloadTimeout = 30 * time.Minute
)

// Stage concurrency.
//
// Peak concurrent requests against the remote host is 3+3+4 = 10, the
// same ceiling the old 10-worker pool had. Nothing here is an increase:
// the gain comes from the stages no longer blocking each other, and from
// dropping the redundant session fetch, not from more load. Tuning these
// needs per-stage timings from a real run, which the summary at the end
// of a run now reports.
const (
	defaultResolveConcurrency  = 3
	defaultSaveConcurrency     = 3
	defaultDownloadConcurrency = 4
)

func envInt(name string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(name))

	if raw == "" {
		return fallback
	}

	value, err := strconv.Atoi(raw)

	if err != nil || value <= 0 {
		return fallback
	}

	return value
}

// newTransport builds the shared HTTP transport.
//
// Pool sizes are kept as they were; they were already well above what
// this program needs. Proxy support and a dial timeout are restored to
// match http.DefaultTransport, which the hand-built transport had
// silently dropped.
//
// ForceAttemptHTTP2 is required here, and only here: Go disables its
// automatic HTTP/2 upgrade as soon as a custom DialContext is set, so
// adding the dial timeout without this flag would quietly downgrade
// every request to HTTP/1.1. For the same reason TLSClientConfig is
// deliberately left nil.
func newTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,

		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,

		ForceAttemptHTTP2: true,

		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 100,
		MaxConnsPerHost:     100,

		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

// appSettings is the resolved configuration for this run.
var appSettings *Settings

func main() {
	if len(os.Args) > 1 {
		input := os.Args[1]

		switch strings.ToLower(input) {

		case "settings", "setting", "config":
			runSettings(os.Args[2:])
			return

		case "help", "-h", "--help":
			printUsage()
			return
		}

		settings, notes := LoadSettings()
		appSettings = settings

		for _, note := range notes {
			fmt.Println(note)
		}

		if isAppleMusicPlaylistURL(input) {
			runPlaylist(input)
			return
		}

		if isAppleMusicSongURL(input) {
			runSingleSong(input)
			return
		}

		fmt.Printf("Unrecognised argument: %s\n\n", input)
		printUsage()

		return
	}

	settings, notes := LoadSettings()
	appSettings = settings

	for _, note := range notes {
		fmt.Println(note)
	}

	runAlbumDetails()
}

func printUsage() {
	fmt.Println("media-cli")
	fmt.Println()
	fmt.Println("Usage:")
	fmt.Println("  media-cli <apple-music-playlist-url>   download a playlist")
	fmt.Println("  media-cli <apple-music-song-url>       download one song")
	fmt.Println("  media-cli                              replay ./album_details")
	fmt.Println("  media-cli settings                     show or change settings")
	fmt.Println("  media-cli help                         this message")
	fmt.Println()
	fmt.Println("A song URL is an /album/ URL containing ?i=<track id>.")
}

func runPlaylist(input string) {
	fmt.Println("Fetching Apple Music playlist...")
	fmt.Println()

	playlist, err := FetchAppleMusicPlaylist(input)
	if err != nil {
		fmt.Printf("ERROR: %v\n", err)
		return
	}

	tracks := playlist.Tracks
	if len(tracks) == 0 {
		fmt.Println("No tracks found.")
		return
	}

	if err := SaveAlbumDetails(playlist, "album_details"); err != nil {
		fmt.Printf(
			"ERROR: failed to save album_details: %v\n",
			err,
		)
		return
	}

	fmt.Println()
	fmt.Println("album_details saved to ./album_details")
	fmt.Printf("Tracks: %d\n", len(tracks))
	fmt.Printf("Album:  %s\n", playlist.Album)

	albumName := strings.TrimSpace(
		playlist.Album,
	)

	if albumName == "" {
		albumName = "Unknown Album"
	}

	albumDir := filepath.Join(
		appSettings.OutputDir,
		safeFilename(albumName),
	)

	if err := os.MkdirAll(
		albumDir,
		0o755,
	); err != nil {
		fmt.Printf(
			"ERROR: failed to create album directory: %v\n",
			err,
		)
		return
	}

	processTracks(
		tracks,
		albumDir,
	)
}

func runSingleSong(input string) {
	fmt.Println("Fetching Apple Music song...")
	fmt.Println()

	track, err := FetchAppleMusicSong(input)
	if err != nil {
		fmt.Printf(
			"ERROR: %v\n",
			err,
		)
		return
	}

	fmt.Println()
	fmt.Println("Single song found:")

	fmt.Printf("Name:     %s\n", track.Name)
	fmt.Printf("Artist:   %s\n", track.Artist)
	fmt.Printf("Album:    %s\n", track.Album)
	fmt.Printf("Duration: %s\n", track.Duration)
	fmt.Printf("Link:     %s\n", track.Link)

	fmt.Println()

	playlist := &AppleMusicPlaylist{
		Name:  track.Name,
		Album: track.Album,
		Thumb: track.Thumb,
		Date:  "1 song",
		Tracks: []Track{
			track,
		},
	}

	if err := SaveAlbumDetails(
		playlist,
		"album_details",
	); err != nil {
		fmt.Printf(
			"ERROR: failed to save album_details: %v\n",
			err,
		)
		return
	}

	fmt.Println(
		"album_details saved to ./album_details",
	)

	fmt.Println()

	downloadDir := appSettings.OutputDir

	if err := os.MkdirAll(
		downloadDir,
		0o755,
	); err != nil {
		fmt.Printf(
			"ERROR: failed to create download directory: %v\n",
			err,
		)
		return
	}

	processTracks(
		[]Track{track},
		downloadDir,
	)
}

func runAlbumDetails() {
	input, err := os.ReadFile(
		"album_details",
	)
	if err != nil {
		fmt.Printf(
			"Failed to read album_details: %v\n",
			err,
		)
		return
	}

	tracks := ParseTracks(
		string(input),
	)

	if len(tracks) == 0 {
		fmt.Println("No tracks found.")
		return
	}

	fmt.Printf(
		"Parsed %d tracks\n\n",
		len(tracks),
	)

	albumName := strings.TrimSpace(
		tracks[0].Album,
	)

	if albumName == "" {
		albumName = "Unknown Album"
	}

	albumDir := filepath.Join(
		appSettings.OutputDir,
		safeFilename(albumName),
	)

	if err := os.MkdirAll(
		albumDir,
		0o755,
	); err != nil {
		fmt.Printf(
			"Failed to create download directory: %v\n",
			err,
		)
		return
	}

	processTracks(
		tracks,
		albumDir,
	)
}

func processTracks(
	tracks []Track,
	downloadDir string,
) {

	// Ctrl-C cancels in-flight work instead of killing the process
	// mid-write. Partial transfers live in .part files and are removed
	// on the way out, so an interrupted run never leaves something that
	// looks like a finished download.
	ctx, stop := signal.NotifyContext(
		context.Background(),
		os.Interrupt,
		syscall.SIGTERM,
	)
	defer stop()

	// A single transport, shared by the resolver, the metadata stage
	// and downloads, so connections stay pooled across all of them.
	//
	// Client.Timeout is intentionally zero: a single deadline covering
	// every kind of request cannot be right for both a JSON call and a
	// 7 MiB transfer. Each stage sets its own via context instead.
	client := &http.Client{
		Transport: newTransport(),
	}

	log := NewLogger(os.Stdout)
	defer log.Close()

	progress := NewProgressManager()

	// One session, shared by every stage, so the service sees a single
	// coherent client instead of a new anonymous one per request.
	session := &SessionManager{
		Transport: client.Transport,
		Log:       log,
	}

	resolver := &HTTPMediaResolver{
		Endpoint: resolverEndpoint,
		Session:  session,
		Log:      log,
	}

	downloader := &Downloader{
		Session:   session,
		Progress:  progress,
		OutputDir: downloadDir,
		Log:       log,

		DisableLegacyDetection: !appSettings.DetectLegacyNames,
	}

	processor := &Processor{
		ResolveConcurrency:  appSettings.ResolveConcurrency,
		SaveConcurrency:     appSettings.SaveConcurrency,
		DownloadConcurrency: appSettings.DownloadConcurrency,

		Quality:             appSettings.Quality,
		DisableSkipExisting: !appSettings.SkipExisting,

		Resolver:   resolver,
		Downloader: downloader,
		OutputDir:  downloadDir,
		Log:        log,

		// The resolver is cheap and idempotent, so it gets the most
		// attempts. The metadata stage triggers a server side
		// transcode, so it gets the fewest.
		ResolveRetry: retryPolicy{
			MaxAttempts: appSettings.ResolveAttempts,
			BaseDelay:   500 * time.Millisecond,
			MaxDelay:    10 * time.Second,
		},

		SaveRetry: retryPolicy{
			MaxAttempts: appSettings.SaveAttempts,
			BaseDelay:   2 * time.Second,
			MaxDelay:    15 * time.Second,
		},

		DownloadRetry: retryPolicy{
			MaxAttempts: appSettings.DownloadAttempts,
			BaseDelay:   1 * time.Second,
			MaxDelay:    20 * time.Second,
		},
	}

	fmt.Printf("Download directory: %s\n", downloadDir)

	fmt.Printf(
		"Concurrency: resolve=%d save=%d download=%d   Quality: %d\n",
		processor.ResolveConcurrency,
		processor.SaveConcurrency,
		processor.DownloadConcurrency,
		processor.Quality,
	)

	fmt.Println()

	start := time.Now()

	results := processor.Process(
		ctx,
		tracks,
	)

	log.Close()

	printResults(
		results,
		processor,
		session,
		start,
	)
}

func isAppleMusicPlaylistURL(
	value string,
) bool {

	return strings.Contains(
		value,
		"music.apple.com",
	) &&
		strings.Contains(
			value,
			"/playlist/",
		)
}

func isAppleMusicSongURL(
	value string,
) bool {

	if !strings.Contains(
		value,
		"music.apple.com",
	) {
		return false
	}

	if !strings.Contains(
		value,
		"/album/",
	) {
		return false
	}

	return strings.Contains(
		value,
		"?i=",
	)
}

func printResults(
	results []ProcessResult,
	processor *Processor,
	session *SessionManager,
	start time.Time,
) {

	fmt.Println()
	fmt.Println("========================================")
	fmt.Println("Processing finished")
	fmt.Println("========================================")

	success := 0
	failed := 0
	skipped := 0

	var bytes int64

	for _, result := range results {

		if result.Skipped {
			skipped++

			continue
		}

		if result.Error != nil {

			failed++

			fmt.Printf(
				"[FAILED %02d] %s - %s\n",
				result.Track.Index+1,
				result.Track.Name,
				result.Error,
			)

			continue
		}

		success++
		bytes += result.Bytes

		fmt.Printf(
			"[SUCCESS %02d] %s - %s\n",
			result.Track.Index+1,
			result.Track.Name,
			result.Track.Artist,
		)
	}

	fmt.Println()

	fmt.Printf("Total:       %d\n", len(results))
	fmt.Printf("Success:     %d\n", success)
	fmt.Printf("Skipped:     %d\n", skipped)
	fmt.Printf("Failed:      %d\n", failed)
	fmt.Printf("Downloaded:  %s\n", formatBytes(bytes))
	fmt.Printf("Time:        %s\n", time.Since(start).Round(time.Millisecond))

	if processor == nil {
		return
	}

	// Per-stage timings, so the concurrency settings above can be
	// tuned from measurements rather than assumptions.
	fmt.Println()
	fmt.Println("Stage timings (per track)")

	printStage("resolve ", &processor.ResolveStats)
	printStage("saveid3 ", &processor.SaveStats)
	printStage("download", &processor.DownloadStats)

	if session == nil {
		return
	}

	rotations, observedLimit := session.Stats()

	fmt.Println()
	fmt.Printf("Sessions established: %d\n", rotations)

	if observedLimit > 0 {
		fmt.Printf(
			"Observed service session limit: ~%d uses\n",
			observedLimit,
		)
	}
}

func printStage(name string, stats *StageStats) {
	count, avg, max := stats.Snapshot()

	if count == 0 {
		fmt.Printf("  %s  n=0\n", name)

		return
	}

	fmt.Printf(
		"  %s  n=%-4d avg=%-10s max=%s\n",
		name,
		count,
		avg.Round(time.Millisecond),
		max.Round(time.Millisecond),
	)
}
