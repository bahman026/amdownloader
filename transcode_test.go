package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeFFmpeg writes a stub binary that records the arguments it was
// given and produces a small ID3-tagged file at the output path.
//
// mode: "ok" succeeds, "fail" exits non-zero, "empty" writes nothing.
func fakeFFmpeg(t *testing.T, mode string) (bin string, argsFile string) {
	t.Helper()

	dir := t.TempDir()
	bin = filepath.Join(dir, "ffmpeg")
	argsFile = filepath.Join(dir, "args.txt")

	var body string

	switch mode {
	case "fail":
		body = `echo "simulated encoder failure" >&2
exit 1`
	case "empty":
		body = `for a in "$@"; do :; done
: > "${@: -1}"
exit 0`
	default:
		// A 10-byte empty ID3v2.3 header followed by filler, so the
		// result is something the tagger can parse.
		body = `out="${@: -1}"
printf 'ID3\x03\x00\x00\x00\x00\x00\x00' > "$out"
printf 'AUDIOAUDIOAUDIO' >> "$out"
exit 0`
	}

	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsFile + "\n" + body + "\n"

	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	return bin, argsFile
}

func readArgs(t *testing.T, path string) string {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("stub was never invoked: %v", err)
	}

	return string(raw)
}

func TestTranscoderCommand(t *testing.T) {
	bin, argsFile := fakeFFmpeg(t, "ok")

	dir := t.TempDir()
	src := filepath.Join(dir, "in.m4a")
	cover := filepath.Join(dir, "cover.jpg")
	out := filepath.Join(dir, "out.mp3")

	os.WriteFile(src, []byte("source"), 0o644)
	os.WriteFile(cover, []byte("image"), 0o644)

	tr := &Transcoder{Bitrate: 320, Path: bin}

	err := tr.ToMP3(context.Background(), src, out, Track{
		Name: "Some Title", Artist: "Some Artist", Album: "Some Album",
	}, cover)

	if err != nil {
		t.Fatalf("transcode failed: %v", err)
	}

	args := readArgs(t, argsFile)

	for _, want := range []string{
		"-b:a", "320k",
		"libmp3lame",
		"-id3v2_version", "3", // so the SYLT frame lands in a tag players read
		"title=Some Title",
		"artist=Some Artist",
		"album=Some Album",
		"attached_pic", // artwork is attached, not muxed as a video stream
		cover,
		src,
		out,
	} {
		if !strings.Contains(args, want) {
			t.Errorf("command is missing %q\ngot:\n%s", want, args)
		}
	}

	if _, err := os.Stat(out); err != nil {
		t.Errorf("no output produced: %v", err)
	}
}

func TestTranscoderBitrateIsHonoured(t *testing.T) {
	for _, rate := range validBitrates {
		bin, argsFile := fakeFFmpeg(t, "ok")

		dir := t.TempDir()
		src := filepath.Join(dir, "in.m4a")
		os.WriteFile(src, []byte("x"), 0o644)

		tr := &Transcoder{Bitrate: rate, Path: bin}

		if err := tr.ToMP3(context.Background(), src,
			filepath.Join(dir, "out.mp3"), Track{Name: "T"}, ""); err != nil {
			t.Fatal(err)
		}

		if !strings.Contains(readArgs(t, argsFile), "-b:a\n"+itoa(rate)+"k") {
			t.Errorf("bitrate %d not passed through", rate)
		}
	}

	// Anything not on the list falls back rather than being passed on.
	if got := (&Transcoder{Bitrate: 999}).bitrate(); got != 320 {
		t.Errorf("invalid bitrate became %d, want the 320 fallback", got)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}

	var out []byte

	for n > 0 {
		out = append([]byte{byte('0' + n%10)}, out...)
		n /= 10
	}

	return string(out)
}

// Without artwork the image inputs must be absent entirely, not empty.
func TestTranscoderWithoutArtwork(t *testing.T) {
	bin, argsFile := fakeFFmpeg(t, "ok")

	dir := t.TempDir()
	src := filepath.Join(dir, "in.m4a")
	os.WriteFile(src, []byte("x"), 0o644)

	tr := &Transcoder{Bitrate: 256, Path: bin}

	if err := tr.ToMP3(context.Background(), src,
		filepath.Join(dir, "out.mp3"), Track{Name: "T"}, ""); err != nil {
		t.Fatal(err)
	}

	args := readArgs(t, argsFile)

	for _, unwanted := range []string{"attached_pic", "1:v:0"} {
		if strings.Contains(args, unwanted) {
			t.Errorf("artwork flag %q present with no cover", unwanted)
		}
	}
}

// A failed encode must leave nothing behind.
func TestTranscoderFailureLeavesNoFile(t *testing.T) {
	for _, mode := range []string{"fail", "empty"} {
		bin, _ := fakeFFmpeg(t, mode)

		dir := t.TempDir()
		src := filepath.Join(dir, "in.m4a")
		out := filepath.Join(dir, "out.mp3")

		os.WriteFile(src, []byte("x"), 0o644)

		tr := &Transcoder{Bitrate: 320, Path: bin}

		err := tr.ToMP3(context.Background(), src, out, Track{Name: "T"}, "")

		if err == nil {
			t.Errorf("%s: expected an error", mode)
		}

		if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
			t.Errorf("%s: output file was left behind", mode)
		}
	}
}

// A missing encoder must say how to fix it.
func TestLookupFFmpegExplainsItself(t *testing.T) {
	_, err := LookupFFmpeg("/definitely/not/here/ffmpeg")

	if err == nil {
		t.Fatal("expected an error for a bad explicit path")
	}

	tr := &Transcoder{Path: "/definitely/not/here/ffmpeg"}

	if err := tr.Available(); err == nil {
		t.Error("Available accepted a nonexistent binary")
	}
}

// The whole pipeline in transcode mode: the service's transcode stage
// must be skipped entirely and the AAC source used instead.
func TestPipelineTranscodeModeSkipsServiceEncode(t *testing.T) {
	svc := newFakeService(t, 0)
	dir := t.TempDir()

	bin, _ := fakeFFmpeg(t, "ok")

	processor, _ := newTestPipeline(t, svc, dir)
	processor.AudioMode = audioModeTranscode
	processor.Transcoder = &Transcoder{Bitrate: 320, Path: bin}

	results := processor.Process(context.Background(), makeTracks(4))

	for _, r := range results {
		if r.Error != nil {
			t.Errorf("track %d failed: %v", r.Track.Index, r.Error)
		}
	}

	// The expensive remote stage is never touched.
	if got := svc.saveHits.Load(); got != 0 {
		t.Errorf("saveid3.php was called %d times; transcode mode must skip it", got)
	}

	if got := svc.savedHits.Load(); got != 0 {
		t.Errorf("the service's generated file was fetched %d times", got)
	}

	// The AAC source is what gets downloaded.
	if got := svc.sourceHits.Load(); got != 4 {
		t.Errorf("source fetched %d times, want 4", got)
	}

	files := listFiles(t, dir)

	if len(files) != 4 {
		t.Errorf("got %d files, want 4: %v", len(files), files)
	}

	for _, f := range files {
		if !strings.HasSuffix(f, ".mp3") {
			t.Errorf("unexpected leftover: %s", f)
		}
	}
}

// A broken encoder fails the track but leaves no debris.
func TestPipelineTranscodeFailureIsClean(t *testing.T) {
	svc := newFakeService(t, 0)
	dir := t.TempDir()

	bin, _ := fakeFFmpeg(t, "fail")

	processor, _ := newTestPipeline(t, svc, dir)
	processor.AudioMode = audioModeTranscode
	processor.Transcoder = &Transcoder{Bitrate: 320, Path: bin}
	processor.Log = NewLogger(io.Discard)

	results := processor.Process(context.Background(), makeTracks(2))

	for _, r := range results {
		if r.Error == nil {
			t.Errorf("track %d reported success despite the encoder failing", r.Track.Index)
		}
	}

	if files := listFiles(t, dir); len(files) != 0 {
		t.Errorf("scratch files left behind: %v", files)
	}
}

// The output format must be named explicitly.
//
// Output goes to a .part file first, and ffmpeg picks its muxer from
// the extension unless told otherwise. Without -f it fails with
// "Unable to choose an output format".
func TestTranscoderNamesTheOutputFormat(t *testing.T) {
	bin, argsFile := fakeFFmpeg(t, "ok")

	dir := t.TempDir()
	src := filepath.Join(dir, "in.m4a")
	os.WriteFile(src, []byte("x"), 0o644)

	tr := &Transcoder{Bitrate: 320, Path: bin}

	if err := tr.ToMP3(context.Background(), src,
		filepath.Join(dir, "out.mp3.part"), Track{Name: "T"}, ""); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(readArgs(t, argsFile), "-f\nmp3") {
		t.Error("output format is not named; ffmpeg would guess from the extension")
	}
}

// Exercises the real encoder against a .part output path.
//
// The stub cannot catch format-inference problems because it ignores
// the filename entirely, which is exactly how the missing -f slipped
// through. Skipped where ffmpeg is unavailable.
func TestRealFFmpegWritesToPartPath(t *testing.T) {
	tr := &Transcoder{Bitrate: 320}

	if err := tr.Available(); err != nil {
		t.Skip("ffmpeg not available:", err)
	}

	dir := t.TempDir()
	src := filepath.Join(dir, "source.m4a")

	// Two seconds of silence, built by ffmpeg so the test needs no
	// audio fixture of its own.
	build := exec.Command(tr.Path,
		"-nostdin", "-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "anullsrc=r=44100:cl=stereo",
		"-t", "2", "-c:a", "aac", src)

	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("could not build a test source: %v %s", err, out)
	}

	// The real pipeline writes here before renaming into place.
	out := filepath.Join(dir, "01 - Track - Artist.mp3.part")

	if err := tr.ToMP3(context.Background(), src, out, Track{
		Name: "Track", Artist: "Artist", Album: "Album",
	}, ""); err != nil {
		t.Fatalf("real ffmpeg failed on a .part output: %v", err)
	}

	info, err := os.Stat(out)
	if err != nil {
		t.Fatalf("no output produced: %v", err)
	}

	if info.Size() == 0 {
		t.Fatal("output is empty")
	}

	// It must really be an MP3 with an ID3v2 tag, not whatever the
	// extension suggested.
	head, _ := os.ReadFile(out)

	if len(head) < 3 || string(head[0:3]) != "ID3" {
		t.Errorf("output does not start with an ID3 tag: % x", head[:min(8, len(head))])
	}

	t.Logf("real ffmpeg wrote %.1f KiB to a .part path", float64(info.Size())/1024)
}

func min(a, b int) int {
	if a < b {
		return a
	}

	return b
}
