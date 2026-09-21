package main

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type ProcessResult struct {
	Track   Track
	Error   error
	Skipped bool
	Bytes   int64
}

// StageStats accumulates timings so the concurrency of each stage can be
// tuned against measurements instead of guesses.
type StageStats struct {
	mu    sync.Mutex
	count int
	total time.Duration
	max   time.Duration
}

func (s *StageStats) Observe(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.count++
	s.total += d

	if d > s.max {
		s.max = d
	}
}

func (s *StageStats) Snapshot() (count int, avg, max time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.count == 0 {
		return 0, 0, 0
	}

	return s.count, s.total / time.Duration(s.count), s.max
}

// pipelineItem carries one track through the stages.
type pipelineItem struct {
	track Track
	plan  outputPlan
	dlink string
	saved string
}

// Processor runs tracks through resolve, metadata and download as three
// independently bounded stages.
//
// Previously a single pool of 10 workers ran all three steps in sequence
// for one track, so the stages were coupled: a worker spending twenty
// seconds on a 7 MiB download was a worker unavailable to resolve, and
// the resolver's own limit of 3 capped the whole pipeline's entry rate
// regardless of how many workers existed. Splitting them means a slow
// download no longer starves resolving, and each stage's limit can be
// set to what that particular endpoint tolerates.
type Processor struct {
	ResolveConcurrency  int
	SaveConcurrency     int
	DownloadConcurrency int

	// Quality is the bitrate requested from the resolver.
	Quality int

	// DisableSkipExisting processes every track even if its file is
	// already on disk. Inverted so the zero value keeps skipping on,
	// which is the useful default.
	DisableSkipExisting bool

	Resolver   MediaResolver
	Downloader *Downloader
	OutputDir  string
	Log        *Logger

	ResolveRetry  retryPolicy
	SaveRetry     retryPolicy
	DownloadRetry retryPolicy

	ResolveStats  StageStats
	SaveStats     StageStats
	DownloadStats StageStats

	SkippedCount int
}

// alreadyDownloaded reports whether the track can be skipped. Claim has
// a side effect, so it is not called at all when skipping is off.
func (p *Processor) alreadyDownloaded(
	completed *CompletedIndex,
	track Track,
	plan outputPlan,
) (bool, string) {

	if p.DisableSkipExisting {
		return false, ""
	}

	return completed.Claim(track, plan)
}

// quality falls back to the default bitrate when unset, so a Processor
// built without settings still behaves as before.
func (p *Processor) quality() int {
	if p.Quality <= 0 {
		return 128
	}

	return p.Quality
}

func clampConcurrency(value, fallback int) int {
	if value <= 0 {
		return fallback
	}

	return value
}

func (p *Processor) Process(
	ctx context.Context,
	tracks []Track,
) []ProcessResult {

	if len(tracks) == 0 {
		return nil
	}

	if p.Resolver == nil || p.Downloader == nil {
		results := make([]ProcessResult, 0, len(tracks))

		for _, track := range tracks {
			results = append(results, ProcessResult{
				Track: track,
				Error: fmt.Errorf("processor is not configured"),
			})
		}

		return results
	}

	resolveN := clampConcurrency(p.ResolveConcurrency, 3)
	saveN := clampConcurrency(p.SaveConcurrency, 3)
	downloadN := clampConcurrency(p.DownloadConcurrency, 4)

	// Every channel is buffered to the full track count, so no stage
	// can ever block handing work to the next one. That removes any
	// possibility of the pipeline deadlocking on itself.
	toResolve := make(chan pipelineItem, len(tracks))
	toSave := make(chan pipelineItem, len(tracks))
	toDownload := make(chan pipelineItem, len(tracks))
	results := make(chan ProcessResult, len(tracks))

	// Anything already on disk is settled before a single request is
	// made, so a rerun after a partial failure costs no network at all.
	completed := p.Downloader.NewCompletedIndex()

	for _, track := range tracks {
		plan := p.Downloader.Plan(track)

		if done, how := p.alreadyDownloaded(completed, track, plan); done {
			p.SkippedCount++

			p.Log.Printf(
				"[SKIP %02d] Already downloaded (%s): %s\n",
				track.Index+1,
				how,
				plan.Stem,
			)

			results <- ProcessResult{
				Track:   track,
				Skipped: true,
			}

			continue
		}

		toResolve <- pipelineItem{
			track: track,
			plan:  plan,
		}
	}

	close(toResolve)

	var resolveWG, saveWG, downloadWG sync.WaitGroup

	// ----------------------------------------
	// Stage 1: resolve
	// ----------------------------------------

	for i := 0; i < resolveN; i++ {
		resolveWG.Add(1)

		go func() {
			defer resolveWG.Done()

			for item := range toResolve {
				dlink, err := p.resolve(ctx, item.track)

				if err != nil {
					results <- ProcessResult{
						Track: item.track,
						Error: err,
					}

					continue
				}

				item.dlink = dlink
				toSave <- item
			}
		}()
	}

	// ----------------------------------------
	// Stage 2: metadata / MP3 generation
	// ----------------------------------------

	for i := 0; i < saveN; i++ {
		saveWG.Add(1)

		go func() {
			defer saveWG.Done()

			for item := range toSave {
				saved, err := p.saveID3(ctx, item)

				if err != nil {
					results <- ProcessResult{
						Track: item.track,
						Error: err,
					}

					continue
				}

				item.saved = saved
				toDownload <- item
			}
		}()
	}

	// ----------------------------------------
	// Stage 3: download
	// ----------------------------------------

	for i := 0; i < downloadN; i++ {
		downloadWG.Add(1)

		go func() {
			defer downloadWG.Done()

			for item := range toDownload {
				written, err := p.download(ctx, item)

				results <- ProcessResult{
					Track: item.track,
					Error: err,
					Bytes: written,
				}
			}
		}()
	}

	go func() {
		resolveWG.Wait()
		close(toSave)

		saveWG.Wait()
		close(toDownload)

		downloadWG.Wait()
		close(results)
	}()

	output := make([]ProcessResult, 0, len(tracks))

	for result := range results {
		output = append(output, result)
	}

	return output
}

func (p *Processor) resolve(
	ctx context.Context,
	track Track,
) (string, error) {

	var dlink string

	err := retryOperation(
		ctx,
		p.ResolveRetry,
		"resolve",
		func(attempt int, delay time.Duration, err error) {
			p.Log.Printf(
				"[RESOLVE %02d] attempt %d failed (%v); retrying in %s\n",
				track.Index+1,
				attempt,
				err,
				delay.Round(time.Millisecond),
			)
		},
		func(ctx context.Context) error {
			attemptCtx, cancel := context.WithTimeout(
				ctx,
				resolveTimeout,
			)
			defer cancel()

			start := time.Now()

			response, err := p.Resolver.Resolve(
				attemptCtx,
				track,
				ResolveRequest{
					SongName: track.Name,
					Artist:   track.Artist,
					URL:      track.Link,
					Quality:  p.quality(),
				},
			)

			p.ResolveStats.Observe(time.Since(start))

			if err != nil {
				return err
			}

			if response == nil {
				return permanent(fmt.Errorf(
					"resolver returned nil response",
				))
			}

			if response.DLink == "" {
				return permanent(fmt.Errorf(
					"resolver returned empty download URL",
				))
			}

			dlink = response.DLink

			return nil
		},
	)

	if err != nil {
		return "", err
	}

	p.Log.Printf(
		"[RESOLVE %02d] ok: %s\n",
		track.Index+1,
		track.Name,
	)

	return dlink, nil
}

func (p *Processor) saveID3(
	ctx context.Context,
	item pipelineItem,
) (string, error) {

	var saved string

	err := retryOperation(
		ctx,
		p.SaveRetry,
		"save ID3 metadata",
		func(attempt int, delay time.Duration, err error) {
			p.Log.Printf(
				"[ID3 %02d] attempt %d failed (%v); retrying in %s\n",
				item.track.Index+1,
				attempt,
				err,
				delay.Round(time.Millisecond),
			)
		},
		func(ctx context.Context) error {
			attemptCtx, cancel := context.WithTimeout(
				ctx,
				saveTimeout,
			)
			defer cancel()

			start := time.Now()

			name, err := p.Downloader.SaveID3(
				attemptCtx,
				item.track,
				item.dlink,
			)

			p.SaveStats.Observe(time.Since(start))

			if err != nil {
				return err
			}

			saved = name

			return nil
		},
	)

	if err != nil {
		return "", fmt.Errorf("save ID3 metadata: %w", err)
	}

	p.Log.Printf(
		"[ID3 %02d] created: %s\n",
		item.track.Index+1,
		saved,
	)

	return saved, nil
}

func (p *Processor) download(
	ctx context.Context,
	item pipelineItem,
) (int64, error) {

	var written int64

	err := retryOperation(
		ctx,
		p.DownloadRetry,
		"download generated MP3",
		func(attempt int, delay time.Duration, err error) {
			p.Log.Printf(
				"[MP3 %02d] attempt %d failed (%v); retrying in %s\n",
				item.track.Index+1,
				attempt,
				err,
				delay.Round(time.Millisecond),
			)
		},
		func(ctx context.Context) error {
			attemptCtx, cancel := context.WithTimeout(
				ctx,
				downloadTimeout,
			)
			defer cancel()

			start := time.Now()

			n, err := p.Downloader.DownloadSaved(
				attemptCtx,
				item.track,
				item.saved,
				item.plan,
			)

			p.DownloadStats.Observe(time.Since(start))

			if err != nil {
				return err
			}

			written = n

			return nil
		},
	)

	if err != nil {
		// The retry budget is spent, so the partial transfer is no
		// longer useful. Removing it here is what keeps a failed
		// download from leaving anything behind.
		p.Downloader.DiscardPartial(item.saved, item.plan)

		return written, fmt.Errorf("download generated MP3: %w", err)
	}

	p.Log.Printf(
		"[MP3 %02d] done: %s (%s)\n",
		item.track.Index+1,
		item.plan.Stem,
		formatBytes(written),
	)

	return written, nil
}
