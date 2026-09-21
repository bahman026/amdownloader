package main

import (
	"os"
	"strings"
	"testing"
)

func TestDefaultSettingsAreValid(t *testing.T) {
	if err := DefaultSettings().validate(); err != nil {
		t.Fatalf("defaults are not valid: %v", err)
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	t.Chdir(t.TempDir())

	original := DefaultSettings()
	original.Quality = 320
	original.OutputDir = "./music"
	original.DownloadConcurrency = 6
	original.SkipExisting = false

	if err := original.Save(); err != nil {
		t.Fatal(err)
	}

	loaded, notes := LoadSettings()

	for _, n := range notes {
		t.Logf("note: %s", n)
	}

	if loaded.Quality != 320 {
		t.Errorf("quality = %d, want 320", loaded.Quality)
	}

	if loaded.OutputDir != "./music" {
		t.Errorf("output_dir = %q, want ./music", loaded.OutputDir)
	}

	if loaded.DownloadConcurrency != 6 {
		t.Errorf("download_concurrency = %d, want 6", loaded.DownloadConcurrency)
	}

	if loaded.SkipExisting {
		t.Error("skip_existing should have survived as false")
	}
}

// A hand-edited file with nonsense values must be clamped, not fatal.
func TestHandEditedFileIsClamped(t *testing.T) {
	t.Chdir(t.TempDir())

	os.WriteFile(settingsFile, []byte(`{
        "quality": 999,
        "download_concurrency": 500,
        "save_attempts": 0,
        "output_dir": ""
    }`), 0o644)

	loaded, notes := LoadSettings()

	if len(notes) == 0 {
		t.Error("expected warnings about the clamped values")
	}

	if loaded.Quality != 128 {
		t.Errorf("quality = %d, want the 128 fallback", loaded.Quality)
	}

	if loaded.DownloadConcurrency != 16 {
		t.Errorf("download_concurrency = %d, want clamp to 16", loaded.DownloadConcurrency)
	}

	if loaded.SaveAttempts < 1 {
		t.Errorf("save_attempts = %d, want at least 1", loaded.SaveAttempts)
	}

	if loaded.OutputDir == "" {
		t.Error("empty output_dir should fall back to a default")
	}

	if err := loaded.validate(); err != nil {
		t.Errorf("clamped settings should be valid: %v", err)
	}
}

// Malformed JSON must not take the program down.
func TestCorruptSettingsFileFallsBackToDefaults(t *testing.T) {
	t.Chdir(t.TempDir())

	os.WriteFile(settingsFile, []byte("{not json"), 0o644)

	loaded, notes := LoadSettings()

	if loaded.Quality != 128 {
		t.Errorf("quality = %d, want defaults", loaded.Quality)
	}

	joined := strings.Join(notes, " ")

	if !strings.Contains(joined, "not valid JSON") {
		t.Errorf("expected a warning, got %v", notes)
	}
}

func TestEnvironmentOverridesFile(t *testing.T) {
	t.Chdir(t.TempDir())

	saved := DefaultSettings()
	saved.Quality = 128
	saved.DownloadConcurrency = 4
	saved.Save()

	t.Setenv("MEDIA_CLI_QUALITY", "320")
	t.Setenv("MEDIA_CLI_DOWNLOAD_CONCURRENCY", "8")
	t.Setenv("MEDIA_CLI_OUTPUT_DIR", "/tmp/elsewhere")

	loaded, _ := LoadSettings()

	if loaded.Quality != 320 {
		t.Errorf("env should win for quality, got %d", loaded.Quality)
	}

	if loaded.DownloadConcurrency != 8 {
		t.Errorf("env should win for concurrency, got %d", loaded.DownloadConcurrency)
	}

	if loaded.OutputDir != "/tmp/elsewhere" {
		t.Errorf("env should win for output_dir, got %q", loaded.OutputDir)
	}
}

func TestValidateRejectsBadValues(t *testing.T) {
	cases := map[string]func(*Settings){
		"quality":              func(s *Settings) { s.Quality = 999 },
		"download_concurrency": func(s *Settings) { s.DownloadConcurrency = 0 },
		"save_attempts":        func(s *Settings) { s.SaveAttempts = 11 },
		"output_dir":           func(s *Settings) { s.OutputDir = "  " },
	}

	for name, mutate := range cases {
		s := DefaultSettings()
		mutate(s)

		if err := s.validate(); err == nil {
			t.Errorf("%s: expected validation to fail", name)
		}
	}
}

func TestEverySettingIsReadableAndWritable(t *testing.T) {
	fields := settingFields()
	s := DefaultSettings()

	for _, name := range sortedFieldNames() {
		f := fields[name]

		if f.get(s) == "" && name != "output_dir" {
			t.Errorf("%s: get returned empty", name)
		}

		if f.help == "" {
			t.Errorf("%s: missing help text", name)
		}

		if err := f.set(s, f.get(s)); err != nil {
			t.Errorf("%s: could not set its own value back: %v", name, err)
		}
	}
}

// The quality setting must actually reach the resolver.
func TestQualityReachesTheResolver(t *testing.T) {
	p := &Processor{Quality: 320}

	if got := p.quality(); got != 320 {
		t.Errorf("quality() = %d, want 320", got)
	}

	// A Processor built without settings keeps the original default.
	if got := (&Processor{}).quality(); got != 128 {
		t.Errorf("unset quality() = %d, want the 128 default", got)
	}
}
