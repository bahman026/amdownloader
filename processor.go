package main

import (
	"context"
	"fmt"
	"time"
)

type ProcessResult struct {
	Track Track
	Error error
}

type Processor struct {
	Workers    int
	Resolver   MediaResolver
	Downloader *Downloader
	OutputDir  string
}

func (p *Processor) Process(
	ctx context.Context,
	tracks []Track,
) []ProcessResult {

	if p.Workers <= 0 {
		p.Workers = 4
	}

	if len(tracks) == 0 {
		return nil
	}

	jobs := make(chan Track)
	results := make(chan ProcessResult)

	for i := 0; i < p.Workers; i++ {

		workerID := i + 1

		go func() {

			for {

				select {

				case <-ctx.Done():
					return

				case track, ok := <-jobs:

					if !ok {
						return
					}

					fmt.Printf(
						"[WORKER %02d] Processing: %s - %s\n",
						workerID,
						track.Name,
						track.Artist,
					)

					err := p.processTrack(
						ctx,
						track,
						workerID,
					)

					select {

					case results <- ProcessResult{
						Track: track,
						Error: err,
					}:

					case <-ctx.Done():
						return
					}
				}
			}

		}()
	}

	go func() {

		defer close(jobs)

		for _, track := range tracks {

			select {

			case <-ctx.Done():
				return

			case jobs <- track:
			}
		}

	}()

	output := make(
		[]ProcessResult,
		0,
		len(tracks),
	)

	for i := 0; i < len(tracks); i++ {

		select {

		case result := <-results:

			output = append(
				output,
				result,
			)

		case <-ctx.Done():
			return output
		}
	}

	return output
}

func (p *Processor) processTrack(
	ctx context.Context,
	track Track,
	workerID int,
) error {

	if p.Resolver == nil {
		return fmt.Errorf(
			"resolver is nil",
		)
	}

	if p.Downloader == nil {
		return fmt.Errorf(
			"downloader is nil",
		)
	}

	response, err := p.retryResolve(
		ctx,
		track,
		workerID,
	)

	if err != nil {
		return err
	}

	if response == nil {
		return fmt.Errorf(
			"resolver returned nil response",
		)
	}

	if response.DLink == "" {
		return fmt.Errorf(
			"resolver returned empty download URL",
		)
	}

	fmt.Printf(
		"[RESOLVER %02d] Status: %s\n",
		workerID,
		response.Status,
	)

	fmt.Printf(
		"[RESOLVER %02d] DLink: %s\n",
		workerID,
		response.DLink,
	)

	fmt.Printf(
		"[RESOLVER %02d] Comments: %s\n",
		workerID,
		response.Comments,
	)

	// ----------------------------------------
	// Save metadata / create MP3
	// ----------------------------------------

	fmt.Printf(
		"[WORKER %02d] Saving metadata: %s\n",
		workerID,
		track.Name,
	)

	filename, err := p.Downloader.SaveID3(
		ctx,
		track,
		response.DLink,
	)

	if err != nil {
		return fmt.Errorf(
			"save ID3 metadata: %w",
			err,
		)
	}

	// ----------------------------------------
	// Download generated MP3
	// ----------------------------------------

	fmt.Printf(
		"[WORKER %02d] Downloading MP3: %s\n",
		workerID,
		filename,
	)

	if err := p.Downloader.DownloadSaved(
		ctx,
		track,
		filename,
	); err != nil {

		return fmt.Errorf(
			"download generated MP3: %w",
			err,
		)
	}

	return nil
}

func (p *Processor) retryResolve(
	ctx context.Context,
	track Track,
	workerID int,
) (*ResolveResponse, error) {

	const maxAttempts = 3

	for attempt := 1; attempt <= maxAttempts; attempt++ {

		fmt.Printf(
			"[RESOLVER %02d] Attempt %d/%d: %s - %s\n",
			workerID,
			attempt,
			maxAttempts,
			track.Name,
			track.Artist,
		)

		response, err := p.Resolver.Resolve(
			ctx,
			track,
			ResolveRequest{
				SongName: track.Name,
				Artist:   track.Artist,
				URL:      track.Link,
				Quality:  128,
			},
		)

		if err == nil {
			return response, nil
		}

		fmt.Printf(
			"[RESOLVER %02d] ERROR: %v\n",
			workerID,
			err,
		)

		if attempt < maxAttempts {

			delay := time.Duration(attempt) *
				time.Second

			fmt.Printf(
				"[RESOLVER %02d] Retrying in %s...\n",
				workerID,
				delay,
			)

			select {

			case <-ctx.Done():
				return nil, ctx.Err()

			case <-time.After(delay):
			}
		}
	}

	return nil, fmt.Errorf(
		"resolver failed after %d attempts",
		maxAttempts,
	)
}