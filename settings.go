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
// is deliberately absent: the remote service decides the container and
// codec, and exposing a knob that silently did nothing would be worse
// than not having one.
type Settings struct {
	// Quality is the bitrate requested from the resolver.
	Quality int `json:"quality"`

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
}

func DefaultSettings() *Settings {
	return &Settings{
		Quality:             128,
		OutputDir:           "./downloads",
		ResolveConcurrency:  defaultResolveConcurrency,
		SaveConcurrency:     defaultSaveConcurrency,
		DownloadConcurrency: defaultDownloadConcurrency,
		ResolveAttempts:     3,
		SaveAttempts:        2,
		DownloadAttempts:    3,
		SkipExisting:        true,
		DetectLegacyNames:   true,
	}
}

// validQualities are the bitrates the resolver accepts.
var validQualities = []int{128, 256, 320}

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
	settings.Quality = envInt("MEDIA_CLI_QUALITY", settings.Quality)

	settings.ResolveConcurrency = envInt(
		"MEDIA_CLI_RESOLVE_CONCURRENCY", settings.ResolveConcurrency)

	settings.SaveConcurrency = envInt(
		"MEDIA_CLI_SAVE_CONCURRENCY", settings.SaveConcurrency)

	settings.DownloadConcurrency = envInt(
		"MEDIA_CLI_DOWNLOAD_CONCURRENCY", settings.DownloadConcurrency)

	if dir := strings.TrimSpace(os.Getenv("MEDIA_CLI_OUTPUT_DIR")); dir != "" {
		settings.OutputDir = dir
	}

	notes = append(notes, settings.normalize()...)

	return settings, notes
}

// normalize clamps anything out of range and reports what it changed.
func (s *Settings) normalize() []string {
	var notes []string

	if !containsInt(validQualities, s.Quality) {
		notes = append(notes, fmt.Sprintf(
			"WARNING: quality %d is not one of %v; using 128",
			s.Quality,
			validQualities,
		))

		s.Quality = 128
	}

	if strings.TrimSpace(s.OutputDir) == "" {
		s.OutputDir = "./downloads"
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
	clamp("resolve_attempts", &s.ResolveAttempts, 1, 10)
	clamp("save_attempts", &s.SaveAttempts, 1, 10)
	clamp("download_attempts", &s.DownloadAttempts, 1, 10)

	return notes
}

// validate rejects out-of-range values outright.
//
// normalize clamps, which is right when loading a file someone edited by
// hand. An explicit `settings set` should not silently store something
// other than what was asked for, so that path validates instead.
func (s *Settings) validate() error {
	if !containsInt(validQualities, s.Quality) {
		return fmt.Errorf(
			"quality must be one of %v",
			validQualities,
		)
	}

	if strings.TrimSpace(s.OutputDir) == "" {
		return fmt.Errorf("output_dir cannot be empty")
	}

	ranges := []struct {
		name     string
		value    int
		min, max int
	}{
		{"resolve_concurrency", s.ResolveConcurrency, 1, 16},
		{"save_concurrency", s.SaveConcurrency, 1, 16},
		{"download_concurrency", s.DownloadConcurrency, 1, 16},
		{"resolve_attempts", s.ResolveAttempts, 1, 10},
		{"save_attempts", s.SaveAttempts, 1, 10},
		{"download_attempts", s.DownloadAttempts, 1, 10},
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
		"quality": {
			get:  func(s *Settings) string { return strconv.Itoa(s.Quality) },
			set:  setInt(func(s *Settings, v int) { s.Quality = v }),
			help: "bitrate requested from the resolver (128, 256 or 320)",
		},
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
		"detect_legacy_names": {
			get:  func(s *Settings) string { return strconv.FormatBool(s.DetectLegacyNames) },
			set:  setBool(func(s *Settings, v bool) { s.DetectLegacyNames = v }),
			help: "also recognise files saved under the old naming scheme",
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
			"  %-22s %-12s %s\n",
			name,
			fields[name].get(s),
			fields[name].help,
		)
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, "Change one with:  media-cli settings set <key> <value>")
	fmt.Fprintln(out, "Environment variables override these for a single run.")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Audio format is not configurable: the remote service")
	fmt.Fprintln(out, "decides the container and codec, and the file extension")
	fmt.Fprintln(out, "follows whatever it returns.")
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
