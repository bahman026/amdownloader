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
	"time"
)

type Downloader struct {
	Client    *http.Client
	Progress  *ProgressManager
	OutputDir string
}

func (d *Downloader) Download(
	ctx context.Context,
	track Track,
	mediaURL string,
	outputPath string,
) error {

	if mediaURL == "" {
		return fmt.Errorf("media URL is empty")
	}

	if d.Client == nil {
		d.Client = &http.Client{
			Timeout: 30 * time.Minute,
		}
	}

	fmt.Printf(
		"[DOWNLOAD %02d] Starting\n",
		track.Index+1,
	)

	fmt.Printf(
		"[DOWNLOAD %02d] URL: %s\n",
		track.Index+1,
		mediaURL,
	)

	fmt.Printf(
		"[DOWNLOAD %02d] Destination: %s\n",
		track.Index+1,
		outputPath,
	)

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		mediaURL,
		nil,
	)
	if err != nil {
		return fmt.Errorf(
			"create request: %w",
			err,
		)
	}

	req.Header.Set(
		"User-Agent",
		"media-cli/1.0",
	)

	req.Header.Set(
		"Accept",
		"*/*",
	)

	resp, err := d.Client.Do(req)
	if err != nil {
		return fmt.Errorf(
			"download request failed: %w",
			err,
		)
	}

	defer resp.Body.Close()

	fmt.Printf(
		"[DOWNLOAD %02d] HTTP Status: %s\n",
		track.Index+1,
		resp.Status,
	)

	if resp.StatusCode < 200 ||
		resp.StatusCode >= 300 {

		body, _ := io.ReadAll(
			io.LimitReader(
				resp.Body,
				4096,
			),
		)

		return fmt.Errorf(
			"HTTP %d: %s; body=%q",
			resp.StatusCode,
			resp.Status,
			string(body),
		)
	}

	if err := os.MkdirAll(
		filepath.Dir(outputPath),
		0755,
	); err != nil {
		return fmt.Errorf(
			"create output directory: %w",
			err,
		)
	}

	file, err := os.Create(
		outputPath,
	)
	if err != nil {
		return fmt.Errorf(
			"create output file: %w",
			err,
		)
	}

	defer file.Close()

	label := fmt.Sprintf(
		"%02d - %s - %s",
		track.Index+1,
		track.Name,
		track.Artist,
	)

	if d.Progress != nil {
		d.Progress.Start(
			track.Index,
			label,
			resp.ContentLength,
		)
	}

	reader := io.Reader(resp.Body)

	if d.Progress != nil {
		reader = &ProgressReader{
			Reader:   resp.Body,
			TrackID:  track.Index,
			Progress: d.Progress,
		}
	}

	written, err := io.Copy(
		file,
		reader,
	)

	if err != nil {

		if d.Progress != nil {
			d.Progress.Complete(
				track.Index,
				err,
			)
		}

		return fmt.Errorf(
			"write downloaded file: %w",
			err,
		)
	}

	if d.Progress != nil {
		d.Progress.Complete(
			track.Index,
			nil,
		)
	}

	fmt.Printf(
		"[DOWNLOAD %02d] Completed: %s (%s)\n",
		track.Index+1,
		outputPath,
		formatBytes(written),
	)

	return nil
}

func (d *Downloader) SaveID3(
	ctx context.Context,
	track Track,
	mediaURL string,
) (string, error) {

	if mediaURL == "" {
		return "", fmt.Errorf(
			"media URL is empty",
		)
	}

	if d.Client == nil {
		d.Client = &http.Client{
			Timeout: 30 * time.Minute,
		}
	}

	form := url.Values{}

	form.Set(
		"url",
		mediaURL,
	)

	form.Set(
		"name",
		track.Name,
	)

	form.Set(
		"artist",
		track.Artist,
	)

	form.Set(
		"album",
		track.Album,
	)

	form.Set(
		"thumb",
		track.Thumb,
	)

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		"https://aaplmusicdownloader.com/api/composer/ffmpeg/saveid3.php",
		strings.NewReader(form.Encode()),
	)

	if err != nil {
		return "", fmt.Errorf(
			"create saveid3 request: %w",
			err,
		)
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
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) "+
			"AppleWebKit/537.36 (KHTML, like Gecko) "+
			"Chrome/153.0.0.0 Safari/537.36",
	)

	req.Header.Set(
		"X-Requested-With",
		"XMLHttpRequest",
	)

	fmt.Printf(
		"[ID3 %02d] Saving metadata...\n",
		track.Index+1,
	)

	resp, err := d.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf(
			"saveid3 request failed: %w",
			err,
		)
	}

	defer resp.Body.Close()

	body, err := io.ReadAll(
		io.LimitReader(
			resp.Body,
			1024*1024,
		),
	)

	if err != nil {
		return "", fmt.Errorf(
			"read saveid3 response: %w",
			err,
		)
	}

	if resp.StatusCode < 200 ||
		resp.StatusCode >= 300 {

		return "", fmt.Errorf(
			"saveid3 HTTP %d: %s; body=%q",
			resp.StatusCode,
			resp.Status,
			string(body),
		)
	}

	filename := strings.TrimSpace(
		string(body),
	)

	if filename == "" {
		return "", fmt.Errorf(
			"saveid3 returned empty filename",
		)
	}

	fmt.Printf(
		"[ID3 %02d] Created: %s\n",
		track.Index+1,
		filename,
	)

	return filename, nil
}

func (d *Downloader) DownloadSaved(
	ctx context.Context,
	track Track,
	filename string,
) error {

	filename = strings.TrimSpace(
		filename,
	)

	if filename == "" {
		return fmt.Errorf(
			"saved filename is empty",
		)
	}

	downloadURL :=
		"https://aaplmusicdownloader.com/api/composer/ffmpeg/saved/" +
			url.PathEscape(filename)

	outputDir := d.OutputDir

	if strings.TrimSpace(outputDir) == "" {
		outputDir = "./downloads"
	}

	if err := os.MkdirAll(
		outputDir,
		0755,
	); err != nil {
		return fmt.Errorf(
			"create output directory: %w",
			err,
		)
	}

	outputName := safeFilename(
		filename,
	)

	outputPath := filepath.Join(
		outputDir,
		outputName,
	)

	fmt.Printf(
		"[MP3 %02d] Downloading: %s\n",
		track.Index+1,
		filename,
	)

	return d.Download(
		ctx,
		track,
		downloadURL,
		outputPath,
	)
}