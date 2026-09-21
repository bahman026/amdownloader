package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

type ProcessResult struct {
	Track   Track
	Error   error
	Skipped bool
	Bytes   int64

	// Lyrics records what the lyrics stage did: "synced", "none",
	// "cached", "off", or a short failure note. It never affects
	// whether the track counts as a success.
	Lyrics string
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
	track   Track
	plan    outputPlan
	dlink   string
	saved   string
	written int64

	// finalPath is set by the download stage to the file it actually
	// wrote. The lyrics stage uses it directly rather than working the
	// name out a second time, which is how the two came to disagree.
	finalPath string
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

	// DisableSkipExisting processes every track even if its file is
	// already on disk. Inverted so the zero value keeps skipping on,
	// which is the useful default.
	DisableSkipExisting bool

	// LyricsConcurrency and Lyrics control the optional final stage.
	LyricsConcurrency int
	Lyrics            *LyricsProvider

	// AudioMode selects the service's 128 kbps MP3 or a local encode
	// of the higher quality AAC source.
	AudioMode  string
	Transcoder *Transcoder

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
	LyricsStats   StageStats

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
	toLyrics := make(chan pipelineItem, len(tracks))
	results := make(chan ProcessResult, len(tracks))

	// Anything already on disk is settled before a single request is
	// made, so a rerun after a partial failure costs no network at all.
	completed := p.Downloader.NewCompletedIndex()

	for _, track := range tracks {
		plan := p.Downloader.Plan(track)

		if done, how := p.alreadyDownloaded(completed, track, plan); done {
			p.SkippedCount++

			if p.Downloader.Progress != nil {
				p.Downloader.Progress.NoteSkipped()
			}

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

	lyricsN := clampConcurrency(p.LyricsConcurrency, 4)

	var resolveWG, saveWG, downloadWG, lyricsWG sync.WaitGroup

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
					p.noteFailure()

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
					p.noteFailure()

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
				written, path, err := p.download(ctx, item)

				item.finalPath = path

				if err != nil {
					p.noteFailure()

					results <- ProcessResult{
						Track: item.track,
						Error: err,
						Bytes: written,
					}

					continue
				}

				item.written = written
				toLyrics <- item
			}
		}()
	}

	// ----------------------------------------
	// Stage 4: lyrics
	//
	// Runs against a different host, so it adds no load to the
	// download service. A track that has no lyrics, or whose lookup
	// fails, is still a successful download.
	// ----------------------------------------

	for i := 0; i < lyricsN; i++ {
		lyricsWG.Add(1)

		go func() {
			defer lyricsWG.Done()

			for item := range toLyrics {
				results <- ProcessResult{
					Track:  item.track,
					Bytes:  item.written,
					Lyrics: p.attachLyrics(ctx, item),
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
		close(toLyrics)

		lyricsWG.Wait()
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
					// The service ignores this. It hands back a
					// fixed .m4a whatever is asked for, and the
					// MP3 is produced by saveid3.php, which takes
					// no bitrate at all. 128 is what the site's
					// own client sends.
					Quality: 128,
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

func (p *Processor) noteFailure() {
	if p.Downloader != nil && p.Downloader.Progress != nil {
		p.Downloader.Progress.NoteFailed()
	}
}

func (p *Processor) transcoding() bool {
	return p.AudioMode == audioModeTranscode && p.Transcoder != nil
}

func (p *Processor) saveID3(
	ctx context.Context,
	item pipelineItem,
) (string, error) {

	// In transcode mode the service's MP3 is never built: the source
	// AAC is encoded locally instead, so this whole stage, and the
	// server-side transcode behind it, is skipped.
	if p.transcoding() {
		return "", nil
	}

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
) (int64, string, error) {

	var written int64

	// Known up front in both modes, so the lyrics stage never has to
	// guess at the extension.
	finalPath := item.plan.FinalPath(".mp3")

	if !p.transcoding() {
		finalPath = p.Downloader.SavedPath(item.saved, item.plan)
	}

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

			var n int64
			var err error

			if p.transcoding() {
				n, err = p.downloadAndEncode(attemptCtx, item)
			} else {
				n, err = p.Downloader.DownloadSaved(
					attemptCtx,
					item.track,
					item.saved,
					item.plan,
				)
			}

			p.DownloadStats.Observe(time.Since(start))

			if err != nil {
				return err
			}

			written = n

			return nil
		},
	)

	if err != nil {
		// The retry budget is spent, so the partial transfer and any
		// transcode scratch are no longer useful. Removing them here
		// is what keeps a failed download from leaving debris.
		p.Downloader.DiscardPartial(item.saved, item.plan)
		p.Downloader.DiscardScratch(item.plan)

		return written, finalPath, fmt.Errorf("download generated MP3: %w", err)
	}

	p.Log.Printf(
		"[MP3 %02d] done: %s (%s)\n",
		item.track.Index+1,
		item.plan.Stem,
		formatBytes(written),
	)

	return written, finalPath, nil
}

// attachLyrics looks up timed lyrics and embeds them in the finished
// file. It returns a short status and never an error: a track without
// lyrics is a completed track.
func (p *Processor) attachLyrics(
	ctx context.Context,
	item pipelineItem,
) string {

	if p.Lyrics == nil {
		return "off"
	}

	path := item.finalPath

	if path == "" {
		return "no file"
	}

	if HasSyncedLyrics(path) {
		return "cached"
	}

	start := time.Now()

	attemptCtx, cancel := context.WithTimeout(ctx, lyricsTimeout)
	defer cancel()

	lyrics, err := p.Lyrics.Fetch(attemptCtx, item.track)

	p.LyricsStats.Observe(time.Since(start))

	if err != nil {
		p.Log.Printf(
			"[LYRICS %02d] lookup failed: %v\n",
			item.track.Index+1,
			err,
		)

		return "lookup failed"
	}

	if len(lyrics.Lines) == 0 {
		return "none"
	}

	if err := EmbedSyncedLyrics(path, lyrics, p.Lyrics.language()); err != nil {
		p.Log.Printf(
			"[LYRICS %02d] could not embed: %v\n",
			item.track.Index+1,
			err,
		)

		return "embed failed"
	}

	p.Log.Printf(
		"[LYRICS %02d] embedded %d lines: %s\n",
		item.track.Index+1,
		len(lyrics.Lines),
		item.track.Name,
	)

	return "synced"
}

// downloadAndEncode fetches the AAC source and encodes it locally.
//
// This replaces both the remote transcode and the remote download: the
// file the service would have built at 128 kbps is never requested.
func (p *Processor) downloadAndEncode(
	ctx context.Context,
	item pipelineItem,
) (int64, error) {

	plan := item.plan

	finalPath := plan.FinalPath(".mp3")
	sourcePath := plan.FinalPath(".m4a") + ".src"
	coverPath := plan.FinalPath(".jpg") + ".src"
	outputPath := finalPath + ".part"

	// A source fetched by an earlier attempt is reused. Re-encoding is
	// cheap; re-downloading nine megabytes because ffmpeg failed is
	// not. Whoever gives up clears the scratch files.
	if info, err := os.Stat(sourcePath); err != nil || info.Size() == 0 {
		if _, err := p.Downloader.DownloadToPath(
			ctx,
			item.track,
			item.dlink,
			sourcePath,
			plan.Stem,
		); err != nil {
			return 0, err
		}
	} else {
		p.Log.Printf(
			"[DL %02d] reusing source from the previous attempt (%s)\n",
			item.track.Index+1,
			formatBytes(info.Size()),
		)
	}

	// Artwork is best effort: a track without a cover is still a
	// track, so a failure here only costs the image.
	if err := p.Downloader.FetchToFile(
		ctx,
		item.track.Thumb,
		coverPath,
	); err != nil {
		p.Log.Printf(
			"[ART %02d] no cover art (%v)\n",
			item.track.Index+1,
			err,
		)

		coverPath = ""
	}

	if err := p.Transcoder.ToMP3(
		ctx,
		sourcePath,
		outputPath,
		item.track,
		coverPath,
	); err != nil {
		_ = os.Remove(outputPath)

		// A missing encoder will not fix itself on a retry.
		if strings.Contains(err.Error(), "not found on PATH") {
			return 0, permanent(err)
		}

		return 0, err
	}

	if err := os.Rename(outputPath, finalPath); err != nil {
		_ = os.Remove(outputPath)

		return 0, fmt.Errorf("publish encoded file: %w", err)
	}

	// Only now that the file is published is the scratch disposable.
	_ = os.Remove(sourcePath)
	_ = os.Remove(coverPath)

	info, err := os.Stat(finalPath)
	if err != nil {
		return 0, err
	}

	return info.Size(), nil
}
