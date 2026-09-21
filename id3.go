package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf16"
)

// ID3v2 SYLT writing.
//
// The remote service already tags the file with ffmpeg, so the job here
// is to add one frame to an existing tag without disturbing the rest.
// Anything unexpected in the tag makes this bail out rather than guess:
// a missing lyric is a non-event, a corrupted audio file is not.

const (
	id3HeaderSize = 10

	// Header flag bits.
	id3FlagUnsynchronisation = 0x80
	id3FlagExtendedHeader    = 0x40
	id3FlagFooterPresent     = 0x10

	// SYLT fields.
	syltEncodingUTF16WithBOM = 1
	syltTimestampMillis      = 2
	syltContentTypeLyrics    = 1
)

// LyricLine is one timed line.
type LyricLine struct {
	At   int64 // milliseconds from the start of the track
	Text string
}

// Lyrics carries both representations of the same words.
//
// Lines feed the SYLT frame, which is the correct place for timed
// lyrics. LRC is the original timestamped text, written to USLT as
// well, because in practice almost no player renders SYLT while most
// read USLT and many detect the timestamps in it.
type Lyrics struct {
	Lines []LyricLine
	LRC   string
}

type id3Frame struct {
	id   string
	full []byte // the complete frame, header included
}

// synchsafe encodes a size as ID3 does in the tag header: 7 bits per
// byte, top bit always clear.
func synchsafe(size int) []byte {
	return []byte{
		byte((size >> 21) & 0x7F),
		byte((size >> 14) & 0x7F),
		byte((size >> 7) & 0x7F),
		byte(size & 0x7F),
	}
}

func unsynchsafe(b []byte) int {
	return int(b[0]&0x7F)<<21 |
		int(b[1]&0x7F)<<14 |
		int(b[2]&0x7F)<<7 |
		int(b[3]&0x7F)
}

// encodeUTF16 returns a BOM-prefixed, null-terminated UTF-16 string.
//
// UTF-16 is used rather than UTF-8 because it is valid in both ID3v2.3
// and v2.4, and these tracks are frequently non-Latin.
func encodeUTF16(value string) []byte {
	units := utf16.Encode([]rune(value))

	out := make([]byte, 0, len(units)*2+4)
	out = append(out, 0xFF, 0xFE) // little-endian BOM

	for _, u := range units {
		out = append(out, byte(u), byte(u>>8))
	}

	return append(out, 0x00, 0x00) // terminator
}

// buildSYLT assembles a synchronised lyrics frame body.
func buildSYLT(lines []LyricLine, language string) []byte {
	if len(language) != 3 {
		language = "und"
	}

	body := []byte{syltEncodingUTF16WithBOM}
	body = append(body, language[0], language[1], language[2])
	body = append(body, syltTimestampMillis, syltContentTypeLyrics)

	// Content descriptor, empty.
	body = append(body, encodeUTF16("")...)

	for _, line := range lines {
		body = append(body, encodeUTF16(line.Text)...)

		at := line.At
		if at < 0 {
			at = 0
		}

		stamp := make([]byte, 4)
		binary.BigEndian.PutUint32(stamp, uint32(at))
		body = append(body, stamp...)
	}

	return body
}

// buildUSLT assembles an unsynchronised lyrics frame carrying the LRC
// text verbatim, timestamps included.
func buildUSLT(text, language string) []byte {
	if len(language) != 3 {
		language = "und"
	}

	body := []byte{syltEncodingUTF16WithBOM}
	body = append(body, language[0], language[1], language[2])

	// Content descriptor, empty.
	body = append(body, encodeUTF16("")...)

	// The text runs to the end of the frame, so no terminator.
	units := utf16.Encode([]rune(text))

	body = append(body, 0xFF, 0xFE)

	for _, u := range units {
		body = append(body, byte(u), byte(u>>8))
	}

	return body
}

// frameHeader builds a frame header for the given tag version.
// v2.3 sizes are plain big-endian; v2.4 sizes are synchsafe.
func frameHeader(id string, size int, version byte) []byte {
	out := make([]byte, 0, 10)
	out = append(out, id...)

	if version >= 4 {
		out = append(out, synchsafe(size)...)
	} else {
		raw := make([]byte, 4)
		binary.BigEndian.PutUint32(raw, uint32(size))
		out = append(out, raw...)
	}

	return append(out, 0x00, 0x00) // no frame flags
}

// parseFrames splits a tag body into frames, stopping at padding.
func parseFrames(body []byte, version byte) ([]id3Frame, error) {
	var frames []id3Frame

	for offset := 0; offset+10 <= len(body); {
		id := string(body[offset : offset+4])

		// A zero byte where a frame id should be means padding.
		if id[0] == 0 {
			break
		}

		for i := 0; i < 4; i++ {
			c := id[i]

			isUpper := c >= 'A' && c <= 'Z'
			isDigit := c >= '0' && c <= '9'

			if !isUpper && !isDigit {
				return nil, fmt.Errorf(
					"unreadable frame id %q at offset %d",
					id,
					offset,
				)
			}
		}

		var size int

		if version >= 4 {
			size = unsynchsafe(body[offset+4 : offset+8])
		} else {
			size = int(binary.BigEndian.Uint32(body[offset+4 : offset+8]))
		}

		if size < 0 || offset+10+size > len(body) {
			return nil, fmt.Errorf(
				"frame %q claims %d bytes, past the end of the tag",
				id,
				size,
			)
		}

		frames = append(frames, id3Frame{
			id:   id,
			full: body[offset : offset+10+size],
		})

		offset += 10 + size
	}

	return frames, nil
}

// EmbedSyncedLyrics adds a SYLT frame to the MP3 at path, replacing any
// SYLT already there so repeated runs stay idempotent.
//
// The file is rewritten through a temporary copy and renamed, so a
// failure at any point leaves the original untouched.
func EmbedSyncedLyrics(path string, lyrics Lyrics, language string) error {
	if len(lyrics.Lines) == 0 {
		return fmt.Errorf("no lyric lines to embed")
	}

	original, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read audio file: %w", err)
	}

	var (
		body    []byte
		audio   []byte
		version byte = 3
		hadTag       = false
	)

	if len(original) >= id3HeaderSize && string(original[0:3]) == "ID3" {
		version = original[3]
		flags := original[5]

		// These are rare in practice and would need the whole tag
		// decoded to edit safely. Not worth the risk for a lyric.
		if flags&id3FlagUnsynchronisation != 0 {
			return fmt.Errorf("tag uses unsynchronisation; left alone")
		}

		if flags&id3FlagExtendedHeader != 0 {
			return fmt.Errorf("tag has an extended header; left alone")
		}

		if flags&id3FlagFooterPresent != 0 {
			return fmt.Errorf("tag has a footer; left alone")
		}

		if version != 3 && version != 4 {
			return fmt.Errorf("unsupported ID3v2.%d tag", version)
		}

		size := unsynchsafe(original[6:10])

		if size < 0 || id3HeaderSize+size > len(original) {
			return fmt.Errorf("tag size %d is past the end of the file", size)
		}

		body = original[id3HeaderSize : id3HeaderSize+size]
		audio = original[id3HeaderSize+size:]
		hadTag = true
	} else {
		// No tag at all: start a fresh v2.3 one.
		audio = original
	}

	var kept []byte

	if hadTag {
		frames, err := parseFrames(body, version)
		if err != nil {
			return fmt.Errorf("parse existing tag: %w", err)
		}

		for _, f := range frames {
			// Drop any previous lyrics frames so re-running
			// replaces rather than accumulates.
			if f.id == "SYLT" || f.id == "USLT" {
				continue
			}

			kept = append(kept, f.full...)
		}
	}

	sylt := buildSYLT(lyrics.Lines, language)

	newBody := make([]byte, 0, len(kept)+len(sylt)+32)
	newBody = append(newBody, kept...)
	newBody = append(newBody, frameHeader("SYLT", len(sylt), version)...)
	newBody = append(newBody, sylt...)

	// USLT is what players actually read. Without it the lyrics are
	// present but invisible in nearly every app.
	if text := strings.TrimSpace(lyrics.LRC); text != "" {
		uslt := buildUSLT(lyrics.LRC, language)

		newBody = append(newBody, frameHeader("USLT", len(uslt), version)...)
		newBody = append(newBody, uslt...)
	}

	header := make([]byte, 0, id3HeaderSize)
	header = append(header, 'I', 'D', '3', version, 0x00, 0x00)
	header = append(header, synchsafe(len(newBody))...)

	tmp := path + ".id3tmp"

	out, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}

	writeErr := func() error {
		if _, err := out.Write(header); err != nil {
			return err
		}

		if _, err := out.Write(newBody); err != nil {
			return err
		}

		_, err := out.Write(audio)

		return err
	}()

	closeErr := out.Close()

	if writeErr == nil {
		writeErr = closeErr
	}

	if writeErr != nil {
		_ = os.Remove(tmp)

		return fmt.Errorf("write tagged file: %w", writeErr)
	}

	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)

		return fmt.Errorf("replace audio file: %w", err)
	}

	return nil
}

// HasSyncedLyrics reports whether the file already carries a SYLT frame,
// so a rerun can skip the lookup entirely.
func HasSyncedLyrics(path string) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}

	defer file.Close()

	header := make([]byte, id3HeaderSize)

	if _, err := io.ReadFull(file, header); err != nil {
		return false
	}

	if string(header[0:3]) != "ID3" {
		return false
	}

	version := header[3]

	if version != 3 && version != 4 {
		return false
	}

	if header[5]&(id3FlagUnsynchronisation|id3FlagExtendedHeader) != 0 {
		return false
	}

	size := unsynchsafe(header[6:10])

	if size <= 0 {
		return false
	}

	body := make([]byte, size)

	if _, err := io.ReadFull(file, body); err != nil {
		return false
	}

	frames, err := parseFrames(body, version)
	if err != nil {
		return false
	}

	for _, f := range frames {
		if f.id == "SYLT" || f.id == "USLT" {
			return true
		}
	}

	return false
}
