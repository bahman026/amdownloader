package main

import (
	"fmt"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

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

	name = regexp.MustCompile(`\s+`).ReplaceAllString(
		name,
		" ",
	)

	return name
}

func extensionFromURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}

	ext := filepath.Ext(
		path.Base(u.Path),
	)

	if ext != "" {
		return strings.ToLower(ext)
	}

	fname := u.Query().Get("fname")

	if fname != "" {
		ext = filepath.Ext(fname)

		if ext != "" {
			return strings.ToLower(ext)
		}
	}

	return ""
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
	runes := []rune(value)

	if len(runes) <= max {
		return value
	}

	return string(runes[:max-3]) + "..."
}
