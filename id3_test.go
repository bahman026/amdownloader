package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"unicode/utf16"
)

// buildTestMP3 makes a file with a minimal ID3v2 tag followed by
// stand-in audio bytes.
func buildTestMP3(t *testing.T, version byte, flags byte) (string, []byte) {
	t.Helper()

	// One TIT2 frame, so we can prove existing frames survive.
	title := append([]byte{0x00}, []byte("Existing Title")...)

	frame := []byte("TIT2")

	if version >= 4 {
		frame = append(frame, synchsafe(len(title))...)
	} else {
		size := make([]byte, 4)
		binary.BigEndian.PutUint32(size, uint32(len(title)))
		frame = append(frame, size...)
	}

	frame = append(frame, 0x00, 0x00)
	frame = append(frame, title...)

	body := append(frame, make([]byte, 16)...) // padding

	header := []byte{'I', 'D', '3', version, 0x00, flags}
	header = append(header, synchsafe(len(body))...)

	audio := []byte{0xFF, 0xFB, 0x90, 0x00}
	audio = append(audio, bytes.Repeat([]byte{0x55}, 512)...)

	path := filepath.Join(t.TempDir(), "track.mp3")

	if err := os.WriteFile(path,
		append(append(header, body...), audio...), 0o644); err != nil {
		t.Fatal(err)
	}

	return path, audio
}

// readSYLT pulls the synchronised lyrics back out, so the round trip is
// verified rather than assumed.
func readSYLT(t *testing.T, path string) []LyricLine {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if string(raw[0:3]) != "ID3" {
		t.Fatal("no ID3 tag")
	}

	version := raw[3]
	size := unsynchsafe(raw[6:10])

	frames, err := parseFrames(raw[10:10+size], version)
	if err != nil {
		t.Fatalf("parse frames: %v", err)
	}

	for _, f := range frames {
		if f.id != "SYLT" {
			continue
		}

		body := f.full[10:]

		// encoding(1) language(3) format(1) type(1)
		at := 6

		decode := func() string {
			// BOM + UTF-16LE until a 0x0000 unit.
			at += 2 // BOM

			var units []uint16

			for at+1 < len(body) {
				u := uint16(body[at]) | uint16(body[at+1])<<8
				at += 2

				if u == 0 {
					break
				}

				units = append(units, u)
			}

			return string(utf16.Decode(units))
		}

		decode() // content descriptor

		var lines []LyricLine

		for at+4 <= len(body) {
			text := decode()

			if at+4 > len(body) {
				break
			}

			stamp := binary.BigEndian.Uint32(body[at : at+4])
			at += 4

			lines = append(lines, LyricLine{At: int64(stamp), Text: text})
		}

		return lines
	}

	return nil
}

func TestEmbedSyncedLyricsRoundTrip(t *testing.T) {
	for _, version := range []byte{3, 4} {
		path, audio := buildTestMP3(t, version, 0x00)

		want := []LyricLine{
			{At: 0, Text: "first placeholder line"},
			{At: 1500, Text: "second placeholder line"},
			{At: 90_000, Text: "متن آزمایشی"}, // non-Latin must survive
		}

		if err := EmbedSyncedLyrics(path, Lyrics{Lines: want}, "eng"); err != nil {
			t.Fatalf("v2.%d: embed failed: %v", version, err)
		}

		got := readSYLT(t, path)

		if len(got) != len(want) {
			t.Fatalf("v2.%d: got %d lines, want %d", version, len(got), len(want))
		}

		for i := range want {
			if got[i].At != want[i].At || got[i].Text != want[i].Text {
				t.Errorf("v2.%d line %d: got %+v want %+v",
					version, i, got[i], want[i])
			}
		}

		// The audio payload must be byte-identical.
		raw, _ := os.ReadFile(path)

		if !bytes.HasSuffix(raw, audio) {
			t.Errorf("v2.%d: audio payload was altered", version)
		}

		// And the pre-existing frame must still be there.
		if !bytes.Contains(raw, []byte("Existing Title")) {
			t.Errorf("v2.%d: existing TIT2 frame was lost", version)
		}
	}
}

func TestEmbedIsIdempotent(t *testing.T) {
	path, _ := buildTestMP3(t, 3, 0x00)

	lines := Lyrics{Lines: []LyricLine{{At: 10, Text: "placeholder"}}}

	for i := 0; i < 3; i++ {
		if err := EmbedSyncedLyrics(path, lines, "eng"); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}

	raw, _ := os.ReadFile(path)

	if n := bytes.Count(raw, []byte("SYLT")); n != 1 {
		t.Errorf("found %d SYLT frames after 3 passes, want 1", n)
	}

	if got := readSYLT(t, path); len(got) != 1 {
		t.Errorf("got %d lines, want 1", len(got))
	}
}

func TestHasSyncedLyrics(t *testing.T) {
	path, _ := buildTestMP3(t, 3, 0x00)

	if HasSyncedLyrics(path) {
		t.Error("reported lyrics before any were added")
	}

	EmbedSyncedLyrics(path, Lyrics{Lines: []LyricLine{{At: 0, Text: "x"}}}, "eng")

	if !HasSyncedLyrics(path) {
		t.Error("did not see the frame just written")
	}
}

// An unusual tag must be left strictly alone rather than guessed at.
func TestEmbedRefusesUnsupportedTags(t *testing.T) {
	for name, flags := range map[string]byte{
		"unsynchronisation": id3FlagUnsynchronisation,
		"extended header":   id3FlagExtendedHeader,
		"footer":            id3FlagFooterPresent,
	} {
		path, _ := buildTestMP3(t, 3, flags)

		before, _ := os.ReadFile(path)

		err := EmbedSyncedLyrics(path, Lyrics{Lines: []LyricLine{{At: 0, Text: "x"}}}, "eng")

		if err == nil {
			t.Errorf("%s: expected a refusal", name)
		}

		after, _ := os.ReadFile(path)

		if !bytes.Equal(before, after) {
			t.Errorf("%s: file was modified despite refusing", name)
		}
	}
}

// A file with no tag at all still gets one.
func TestEmbedIntoUntaggedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bare.mp3")
	audio := bytes.Repeat([]byte{0x41}, 256)

	os.WriteFile(path, audio, 0o644)

	if err := EmbedSyncedLyrics(path,
		Lyrics{Lines: []LyricLine{{At: 5, Text: "placeholder"}}}, "eng"); err != nil {
		t.Fatalf("embed failed: %v", err)
	}

	raw, _ := os.ReadFile(path)

	if !bytes.HasSuffix(raw, audio) {
		t.Error("audio payload was altered")
	}

	if got := readSYLT(t, path); len(got) != 1 || got[0].At != 5 {
		t.Errorf("round trip failed: %+v", got)
	}
}

func TestSynchsafeRoundTrip(t *testing.T) {
	for _, n := range []int{0, 1, 127, 128, 255, 100000, 268435455} {
		if got := unsynchsafe(synchsafe(n)); got != n {
			t.Errorf("synchsafe(%d) round-tripped to %d", n, got)
		}
	}
}

// USLT must be written alongside SYLT.
//
// SYLT is the correct frame for timed lyrics, but almost no player
// renders it. USLT carrying the same timestamped text is what actually
// shows up in an app.
func TestUSLTIsWrittenAlongsideSYLT(t *testing.T) {
	path, audio := buildTestMP3(t, 3, 0x00)

	lrc := "[00:01.00]placeholder one\n[00:05.50]placeholder two\n"

	if err := EmbedSyncedLyrics(path, Lyrics{
		Lines: ParseLRC(lrc),
		LRC:   lrc,
	}, "eng"); err != nil {
		t.Fatal(err)
	}

	raw, _ := os.ReadFile(path)

	for _, id := range []string{"SYLT", "USLT"} {
		if n := bytes.Count(raw, []byte(id)); n != 1 {
			t.Errorf("%s appears %d times, want 1", id, n)
		}
	}

	// The USLT payload is UTF-16, so the timestamp text appears with
	// a null byte between each character.
	wide := []byte{}
	for _, c := range []byte("[00:01.00]") {
		wide = append(wide, c, 0x00)
	}

	if !bytes.Contains(raw, wide) {
		t.Error("USLT does not carry the LRC timestamps")
	}

	if !bytes.HasSuffix(raw, audio) {
		t.Error("audio payload was altered")
	}

	if !bytes.Contains(raw, []byte("Existing Title")) {
		t.Error("existing TIT2 frame was lost")
	}
}

// Re-running must replace both lyric frames, not stack them.
func TestBothLyricFramesAreIdempotent(t *testing.T) {
	path, _ := buildTestMP3(t, 3, 0x00)

	lrc := "[00:02.00]placeholder\n"

	for i := 0; i < 3; i++ {
		if err := EmbedSyncedLyrics(path, Lyrics{
			Lines: ParseLRC(lrc), LRC: lrc,
		}, "eng"); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}

	raw, _ := os.ReadFile(path)

	for _, id := range []string{"SYLT", "USLT"} {
		if n := bytes.Count(raw, []byte(id)); n != 1 {
			t.Errorf("%s appears %d times after 3 passes, want 1", id, n)
		}
	}
}

// Lyrics with no raw LRC still write SYLT, just without USLT.
func TestSYLTWithoutRawLRC(t *testing.T) {
	path, _ := buildTestMP3(t, 3, 0x00)

	if err := EmbedSyncedLyrics(path, Lyrics{
		Lines: []LyricLine{{At: 0, Text: "placeholder"}},
	}, "eng"); err != nil {
		t.Fatal(err)
	}

	raw, _ := os.ReadFile(path)

	if bytes.Count(raw, []byte("SYLT")) != 1 {
		t.Error("SYLT missing")
	}

	if bytes.Count(raw, []byte("USLT")) != 0 {
		t.Error("USLT written with no LRC text to put in it")
	}
}
