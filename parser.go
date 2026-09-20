package main

import (
	"regexp"
	"strings"
)

func ParseTracks(input string) []Track {
	var tracks []Track

	re := regexp.MustCompile(
		`(?s)link:\s*"([^"]+)".*?name:\s*"([^"]+)".*?artist:\s*"([^"]+)".*?duration:\s*"([^"]+)".*?thumb:\s*"([^"]+)".*?album:\s*"([^"]+)"`,
	)

	matches := re.FindAllStringSubmatch(input, -1)

	for i, match := range matches {
		if len(match) < 7 {
			continue
		}

		tracks = append(tracks, Track{
			Index:    i,
			Link:     match[1],
			Name:     cleanHTML(match[2]),
			Artist:   cleanHTML(match[3]),
			Duration: match[4],
			Thumb:    match[5],
			Album:    cleanHTML(match[6]),
		})
	}

	return tracks
}

func cleanHTML(value string) string {
	value = strings.ReplaceAll(value, "&amp;", "&")
	value = strings.ReplaceAll(value, "&quot;", `"`)
	value = strings.ReplaceAll(value, "&#39;", "'")

	return value
}
