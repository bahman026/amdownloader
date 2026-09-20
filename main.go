package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"
)

func main() {
	input, err := os.ReadFile("album_details")
	if err != nil {
		fmt.Printf(
			"Failed to read album_details: %v\n",
			err,
		)

		return
	}

	tracks := ParseTracks(string(input))

	if len(tracks) == 0 {
		fmt.Println("No tracks found.")
		return
	}

	fmt.Printf(
		"Parsed %d tracks\n\n",
		len(tracks),
	)

	resolverEndpoint := os.Getenv("RESOLVER_ENDPOINT")

	if resolverEndpoint == "" {
		fmt.Println("ERROR: RESOLVER_ENDPOINT is not set")
		fmt.Println()
		fmt.Println("Example:")
		fmt.Println(
			"  export RESOLVER_ENDPOINT='https://YOUR-DOMAIN/api/composer/swd.php'",
		)

		return
	}

	// Downloader client.
	// Resolver creates a fresh client/session for every Resolve call.
	client := &http.Client{
		Timeout: 30 * time.Minute,
	}

	progress := NewProgressManager()

	resolver := &HTTPMediaResolver{
		Endpoint: resolverEndpoint,
	}

	downloader := &Downloader{
		Client:   client,
		Progress: progress,
	}

	processor := &Processor{
		Workers:    4,
		Resolver:   resolver,
		Downloader: downloader,
		OutputDir:  "./downloads",
	}

	ctx := context.Background()

	start := time.Now()

	results := processor.Process(
		ctx,
		tracks,
	)

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
