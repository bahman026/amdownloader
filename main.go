package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const resolverEndpoint = "https://aaplmusicdownloader.com/api/composer/swd.php"

// Maximum number of tracks processed simultaneously.
const workerCount = 10

// Maximum number of resolver sessions running simultaneously.
const resolverConcurrency = 3

func main() {
	if len(os.Args) > 1 {
		input := os.Args[1]

		if isAppleMusicPlaylistURL(input) {
			runPlaylist(input)
			return
		}

		if isAppleMusicSongURL(input) {
			runSingleSong(input)
			return
		}
	}

	runAlbumDetails()
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
	fmt.Printf("Workers: %d\n", workerCount)
	fmt.Printf("Resolver concurrency: %d\n", resolverConcurrency)
	fmt.Println()

	albumName := strings.TrimSpace(
		playlist.Album,
	)

	if albumName == "" {
		albumName = "Unknown Album"
	}

	albumDir := filepath.Join(
		"./downloads",
		safeFilename(albumName),
	)

	if err := os.MkdirAll(
		albumDir,
		0755,
	); err != nil {
		fmt.Printf(
			"ERROR: failed to create album directory: %v\n",
			err,
		)
		return
	}

	fmt.Printf(
		"Download directory: %s\n",
		albumDir,
	)

	fmt.Println()

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

	fmt.Printf(
		"Name:     %s\n",
		track.Name,
	)

	fmt.Printf(
		"Artist:   %s\n",
		track.Artist,
	)

	fmt.Printf(
		"Album:    %s\n",
		track.Album,
	)

	fmt.Printf(
		"Duration: %s\n",
		track.Duration,
	)

	fmt.Printf(
		"Link:     %s\n",
		track.Link,
	)

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

	downloadDir := "./downloads"

	if err := os.MkdirAll(
		downloadDir,
		0755,
	); err != nil {
		fmt.Printf(
			"ERROR: failed to create download directory: %v\n",
			err,
		)
		return
	}

	fmt.Printf(
		"Download directory: %s\n",
		downloadDir,
	)

	fmt.Printf(
		"Workers: %d\n",
		workerCount,
	)

	fmt.Printf(
		"Resolver concurrency: %d\n",
		resolverConcurrency,
	)

	fmt.Println()

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
		"./downloads",
		safeFilename(albumName),
	)

	if err := os.MkdirAll(
		albumDir,
		0755,
	); err != nil {
		fmt.Printf(
			"Failed to create download directory: %v\n",
			err,
		)
		return
	}

	fmt.Printf(
		"Download directory: %s\n",
		albumDir,
	)

	fmt.Printf(
		"Workers: %d\n",
		workerCount,
	)

	fmt.Printf(
		"Resolver concurrency: %d\n",
		resolverConcurrency,
	)

	fmt.Println()

	processTracks(
		tracks,
		albumDir,
	)
}

func processTracks(
	tracks []Track,
	downloadDir string,
) {

	transport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 100,
		MaxConnsPerHost:     100,

		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	client := &http.Client{
		Timeout:   30 * time.Minute,
		Transport: transport,
	}

	progress := NewProgressManager()

	resolver := &HTTPMediaResolver{
		Endpoint:    resolverEndpoint,
		Client:      client,
		Concurrency: resolverConcurrency,
	}

	downloader := &Downloader{
		Client:    client,
		Progress:  progress,
		OutputDir: downloadDir,
	}

	processor := &Processor{
		Workers:    workerCount,
		Resolver:   resolver,
		Downloader: downloader,
		OutputDir:  downloadDir,
	}

	ctx := context.Background()

	start := time.Now()

	results := processor.Process(
		ctx,
		tracks,
	)

	printResults(
		results,
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
	start time.Time,
) {

	fmt.Println()
	fmt.Println("========================================")
	fmt.Println("Processing finished")
	fmt.Println("========================================")

	success := 0
	failed := 0

	for _, result := range results {

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

		fmt.Printf(
			"[SUCCESS %02d] %s - %s\n",
			result.Track.Index+1,
			result.Track.Name,
			result.Track.Artist,
		)
	}

	fmt.Println()

	fmt.Printf(
		"Total:   %d\n",
		len(results),
	)

	fmt.Printf(
		"Success: %d\n",
		success,
	)

	fmt.Printf(
		"Failed:  %d\n",
		failed,
	)

	fmt.Printf(
		"Time:    %s\n",
		time.Since(start).Round(time.Millisecond),
	)
}