package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const (
	defaultSaveID3URL   = "https://aaplmusicdownloader.com/api/composer/ffmpeg/saveid3.php"
	defaultSavedBaseURL = "https://aaplmusicdownloader.com/api/composer/ffmpeg/saved/"
)

type Downloader struct {
	Session   *SessionManager
	Progress  *ProgressManager
	OutputDir string
	Log       *Logger

	// Endpoint overrides. Empty means the production endpoints.
	SaveID3URL   string
	SavedBaseURL string

	// DisableLegacyDetection turns off matching of files saved under
	// the older naming scheme. Inverted so the zero value keeps
	// detection on, which is the useful default.
	DisableLegacyDetection bool
}

func (d *Downloader) saveID3URL() string {
	if strings.TrimSpace(d.SaveID3URL) != "" {
		return d.SaveID3URL
	}

	return defaultSaveID3URL
}

func (d *Downloader) savedBaseURL() string {
	if strings.TrimSpace(d.SavedBaseURL) != "" {
		return d.SavedBaseURL
	}

	return defaultSavedBaseURL
}

// outputPlan is the destination for one track, derived entirely from the
// track itself.
//
// The old code named files after the string saveid3.php returned, which
// is built from the song name and artist. Two copies of the same song in
// one playlist therefore produced the same path and raced each other
// through os.Create: on the sample playlist 40 tracks left only 36
// files. Including the track index makes the name unique per entry, so
// intentional duplicates are all kept, and it makes the name knowable
// before any request is sent.
type outputPlan struct {
	Dir  string
	Stem string
}

func (p outputPlan) FinalPath(ext string) string {
	return filepath.Join(p.Dir, p.Stem+ext)
}

func (p outputPlan) PartPath(ext string) string {
	return p.FinalPath(ext) + ".part"
}

func (d *Downloader) dir() string {
	if strings.TrimSpace(d.OutputDir) == "" {
		return "./downloads"
	}

	return d.OutputDir
}

// Plan returns where a track will be written.
func (d *Downloader) Plan(track Track) outputPlan {
	stem := fmt.Sprintf(
		"%02d - %s - %s",
		track.Index+1,
		track.Name,
		track.Artist,
	)

	return outputPlan{
		Dir:  d.dir(),
		Stem: safeFilename(stem),
	}
}

// CompletedIndex is a one-shot scan of the output directory used to
// decide which tracks are already downloaded.
//
// It recognises two naming schemes: the current one, matched exactly by
// stem, and the older one that named files after whatever the remote
// service returned, matched by canonical key.
//
// Legacy matches are counted rather than flagged, because the old scheme
// collapsed duplicate tracks onto a single file. Three copies of one song
// with only one legacy file on disk will skip once and download the
// other two under their own names.
//
// Claim mutates the index, so it is used only from the single-threaded
// pre-flight pass before any worker starts.
type CompletedIndex struct {
	exact        map[string]bool
	legacy       map[string]int
	detectLegacy bool
}

func (d *Downloader) NewCompletedIndex() *CompletedIndex {
	index := &CompletedIndex{
		exact:        make(map[string]bool),
		legacy:       make(map[string]int),
		detectLegacy: !d.DisableLegacyDetection,
	}

	entries, err := os.ReadDir(d.dir())
	if err != nil {
		return index
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		name := entry.Name()

		// A partial transfer is not a completed download.
		if strings.HasSuffix(name, ".part") {
			_ = os.Remove(filepath.Join(d.dir(), name))

			continue
		}

		stem := stemOf(name)

		index.exact[stem] = true
		index.legacy[canonicalKey(stem)]++
	}

	return index
}

// Claim reports whether the track is already on disk, consuming a legacy
// match if that is what matched.
func (c *CompletedIndex) Claim(
	track Track,
	plan outputPlan,
) (bool, string) {

	if c.exact[plan.Stem] {
		return true, "current naming"
	}

	if !c.detectLegacy {
		return false, ""
	}

	key := canonicalKey(track.Name + track.Artist)

	if key != "" && c.legacy[key] > 0 {
		c.legacy[key]--

		return true, "legacy naming"
	}

	return false, ""
}

// SaveID3 asks the remote service to build a tagged file and returns the
// name it created. This is a single attempt; retrying is the caller's
// decision, based on how the error is classified.
func (d *Downloader) SaveID3(
	ctx context.Context,
	track Track,
	mediaURL string,
) (string, error) {

	if mediaURL == "" {
		return "", permanent(fmt.Errorf("media URL is empty"))
	}

	if d.Session == nil {
		return "", permanent(fmt.Errorf("downloader session manager is nil"))
	}

	handle, err := d.Session.Acquire(ctx)
	if err != nil {
		return "", fmt.Errorf("establish session: %w", err)
	}

	form := url.Values{}

	form.Set("url", mediaURL)
	form.Set("name", track.Name)
	form.Set("artist", track.Artist)
	form.Set("album", track.Album)
	form.Set("thumb", track.Thumb)

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		d.saveID3URL(),
		strings.NewReader(form.Encode()),
	)
	if err != nil {
		return "", permanent(fmt.Errorf(
			"create saveid3 request: %w",
			err,
		))
	}

	req.Header.Set(
		"Accept",
		"application/json, text/javascript, */*; q=0.01",
	)

	req.Header.Set(
		"Accept-Language",
		"en-US,en;q=0.9",
	)

	req.Header.Set(
		"Content-Type",
		"application/x-www-form-urlencoded; charset=UTF-8",
	)

	req.Header.Set(
		"Origin",
		"https://aaplmusicdownloader.com",
	)

	req.Header.Set(
		"Referer",
		"https://aaplmusicdownloader.com/album.php",
	)

	req.Header.Set(
		"User-Agent",
		browserUserAgent,
	)

	req.Header.Set(
		"X-Requested-With",
		"XMLHttpRequest",
	)

	resp, err := handle.Client().Do(req)
	if err != nil {
		d.Session.Failed(handle)

		return "", classifyTransportError(
			err,
			"saveid3 request failed",
		)
	}

	defer resp.Body.Close()

	body, err := readAndDrain(resp, 1024*1024)
	if err != nil {
		return "", fmt.Errorf(
			"read saveid3 response: %w",
			err,
		)
	}

	// The service refusing the session is a session problem, not a
	// track problem: drop it so the retry runs on a fresh one.
	if d.Session.CheckResponse(handle, resp, "saveid3") {
		return "", sessionReset(fmt.Errorf(
			"saveid3 rejected session with HTTP %d: %s",
			resp.StatusCode,
			resp.Status,
		))
	}

	if err := classifyHTTPStatus(resp, body, "saveid3"); err != nil {
		return "", err
	}

	filename := strings.TrimSpace(string(body))

	if filename == "" {
		err := fmt.Errorf("saveid3 returned empty filename")

		// Permanent on purpose: this endpoint runs a server side
		// transcode, and retrying an accepted-but-empty response
		// would re-trigger that work with no reason to expect a
		// different answer. A run of them is a session signal, and
		// Failed decides when that threshold is crossed.
		if d.Session.Failed(handle) {
			return "", err
		}

		return "", permanent(err)
	}

	d.Session.Succeeded(handle)

	return filename, nil
}

// DownloadSaved fetches the generated file described by filename into the
// destination described by plan.
func (d *Downloader) DownloadSaved(
	ctx context.Context,
	track Track,
	filename string,
	plan outputPlan,
) (int64, error) {

	filename = strings.TrimSpace(filename)

	if filename == "" {
		return 0, permanent(fmt.Errorf("saved filename is empty"))
	}

	downloadURL := d.savedBaseURL() + url.PathEscape(filename)

	return d.Download(
		ctx,
		track,
		downloadURL,
		plan,
		d.extensionFor(filename, downloadURL),
	)
}

// extensionFor picks the output extension. The remote name decides it;
// the rest of the local name comes from the track.
func (d *Downloader) extensionFor(filename, downloadURL string) string {
	ext := strings.ToLower(filepath.Ext(filename))

	if ext == "" {
		ext = extensionFromURL(downloadURL)
	}

	if ext == "" {
		ext = ".mp3"
	}

	return ext
}

// DiscardPartial removes the partial transfer for a track. Called once
// the retry budget is spent, so a give-up leaves nothing behind.
func (d *Downloader) DiscardPartial(filename string, plan outputPlan) {
	_ = os.Remove(plan.PartPath(d.extensionFor(filename, "")))
}

// Download streams mediaURL to disk.
//
// The transfer lands in a .part file and is renamed into place only
// after it completes, so an interrupted or failed download can never be
// mistaken for a finished one by a later run.
func (d *Downloader) Download(
	ctx context.Context,
	track Track,
	mediaURL string,
	plan outputPlan,
	ext string,
) (int64, error) {

	if mediaURL == "" {
		return 0, permanent(fmt.Errorf("media URL is empty"))
	}

	if d.Session == nil {
		return 0, permanent(fmt.Errorf("downloader session manager is nil"))
	}

	handle, err := d.Session.Acquire(ctx)
	if err != nil {
		return 0, fmt.Errorf("establish session: %w", err)
	}

	if err := os.MkdirAll(plan.Dir, 0o755); err != nil {
		return 0, fmt.Errorf(
			"create output directory: %w",
			err,
		)
	}

	finalPath := plan.FinalPath(ext)
	partPath := plan.PartPath(ext)

	// A partial transfer left by an earlier attempt in this run is
	// resumed rather than refetched. Partials never survive between
	// runs: startup sweeps them, because the service regenerates the
	// file each time and appending to a stale one would corrupt it.
	var resumeFrom int64

	if info, statErr := os.Stat(partPath); statErr == nil && info.Size() > 0 {
		resumeFrom = info.Size()
	}

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		mediaURL,
		nil,
	)
	if err != nil {
		return 0, permanent(fmt.Errorf(
			"create request: %w",
			err,
		))
	}

	// Sent as the same session that generated the file, matching what
	// a browser on the album page would do.
	req.Header.Set("User-Agent", browserUserAgent)
	req.Header.Set("Accept", "*/*")

	req.Header.Set(
		"Referer",
		"https://aaplmusicdownloader.com/album.php",
	)

	if resumeFrom > 0 {
		req.Header.Set(
			"Range",
			fmt.Sprintf("bytes=%d-", resumeFrom),
		)
	}

	resp, err := handle.Client().Do(req)
	if err != nil {
		d.Session.Failed(handle)

		return 0, classifyTransportError(
			err,
			"download request failed",
		)
	}

	defer resp.Body.Close()

	if d.Session.CheckResponse(handle, resp, "download") {
		// Drained before returning so the connection goes back to
		// the idle pool instead of being torn down.
		_, _ = readAndDrain(resp, 4096)

		return 0, sessionReset(fmt.Errorf(
			"download rejected session with HTTP %d: %s",
			resp.StatusCode,
			resp.Status,
		))
	}

	appending := false

	// expectedTotal is the full size of the finished file, or -1 when
	// the server did not say.
	expectedTotal := int64(-1)

	switch {

	case resp.StatusCode == http.StatusPartialContent:
		// The server honoured the range: continue where we stopped.
		appending = true

		if resp.ContentLength > 0 {
			expectedTotal = resumeFrom + resp.ContentLength
		}

		d.Log.Printf(
			"[MP3 %02d] resuming at %s\n",
			track.Index+1,
			formatBytes(resumeFrom),
		)

	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable:
		// Our partial is at or past the real size: it is unusable.
		_, _ = readAndDrain(resp, 4096)
		_ = os.Remove(partPath)

		return 0, fmt.Errorf(
			"stale partial discarded for %s; will refetch",
			plan.Stem,
		)

	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		// Either no resume was requested, or the server ignored the
		// range header and is sending the whole file. Either way,
		// start from the beginning.
		resumeFrom = 0

		if resp.ContentLength > 0 {
			expectedTotal = resp.ContentLength
		}

	default:
		body, _ := readAndDrain(resp, 4096)

		return 0, classifyHTTPStatus(resp, body, "download")
	}

	flags := os.O_CREATE | os.O_WRONLY

	if appending {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}

	file, err := os.OpenFile(partPath, flags, 0o644)
	if err != nil {
		return 0, fmt.Errorf(
			"create output file: %w",
			err,
		)
	}

	if d.Progress != nil {
		d.Progress.Start(
			track.Index,
			plan.Stem,
			expectedTotal,
		)
	}

	reader := io.Reader(resp.Body)

	if d.Progress != nil {
		reader = &ProgressReader{
			Reader:   resp.Body,
			TrackID:  track.Index,
			Progress: d.Progress,
			current:  resumeFrom,
		}
	}

	written, copyErr := io.Copy(file, reader)

	total := resumeFrom + written

	closeErr := file.Close()

	if copyErr == nil && closeErr != nil {
		copyErr = fmt.Errorf("close output file: %w", closeErr)
	}

	if copyErr != nil {
		// The partial is deliberately kept so the next attempt can
		// pick up from here. Whoever gives up removes it.
		if d.Progress != nil {
			d.Progress.Complete(track.Index, copyErr)
		}

		return total, fmt.Errorf(
			"write downloaded file: %w",
			copyErr,
		)
	}

	// A short read against a known size is a truncated transfer.
	// Without this check the rename below would publish a partial
	// file as a finished one.
	if expectedTotal > 0 && total != expectedTotal {
		err := fmt.Errorf(
			"truncated download: have %d of %d bytes",
			total,
			expectedTotal,
		)

		if d.Progress != nil {
			d.Progress.Complete(track.Index, err)
		}

		return total, err
	}

	if err := os.Rename(partPath, finalPath); err != nil {
		_ = os.Remove(partPath)

		if d.Progress != nil {
			d.Progress.Complete(track.Index, err)
		}

		return total, fmt.Errorf(
			"publish downloaded file: %w",
			err,
		)
	}

	if d.Progress != nil {
		d.Progress.Complete(track.Index, nil)
	}

	d.Session.Succeeded(handle)

	return total, nil
}
