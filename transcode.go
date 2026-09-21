package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Local transcoding.
//
// The service hands back a ~273 kbps AAC source and separately offers a
// 128 kbps MP3 it made from that source. Taking the source and encoding
// it here gives a much better MP3 than the one the service produces,
// and skips its transcode stage entirely.
//
// Encoding lossy audio a second time does cost some fidelity against
// the AAC original. That is the trade for staying in MP3, where ID3
// tags, artwork and SYLT lyrics all work.

// validBitrates are the rates offered for the local encode.
var validBitrates = []int{128, 192, 256, 320}

type Transcoder struct {
	// Bitrate in kbps for the MP3 encode.
	Bitrate int

	// Path to the ffmpeg binary. Resolved from PATH when empty.
	Path string
}

func (t *Transcoder) bitrate() int {
	for _, b := range validBitrates {
		if t.Bitrate == b {
			return t.Bitrate
		}
	}

	return 320
}

// LookupFFmpeg finds the ffmpeg binary, or explains how to install it.
func LookupFFmpeg(configured string) (string, error) {
	if strings.TrimSpace(configured) != "" {
		if _, err := os.Stat(configured); err != nil {
			return "", fmt.Errorf(
				"ffmpeg not found at %q: %w",
				configured,
				err,
			)
		}

		return configured, nil
	}

	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		return "", fmt.Errorf(
			"ffmpeg is required for local transcoding but was not " +
				"found on PATH.\n" +
				"  Install it with:  brew install ffmpeg\n" +
				"  Or point at it:   media-cli settings set ffmpeg_path /full/path/to/ffmpeg\n" +
				"  Or turn it off:   media-cli settings set audio_mode service",
		)
	}

	return path, nil
}

// Available reports whether this transcoder can run.
func (t *Transcoder) Available() error {
	path, err := LookupFFmpeg(t.Path)
	if err != nil {
		return err
	}

	t.Path = path

	return nil
}

// ToMP3 encodes source into an MP3 at destination, writing ID3v2.3 tags
// and attaching artwork when a file is supplied.
//
// Everything is written to destination directly; the caller is expected
// to be working on a temporary path and to rename on success.
func (t *Transcoder) ToMP3(
	ctx context.Context,
	source string,
	destination string,
	track Track,
	artwork string,
) error {

	if strings.TrimSpace(t.Path) == "" {
		if err := t.Available(); err != nil {
			return err
		}
	}

	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}

	args := []string{
		"-nostdin",
		"-y",
		"-loglevel", "error",
		"-i", source,
	}

	hasArtwork := strings.TrimSpace(artwork) != ""

	if hasArtwork {
		args = append(args, "-i", artwork)
	}

	args = append(args, "-map", "0:a:0")

	if hasArtwork {
		args = append(args,
			"-map", "1:v:0",
			"-c:v", "copy",
			"-disposition:v", "attached_pic",
		)
	}

	args = append(args,
		"-c:a", "libmp3lame",
		"-b:a", strconv.Itoa(t.bitrate())+"k",

		// v2.3 so the SYLT frame written afterwards lands in a tag
		// every player understands.
		"-id3v2_version", "3",
		"-write_xing", "1",

		// Named explicitly because the output is written to a
		// .part file first. ffmpeg infers the muxer from the
		// extension otherwise, and fails on anything unfamiliar.
		"-f", "mp3",
	)

	for key, value := range map[string]string{
		"title":  track.Name,
		"artist": track.Artist,
		"album":  track.Album,
	} {
		if strings.TrimSpace(value) != "" {
			args = append(args, "-metadata", key+"="+value)
		}
	}

	args = append(args, destination)

	cmd := exec.CommandContext(ctx, t.Path, args...)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		_ = os.Remove(destination)

		detail := strings.TrimSpace(stderr.String())

		if detail == "" {
			detail = err.Error()
		}

		return fmt.Errorf(
			"ffmpeg failed: %s",
			truncate(detail, 300),
		)
	}

	info, err := os.Stat(destination)

	if err != nil || info.Size() == 0 {
		_ = os.Remove(destination)

		return fmt.Errorf("ffmpeg produced no output")
	}

	return nil
}
