package main

import (
	"fmt"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// Compiled once at startup rather than on every call. This was never a
// measurable cost (about 3 microseconds per track) but there is no
// reason to recompile a constant pattern inside a hot path.
var whitespaceRun = regexp.MustCompile(`\s+`)

// nonAlphanumeric matches everything that is not a Unicode letter or
// digit. Accented letters are kept, so "Âmadan" stays distinct.
var nonAlphanumeric = regexp.MustCompile(`[^\p{L}\p{N}]+`)

// canonicalKey reduces a name to just its letters and digits, lowercased.
//
// It exists to recognise files downloaded under the old naming scheme.
// Those names came from the remote service, which strips punctuation:
// "Rain (Lyrics By Ali Mo'allem)" was saved as "Rain Lyrics By Ali
// Moallem", and "Kayhan Kalhor & Mohammad-Reza Shajarian" lost its
// ampersand and hyphen. Comparing only letters and digits makes both
// spellings resolve to the same key.
func canonicalKey(value string) string {
	return nonAlphanumeric.ReplaceAllString(
		strings.ToLower(value),
		"",
	)
}

func safeFilename(name string) string {
	name = strings.TrimSpace(name)

	name = strings.ReplaceAll(name, "/", "_")
	name = strings.ReplaceAll(name, "\\", "_")
	name = strings.ReplaceAll(name, ":", "_")
	name = strings.ReplaceAll(name, "*", "_")
	name = strings.ReplaceAll(name, "?", "_")
	name = strings.ReplaceAll(name, `"`, "_")
	name = strings.ReplaceAll(name, "<", "_")
	name = strings.ReplaceAll(name, ">", "_")
	name = strings.ReplaceAll(name, "|", "_")

	name = whitespaceRun.ReplaceAllString(
		name,
		" ",
	)

	return strings.TrimSpace(name)
}

func extensionFromURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}

	// path.Base("") is ".", and filepath.Ext(".") is "." -- a bare dot
	// is not an extension, and returning one produces paths like
	// "Track." that match no file on disk.
	ext := cleanExtension(
		filepath.Ext(path.Base(u.Path)),
	)

	if ext != "" {
		return ext
	}

	fname := u.Query().Get("fname")

	if fname != "" {
		if ext := cleanExtension(filepath.Ext(fname)); ext != "" {
			return ext
		}
	}

	return ""
}

// cleanExtension normalises an extension, rejecting the degenerate
// results filepath.Ext can return for empty or dot-only names.
func cleanExtension(ext string) string {
	ext = strings.ToLower(strings.TrimSpace(ext))

	if ext == "" || ext == "." {
		return ""
	}

	return ext
}

// stemOf strips a single trailing extension from a file name.
//
// It is how a finished download on disk is matched back to the track
// that produced it, so that re-running the program does not fetch a file
// it already has.
func stemOf(name string) string {
	ext := filepath.Ext(name)

	if ext == "" {
		return name
	}

	return strings.TrimSuffix(name, ext)
}

func formatBytes(bytes int64) string {
	if bytes < 0 {
		return "unknown"
	}

	const unit = 1024

	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}

	div, exp := int64(unit), 0

	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}

	return fmt.Sprintf(
		"%.2f %ciB",
		float64(bytes)/float64(div),
		"KMGTPE"[exp],
	)
}

func truncate(value string, max int) string {
	if max <= 0 {
		return ""
	}

	runes := []rune(value)

	if len(runes) <= max {
		return value
	}

	if max <= 3 {
		return string(runes[:max])
	}

	return string(runes[:max-3]) + "..."
}
