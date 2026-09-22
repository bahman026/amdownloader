package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
)

// settingsFile is where persisted settings live, alongside
// album_details and downloads in the working directory.
const settingsFile = "settings.json"

// Settings holds the user-configurable behaviour of the program.
//
// Every field here actually changes what the program does. Audio format
// and bitrate are deliberately absent: the service hands back a fixed
// .m4a and transcodes it to a fixed 128 kbps MP3, taking no bitrate
// parameter at all. A knob for either would silently do nothing.
type Settings struct {
	// OutputDir is the base directory downloads are written under.
	OutputDir string `json:"output_dir"`

	ResolveConcurrency  int `json:"resolve_concurrency"`
	SaveConcurrency     int `json:"save_concurrency"`
	DownloadConcurrency int `json:"download_concurrency"`

	ResolveAttempts  int `json:"resolve_attempts"`
	SaveAttempts     int `json:"save_attempts"`
	DownloadAttempts int `json:"download_attempts"`

	// SkipExisting skips tracks already present in the output
	// directory, before any network request is made.
	SkipExisting bool `json:"skip_existing"`

	// DetectLegacyNames also recognises files saved under the older
	// naming scheme when deciding what is already downloaded.
	DetectLegacyNames bool `json:"detect_legacy_names"`

	// Archive records every finished track in ArchiveFile and skips
	// anything already listed there.
	//
	// Unlike SkipExisting, which can only see the folder this run
	// writes to and the name this run would give the file, the archive
	// is keyed by the Apple Music track id. It therefore survives a
	// playlist being reordered, the same song turning up in a second
	// playlist, and a track already fetched as a single.
	Archive bool `json:"archive"`

	// ArchiveFile is where that record lives.
	ArchiveFile string `json:"archive_file"`

	// Lyrics controls whether synchronised lyrics are looked up on
	// LRCLIB and embedded into each finished MP3.
	Lyrics bool `json:"lyrics"`

	// LyricsLanguage is the 3-letter code recorded in the ID3 frame.
	LyricsLanguage string `json:"lyrics_language"`

	LyricsConcurrency int `json:"lyrics_concurrency"`

	// AudioMode selects where the MP3 comes from.
	//
	//   "service"   the remote service transcodes it, fixed 128 kbps
	//   "transcode" download the ~273 kbps AAC source and encode it
	//               here, at TranscodeBitrate
	AudioMode string `json:"audio_mode"`

	// TranscodeBitrate is the kbps of the local encode. Unlike the
	// removed "quality" setting, this one reaches a real encoder and
	// genuinely changes the output.
	TranscodeBitrate int `json:"transcode_bitrate"`

	// FFmpegPath overrides binary discovery. Empty means use PATH.
	FFmpegPath string `json:"ffmpeg_path"`

	// SearchCountry is the storefront searched when a query is given
	// instead of a link. Results, and the links behind them, differ
	// between storefronts.
	SearchCountry string `json:"search_country"`

	// SearchLimit is how many matches a search offers to choose from.
	SearchLimit int `json:"search_limit"`
}

const (
	defaultSearchCountry = "us"
	defaultSearchLimit   = 20
)

const (
	audioModeService   = "service"
	audioModeTranscode = "transcode"
)

func DefaultSettings() *Settings {
	return &Settings{
		OutputDir:           "./downloads",
		ResolveConcurrency:  defaultResolveConcurrency,
		SaveConcurrency:     defaultSaveConcurrency,
		DownloadConcurrency: defaultDownloadConcurrency,
		ResolveAttempts:     3,
		SaveAttempts:        2,
		DownloadAttempts:    3,
		SkipExisting:        true,
		DetectLegacyNames:   true,
		Archive:             true,
		ArchiveFile:         defaultArchiveFile,
		Lyrics:              true,
		LyricsLanguage:      "und",
		LyricsConcurrency:   4,
		AudioMode:           audioModeService,
		TranscodeBitrate:    320,
		SearchCountry:       defaultSearchCountry,
		SearchLimit:         defaultSearchLimit,
	}
}

// LoadSettings reads settings.json if present, falling back to defaults,
// then applies any environment overrides.
//
// Precedence, lowest to highest: defaults, settings.json, environment.
func LoadSettings() (*Settings, []string) {
	settings := DefaultSettings()

	var notes []string

	raw, err := os.ReadFile(settingsFile)

	switch {

	case err == nil:
		notes = append(notes, unknownKeyNotes(raw)...)

		if err := json.Unmarshal(raw, settings); err != nil {
			notes = append(notes, fmt.Sprintf(
				"WARNING: %s is not valid JSON (%v); using defaults",
				settingsFile,
				err,
			))

			settings = DefaultSettings()
		}

	case !os.IsNotExist(err):
		notes = append(notes, fmt.Sprintf(
			"WARNING: could not read %s: %v",
			settingsFile,
			err,
		))
	}

	// Environment overrides win, so a one-off run can differ from the
	// saved configuration without editing it.
	settings.ResolveConcurrency = envInt(
		"MEDIA_CLI_RESOLVE_CONCURRENCY", settings.ResolveConcurrency)

	settings.SaveConcurrency = envInt(
		"MEDIA_CLI_SAVE_CONCURRENCY", settings.SaveConcurrency)

	settings.DownloadConcurrency = envInt(
		"MEDIA_CLI_DOWNLOAD_CONCURRENCY", settings.DownloadConcurrency)

	settings.LyricsConcurrency = envInt(
		"MEDIA_CLI_LYRICS_CONCURRENCY", settings.LyricsConcurrency)

	if dir := strings.TrimSpace(os.Getenv("MEDIA_CLI_OUTPUT_DIR")); dir != "" {
		settings.OutputDir = dir
	}

	if file := strings.TrimSpace(
		os.Getenv("MEDIA_CLI_ARCHIVE_FILE"),
	); file != "" {
		settings.ArchiveFile = file
	}

	if country := strings.TrimSpace(
		os.Getenv("MEDIA_CLI_SEARCH_COUNTRY"),
	); country != "" {
		settings.SearchCountry = country
	}

	settings.SearchLimit = envInt(
		"MEDIA_CLI_SEARCH_LIMIT", settings.SearchLimit)

	notes = append(notes, settings.normalize()...)

	return settings, notes
}

// normalize clamps anything out of range and reports what it changed.
func (s *Settings) normalize() []string {
	var notes []string

	if strings.TrimSpace(s.OutputDir) == "" {
		s.OutputDir = "./downloads"
	}

	if len(s.LyricsLanguage) != 3 {
		s.LyricsLanguage = "und"
	}

	if strings.TrimSpace(s.ArchiveFile) == "" {
		s.ArchiveFile = defaultArchiveFile
	}

	s.SearchCountry = strings.ToLower(strings.TrimSpace(s.SearchCountry))

	if len(s.SearchCountry) != 2 {
		if s.SearchCountry != "" {
			notes = append(notes, fmt.Sprintf(
				"WARNING: search_country %q is not a 2-letter code; using %s",
				s.SearchCountry, defaultSearchCountry,
			))
		}

		s.SearchCountry = defaultSearchCountry
	}

	if s.AudioMode != audioModeService && s.AudioMode != audioModeTranscode {
		if strings.TrimSpace(s.AudioMode) != "" {
			notes = append(notes, fmt.Sprintf(
				"WARNING: audio_mode %q is not %s or %s; using %s",
				s.AudioMode, audioModeService, audioModeTranscode,
				audioModeService,
			))
		}

		s.AudioMode = audioModeService
	}

	if !containsInt(validBitrates, s.TranscodeBitrate) {
		notes = append(notes, fmt.Sprintf(
			"WARNING: transcode_bitrate %d is not one of %v; using 320",
			s.TranscodeBitrate, validBitrates,
		))

		s.TranscodeBitrate = 320
	}

	clamp := func(name string, value *int, min, max int) {
		if *value < min || *value > max {
			original := *value

			if *value < min {
				*value = min
			} else {
				*value = max
			}

			notes = append(notes, fmt.Sprintf(
				"WARNING: %s %d is outside %d-%d; using %d",
				name, original, min, max, *value,
			))
		}
	}

	clamp("resolve_concurrency", &s.ResolveConcurrency, 1, 16)
	clamp("save_concurrency", &s.SaveConcurrency, 1, 16)
	clamp("download_concurrency", &s.DownloadConcurrency, 1, 16)
	clamp("lyrics_concurrency", &s.LyricsConcurrency, 1, 16)
	clamp("resolve_attempts", &s.ResolveAttempts, 1, 10)
	clamp("save_attempts", &s.SaveAttempts, 1, 10)
	clamp("download_attempts", &s.DownloadAttempts, 1, 10)
	clamp("search_limit", &s.SearchLimit, 1, 50)

	return notes
}

// validate rejects out-of-range values outright.
//
// normalize clamps, which is right when loading a file someone edited by
// hand. An explicit `settings set` should not silently store something
// other than what was asked for, so that path validates instead.
func (s *Settings) validate() error {
	if strings.TrimSpace(s.OutputDir) == "" {
		return fmt.Errorf("output_dir cannot be empty")
	}

	if strings.TrimSpace(s.ArchiveFile) == "" {
		return fmt.Errorf("archive_file cannot be empty")
	}

	if len(s.LyricsLanguage) != 3 {
		return fmt.Errorf(
			"lyrics_language must be a 3-letter code, got %q",
			s.LyricsLanguage,
		)
	}

	if len(strings.TrimSpace(s.SearchCountry)) != 2 {
		return fmt.Errorf(
			"search_country must be a 2-letter code such as us, got %q",
			s.SearchCountry,
		)
	}

	if s.AudioMode != audioModeService && s.AudioMode != audioModeTranscode {
		return fmt.Errorf(
			"audio_mode must be %q or %q, got %q",
			audioModeService, audioModeTranscode, s.AudioMode,
		)
	}

	if !containsInt(validBitrates, s.TranscodeBitrate) {
		return fmt.Errorf(
			"transcode_bitrate must be one of %v",
			validBitrates,
		)
	}

	ranges := []struct {
		name     string
		value    int
		min, max int
	}{
		{"resolve_concurrency", s.ResolveConcurrency, 1, 16},
		{"save_concurrency", s.SaveConcurrency, 1, 16},
		{"download_concurrency", s.DownloadConcurrency, 1, 16},
		{"lyrics_concurrency", s.LyricsConcurrency, 1, 16},
		{"resolve_attempts", s.ResolveAttempts, 1, 10},
		{"save_attempts", s.SaveAttempts, 1, 10},
		{"download_attempts", s.DownloadAttempts, 1, 10},
		{"search_limit", s.SearchLimit, 1, 50},
	}

	for _, r := range ranges {
		if r.value < r.min || r.value > r.max {
			return fmt.Errorf(
				"%s must be between %d and %d, got %d",
				r.name, r.min, r.max, r.value,
			)
		}
	}

	return nil
}

// obsoleteKeys are settings that once existed and no longer do, with
// the reason. A stale key sitting in the file silently doing nothing is
// how someone ends up believing a setting is applied when it is not.
var obsoleteKeys = map[string]string{
	"quality": "removed: the service ignores it and always returns " +
		"128 kbps. Use audio_mode=transcode with transcode_bitrate.",
	"format": "removed: the output format is not selectable.",
}

// unknownKeyNotes reports keys in the file that the program does not
// use, so they cannot quietly imply an effect they do not have.
func unknownKeyNotes(raw []byte) []string {
	var present map[string]json.RawMessage

	if err := json.Unmarshal(raw, &present); err != nil {
		return nil
	}

	known := settingFields()

	var notes []string

	for key := range present {
		if _, ok := known[key]; ok {
			continue
		}

		if reason, ok := obsoleteKeys[key]; ok {
			notes = append(notes, fmt.Sprintf(
				"NOTE: %q in %s is %s",
				key, settingsFile, reason,
			))

			continue
		}

		notes = append(notes, fmt.Sprintf(
			"NOTE: %q in %s is not a known setting and is ignored",
			key, settingsFile,
		))
	}

	sort.Strings(notes)

	return notes
}

func containsInt(values []int, want int) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}

	return false
}

func (s *Settings) Save() error {
	encoded, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}

	encoded = append(encoded, '\n')

	return os.WriteFile(settingsFile, encoded, 0o644)
}

// field describes one setting for the `settings` command.
type field struct {
	get  func(*Settings) string
	set  func(*Settings, string) error
	help string
}

func settingFields() map[string]field {
	setInt := func(apply func(*Settings, int)) func(*Settings, string) error {
		return func(s *Settings, raw string) error {
			value, err := strconv.Atoi(strings.TrimSpace(raw))
			if err != nil {
				return fmt.Errorf("expected a number, got %q", raw)
			}

			apply(s, value)

			return nil
		}
	}

	setBool := func(apply func(*Settings, bool)) func(*Settings, string) error {
		return func(s *Settings, raw string) error {
			value, err := strconv.ParseBool(strings.TrimSpace(raw))
			if err != nil {
				return fmt.Errorf(
					"expected true or false, got %q", raw)
			}

			apply(s, value)

			return nil
		}
	}

	return map[string]field{
		"output_dir": {
			get: func(s *Settings) string { return s.OutputDir },
			set: func(s *Settings, raw string) error {
				raw = strings.TrimSpace(raw)
				if raw == "" {
					return fmt.Errorf("output_dir cannot be empty")
				}
				s.OutputDir = raw
				return nil
			},
			help: "base directory downloads are written under",
		},
		"resolve_concurrency": {
			get:  func(s *Settings) string { return strconv.Itoa(s.ResolveConcurrency) },
			set:  setInt(func(s *Settings, v int) { s.ResolveConcurrency = v }),
			help: "parallel resolve requests (1-16)",
		},
		"save_concurrency": {
			get:  func(s *Settings) string { return strconv.Itoa(s.SaveConcurrency) },
			set:  setInt(func(s *Settings, v int) { s.SaveConcurrency = v }),
			help: "parallel MP3 generation requests (1-16)",
		},
		"download_concurrency": {
			get:  func(s *Settings) string { return strconv.Itoa(s.DownloadConcurrency) },
			set:  setInt(func(s *Settings, v int) { s.DownloadConcurrency = v }),
			help: "parallel file transfers (1-16)",
		},
		"resolve_attempts": {
			get:  func(s *Settings) string { return strconv.Itoa(s.ResolveAttempts) },
			set:  setInt(func(s *Settings, v int) { s.ResolveAttempts = v }),
			help: "attempts per track at the resolve stage (1-10)",
		},
		"save_attempts": {
			get:  func(s *Settings) string { return strconv.Itoa(s.SaveAttempts) },
			set:  setInt(func(s *Settings, v int) { s.SaveAttempts = v }),
			help: "attempts at MP3 generation; kept low, it transcodes (1-10)",
		},
		"download_attempts": {
			get:  func(s *Settings) string { return strconv.Itoa(s.DownloadAttempts) },
			set:  setInt(func(s *Settings, v int) { s.DownloadAttempts = v }),
			help: "attempts per file transfer (1-10)",
		},
		"skip_existing": {
			get:  func(s *Settings) string { return strconv.FormatBool(s.SkipExisting) },
			set:  setBool(func(s *Settings, v bool) { s.SkipExisting = v }),
			help: "skip tracks already on disk, before any request",
		},
		"archive": {
			get:  func(s *Settings) string { return strconv.FormatBool(s.Archive) },
			set:  setBool(func(s *Settings, v bool) { s.Archive = v }),
			help: "record finished tracks and skip anything already recorded",
		},
		"archive_file": {
			get: func(s *Settings) string { return s.ArchiveFile },
			set: func(s *Settings, raw string) error {
				raw = strings.TrimSpace(raw)
				if raw == "" {
					return fmt.Errorf("archive_file cannot be empty")
				}
				s.ArchiveFile = raw
				return nil
			},
			help: "file the record of finished tracks is kept in",
		},
		"detect_legacy_names": {
			get:  func(s *Settings) string { return strconv.FormatBool(s.DetectLegacyNames) },
			set:  setBool(func(s *Settings, v bool) { s.DetectLegacyNames = v }),
			help: "also recognise files saved under the old naming scheme",
		},
		"lyrics": {
			get:  func(s *Settings) string { return strconv.FormatBool(s.Lyrics) },
			set:  setBool(func(s *Settings, v bool) { s.Lyrics = v }),
			help: "look up synced lyrics on LRCLIB and embed them",
		},
		"lyrics_language": {
			get: func(s *Settings) string { return s.LyricsLanguage },
			set: func(s *Settings, raw string) error {
				raw = strings.ToLower(strings.TrimSpace(raw))
				if len(raw) != 3 {
					return fmt.Errorf(
						"expected a 3-letter code such as eng, got %q", raw)
				}
				s.LyricsLanguage = raw
				return nil
			},
			help: "3-letter language code recorded in the lyrics frame",
		},
		"audio_mode": {
			get: func(s *Settings) string { return s.AudioMode },
			set: func(s *Settings, raw string) error {
				raw = strings.ToLower(strings.TrimSpace(raw))
				if raw != audioModeService && raw != audioModeTranscode {
					return fmt.Errorf(
						"expected %q or %q, got %q",
						audioModeService, audioModeTranscode, raw)
				}
				s.AudioMode = raw
				return nil
			},
			help: "service (128k, remote) or transcode (local encode of the AAC source)",
		},
		"transcode_bitrate": {
			get:  func(s *Settings) string { return strconv.Itoa(s.TranscodeBitrate) },
			set:  setInt(func(s *Settings, v int) { s.TranscodeBitrate = v }),
			help: "kbps for the local encode (128, 192, 256 or 320)",
		},
		"ffmpeg_path": {
			get: func(s *Settings) string { return s.FFmpegPath },
			set: func(s *Settings, raw string) error {
				s.FFmpegPath = strings.TrimSpace(raw)
				return nil
			},
			help: "path to ffmpeg; empty means look it up on PATH",
		},
		"search_country": {
			get: func(s *Settings) string { return s.SearchCountry },
			set: func(s *Settings, raw string) error {
				raw = strings.ToLower(strings.TrimSpace(raw))
				if len(raw) != 2 {
					return fmt.Errorf(
						"expected a 2-letter code such as us, got %q", raw)
				}
				s.SearchCountry = raw
				return nil
			},
			help: "storefront searched when a query is given instead of a link",
		},
		"search_limit": {
			get:  func(s *Settings) string { return strconv.Itoa(s.SearchLimit) },
			set:  setInt(func(s *Settings, v int) { s.SearchLimit = v }),
			help: "how many search results to offer (1-50)",
		},
		"lyrics_concurrency": {
			get:  func(s *Settings) string { return strconv.Itoa(s.LyricsConcurrency) },
			set:  setInt(func(s *Settings, v int) { s.LyricsConcurrency = v }),
			help: "parallel lyrics lookups (1-16)",
		},
	}
}

func sortedFieldNames() []string {
	fields := settingFields()

	names := make([]string, 0, len(fields))

	for name := range fields {
		names = append(names, name)
	}

	sort.Strings(names)

	return names
}

// runSettings implements the `settings` subcommand.
func runSettings(args []string) {
	settings, notes := LoadSettings()

	for _, note := range notes {
		fmt.Println(note)
	}

	action := "show"

	if len(args) > 0 {
		action = strings.ToLower(args[0])
	}

	switch action {

	case "show", "list":
		settings.Print(os.Stdout)

	case "path":
		fmt.Println(settingsFile)

	case "reset":
		defaults := DefaultSettings()

		if err := defaults.Save(); err != nil {
			fmt.Printf("ERROR: could not write %s: %v\n", settingsFile, err)

			return
		}

		fmt.Printf("Reset to defaults and wrote %s\n\n", settingsFile)
		defaults.Print(os.Stdout)

	case "set":
		if len(args) < 3 {
			fmt.Println("Usage: media-cli settings set <key> <value>")
			fmt.Println()
			printSettingKeys(os.Stdout)

			return
		}

		key := strings.ToLower(strings.TrimSpace(args[1]))
		value := strings.Join(args[2:], " ")

		target, ok := settingFields()[key]

		if !ok {
			fmt.Printf("Unknown setting %q\n\n", key)
			printSettingKeys(os.Stdout)

			return
		}

		if err := target.set(settings, value); err != nil {
			fmt.Printf("ERROR: %v\n", err)

			return
		}

		// Refuse rather than quietly storing a clamped value.
		if err := settings.validate(); err != nil {
			fmt.Printf("ERROR: %v\n", err)
			fmt.Println("Nothing was changed.")

			return
		}

		if err := settings.Save(); err != nil {
			fmt.Printf("ERROR: could not write %s: %v\n", settingsFile, err)

			return
		}

		fmt.Printf(
			"%s = %s  (saved to %s)\n",
			key,
			settingFields()[key].get(settings),
			settingsFile,
		)

	default:
		fmt.Printf("Unknown command %q\n\n", action)
		printSettingsUsage(os.Stdout)
	}
}

func (s *Settings) Print(out io.Writer) {
	fields := settingFields()

	_, err := os.Stat(settingsFile)

	source := settingsFile

	if os.IsNotExist(err) {
		source = "defaults (" + settingsFile + " does not exist yet)"
	}

	fmt.Fprintf(out, "Settings from %s\n\n", source)

	for _, name := range sortedFieldNames() {
		fmt.Fprintf(
			out,
			"  %-22s %-16s %s\n",
			name,
			fields[name].get(s),
			fields[name].help,
		)
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, "Change one with:  media-cli settings set <key> <value>")
	fmt.Fprintln(out, "Environment variables override these for a single run.")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "audio_mode=service takes the MP3 the remote service")
	fmt.Fprintln(out, "makes, which is always 128 kbps whatever is requested.")
	fmt.Fprintln(out, "audio_mode=transcode downloads the ~273 kbps AAC source")
	fmt.Fprintln(out, "instead and encodes it locally, and needs ffmpeg.")
}

func printSettingKeys(out io.Writer) {
	fields := settingFields()

	fmt.Fprintln(out, "Available settings:")

	for _, name := range sortedFieldNames() {
		fmt.Fprintf(out, "  %-22s %s\n", name, fields[name].help)
	}
}

func printSettingsUsage(out io.Writer) {
	fmt.Fprintln(out, "Usage:")
	fmt.Fprintln(out, "  media-cli settings              show current settings")
	fmt.Fprintln(out, "  media-cli settings set K V      change one setting")
	fmt.Fprintln(out, "  media-cli settings reset        restore defaults")
	fmt.Fprintln(out, "  media-cli settings path         print the settings file path")
	fmt.Fprintln(out)
	printSettingKeys(out)
}
