package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
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
	lyricsTimeout   = 30 * time.Second
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

		case "archive", "downloaded":
			runArchive(os.Args[2:])
			return

		case "search", "find":
			loadAppSettings()
			runSearch(strings.Join(os.Args[2:], " "))
			return

		case "help", "-h", "--help":
			printUsage()
			return
		}

		loadAppSettings()

		if isAppleMusicPlaylistURL(input) {
			runPlaylist(input)
			return
		}

		if isAppleMusicSongURL(input) {
			runSingleSong(input)
			return
		}

		if isAppleMusicAlbumURL(input) {
			runAlbumLink(input)
			return
		}

		// A link that matched neither is a link that is wrong, not a
		// phrase to go looking for. Searching for it would bury the
		// mistake under a list of unrelated songs.
		if looksLikeLink(input) {
			fmt.Printf("Unrecognised link: %s\n\n", input)
			printUsage()

			return
		}

		// Anything else is what the user is looking for.
		runSearch(strings.Join(os.Args[1:], " "))

		return
	}

	loadAppSettings()

	runAlbumDetails()
}

// loadAppSettings resolves the configuration for this run and reports
// anything it had to correct.
func loadAppSettings() {
	settings, notes := LoadSettings()

	appSettings = settings

	for _, note := range notes {
		fmt.Println(note)
	}
}

// looksLikeLink reports whether the argument was meant to be a URL.
func looksLikeLink(value string) bool {
	value = strings.TrimSpace(strings.ToLower(value))

	return strings.HasPrefix(value, "http://") ||
		strings.HasPrefix(value, "https://") ||
		strings.Contains(value, "music.apple.com")
}

func printUsage() {
	fmt.Println("media-cli")
	fmt.Println()
	fmt.Println("Usage:")
	fmt.Println("  media-cli \"song name artist\"           search, then pick what to download")
	fmt.Println("  media-cli <apple-music-playlist-url>   download a playlist")
	fmt.Println("  media-cli <apple-music-album-url>      download a whole album")
	fmt.Println("  media-cli <apple-music-song-url>       download one song")
	fmt.Println("  media-cli                              replay ./album_details")
	fmt.Println("  media-cli settings                     show or change settings")
	fmt.Println("  media-cli archive                      inspect what has been downloaded")
	fmt.Println("  media-cli help                         this message")
	fmt.Println()
	fmt.Println("Any argument that is not a link and not a command is searched")
	fmt.Println("for on Apple Music; use `media-cli search <words>` for a query")
	fmt.Println("that would otherwise read as a command.")
	fmt.Println()
	fmt.Println("A song URL is an /album/ URL containing ?i=<track id>.")
	fmt.Println()
	fmt.Println("Every finished track is recorded in the archive and is never")
	fmt.Println("downloaded twice, so re-running a playlist fetches only what")
	fmt.Println("was added to it. See `media-cli archive`.")
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

// runAlbumLink downloads every track of a pasted album URL.
func runAlbumLink(input string) {
	id := appleMusicAlbumID(input)

	if id == 0 {
		fmt.Printf("No album id in that link: %s\n\n", input)
		printUsage()

		return
	}

	fmt.Println("Fetching Apple Music album...")
	fmt.Println()

	searcher := newSongSearch()

	// The link names the store it came from, and that store is the one
	// that has the album.
	if store := appleMusicStorefront(input); store != "" {
		searcher.Country = store
	}

	ctx, cancel := context.WithTimeout(
		context.Background(),
		searchTimeout,
	)
	defer cancel()

	tracks, err := searcher.AlbumTracks(ctx, id)

	if err != nil {
		fmt.Printf("ERROR: %v\n", err)

		return
	}

	album := strings.TrimSpace(tracks[0].Album)

	if album == "" {
		album = "Unknown Album"
	}

	fmt.Printf("Album:  %s\n", album)
	fmt.Printf("Artist: %s\n", tracks[0].Artist)
	fmt.Printf("Tracks: %d\n", len(tracks))
	fmt.Println()

	downloadTracks(
		tracks,
		filepath.Join(
			appSettings.OutputDir,
			safeFilename(album),
		),
		album,
		"",
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

	// Loaded before the pipeline starts, so tracks already recorded are
	// settled without a single request. A failure here is reported and
	// the run continues: not knowing what was downloaded before costs
	// bandwidth, not correctness.
	var archive *Archive

	if appSettings.Archive {
		loaded, err := LoadArchive(appSettings.ArchiveFile)

		if err != nil {
			fmt.Printf("WARNING: %v\n", err)
			fmt.Println("Continuing without the download archive.")
		} else {
			archive = loaded
		}
	}

	progress := NewProgressManager()
	progress.SetTotal(len(tracks))

	// Live block under the scrolling log. On a redirected stdout the
	// logger ignores this and output stays plain.
	log.SetProgress(func() []string {
		return progress.Lines(6)
	})

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

	// Lyrics come from LRCLIB, a different host, so they share the
	// transport but not the session.
	var lyrics *LyricsProvider

	if appSettings.Lyrics {
		lyrics = &LyricsProvider{
			Client:   client,
			Log:      log,
			Language: appSettings.LyricsLanguage,
		}
	}

	// Local encoding is checked before a single request is made, so a
	// missing ffmpeg fails immediately rather than after downloading
	// hundreds of tracks.
	var transcoder *Transcoder

	if appSettings.AudioMode == audioModeTranscode {
		transcoder = &Transcoder{
			Bitrate: appSettings.TranscodeBitrate,
			Path:    appSettings.FFmpegPath,
		}

		if err := transcoder.Available(); err != nil {
			fmt.Printf("ERROR: %v\n", err)

			return
		}
	}

	processor := &Processor{
		ResolveConcurrency:  appSettings.ResolveConcurrency,
		SaveConcurrency:     appSettings.SaveConcurrency,
		DownloadConcurrency: appSettings.DownloadConcurrency,

		DisableSkipExisting: !appSettings.SkipExisting,
		Archive:             archive,
		LyricsConcurrency:   appSettings.LyricsConcurrency,
		Lyrics:              lyrics,
		AudioMode:           appSettings.AudioMode,
		Transcoder:          transcoder,

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

	if archive != nil {
		fmt.Printf(
			"Archive: %s (%d track(s) already downloaded)\n",
			archive.Path(),
			archive.Len(),
		)

		if archive.Malformed() > 0 {
			fmt.Printf(
				"WARNING: %d unreadable line(s) in %s were ignored\n",
				archive.Malformed(),
				archive.Path(),
			)
		}
	} else {
		fmt.Println(
			"Archive: off (tracks are matched by filename only)",
		)
	}

	fmt.Printf(
		"Concurrency: resolve=%d save=%d download=%d lyrics=%d\n",
		processor.ResolveConcurrency,
		processor.SaveConcurrency,
		processor.DownloadConcurrency,
		processor.LyricsConcurrency,
	)

	if transcoder != nil {
		fmt.Printf(
			"Audio: local encode at %d kbps from the AAC source (ffmpeg: %s)\n",
			transcoder.bitrate(),
			transcoder.Path,
		)
	} else {
		fmt.Println(
			"Audio: service MP3 (fixed 128 kbps). " +
				"Set audio_mode=transcode for higher quality.",
		)
	}

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

// isAppleMusicAlbumURL matches an /album/ link with no track id on it,
// which names the whole album.
func isAppleMusicAlbumURL(value string) bool {
	return strings.Contains(value, "music.apple.com") &&
		strings.Contains(value, "/album/") &&
		!strings.Contains(value, "?i=")
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

	skipReasons := map[string]int{}

	lyricOutcomes := map[string]int{}

	var bytes int64

	for _, result := range results {

		if result.Skipped {
			skipped++

			reason := result.SkipReason

			if reason == "" {
				reason = "already downloaded"
			}

			skipReasons[reason]++

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

		if result.Lyrics != "" {
			lyricOutcomes[result.Lyrics]++
		}

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
	fmt.Printf("Skipped:     %d%s\n", skipped, skipDetail(skipReasons))
	fmt.Printf("Failed:      %d\n", failed)
	fmt.Printf("Downloaded:  %s\n", formatBytes(bytes))
	fmt.Printf("Time:        %s\n", time.Since(start).Round(time.Millisecond))

	if len(lyricOutcomes) > 0 {
		fmt.Println()
		fmt.Println("Lyrics")

		for _, key := range sortedKeys(lyricOutcomes) {
			fmt.Printf("  %-14s %d\n", key, lyricOutcomes[key])
		}
	}

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
	printStage("lyrics  ", &processor.LyricsStats)

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

// skipDetail explains a skip count, so "Skipped: 38" says whether those
// tracks were recognised from the archive or found in the folder.
func skipDetail(reasons map[string]int) string {
	if len(reasons) == 0 {
		return ""
	}

	parts := make([]string, 0, len(reasons))

	for _, key := range sortedKeys(reasons) {
		parts = append(parts, fmt.Sprintf("%s %d", key, reasons[key]))
	}

	return "  (" + strings.Join(parts, ", ") + ")"
}

func sortedKeys(counts map[string]int) []string {
	keys := make([]string, 0, len(counts))

	for key := range counts {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	return keys
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
