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
        "download_concurrency": 500,
        "save_attempts": 0,
        "output_dir": "",
        "lyrics_language": "english"
    }`), 0o644)

	loaded, notes := LoadSettings()

	if len(notes) == 0 {
		t.Error("expected warnings about the clamped values")
	}

	if loaded.LyricsLanguage != "und" {
		t.Errorf("lyrics_language = %q, want the und fallback", loaded.LyricsLanguage)
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

	if !loaded.Lyrics || loaded.DownloadConcurrency != defaultDownloadConcurrency {
		t.Errorf("expected defaults, got %+v", loaded)
	}

	joined := strings.Join(notes, " ")

	if !strings.Contains(joined, "not valid JSON") {
		t.Errorf("expected a warning, got %v", notes)
	}
}

func TestEnvironmentOverridesFile(t *testing.T) {
	t.Chdir(t.TempDir())

	saved := DefaultSettings()
	saved.DownloadConcurrency = 4
	saved.Save()

	t.Setenv("MEDIA_CLI_DOWNLOAD_CONCURRENCY", "8")
	t.Setenv("MEDIA_CLI_OUTPUT_DIR", "/tmp/elsewhere")

	loaded, _ := LoadSettings()

	if loaded.DownloadConcurrency != 8 {
		t.Errorf("env should win for concurrency, got %d", loaded.DownloadConcurrency)
	}

	if loaded.OutputDir != "/tmp/elsewhere" {
		t.Errorf("env should win for output_dir, got %q", loaded.OutputDir)
	}
}

func TestValidateRejectsBadValues(t *testing.T) {
	cases := map[string]func(*Settings){
		"lyrics_language":      func(s *Settings) { s.LyricsLanguage = "english" },
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

		// ffmpeg_path is legitimately empty by default: empty means
		// "look it up on PATH".
		switch name {
		case "output_dir", "ffmpeg_path":
		default:
			if f.get(s) == "" {
				t.Errorf("%s: get returned empty", name)
			}
		}

		if f.help == "" {
			t.Errorf("%s: missing help text", name)
		}

		if err := f.set(s, f.get(s)); err != nil {
			t.Errorf("%s: could not set its own value back: %v", name, err)
		}
	}
}

// There is deliberately no bitrate setting.
//
// The service returns a fixed .m4a regardless of the quality value sent
// to swd.php, and the MP3 is produced by saveid3.php, which takes no
// bitrate parameter at all. Every output is 128 kbps. A setting for it
// was removed rather than left as a control that does nothing.
func TestNoBitrateSettingIsExposed(t *testing.T) {
	for _, name := range sortedFieldNames() {
		switch name {
		case "quality", "bitrate", "format":
			t.Errorf(
				"%q is exposed but cannot affect the output", name)
		}
	}
}

// A setting that no longer exists must be called out, not silently
// ignored. A stale "quality": 320 sitting in the file is exactly how
// someone ends up believing a setting is applied when it is not.
func TestObsoleteKeysAreReported(t *testing.T) {
	t.Chdir(t.TempDir())

	os.WriteFile(settingsFile, []byte(`{
        "quality": 320,
        "made_up_key": 1,
        "download_concurrency": 5
    }`), 0o644)

	loaded, notes := LoadSettings()

	joined := strings.Join(notes, "\n")

	if !strings.Contains(joined, "quality") {
		t.Errorf("removed setting not reported:\n%s", joined)
	}

	if !strings.Contains(joined, "audio_mode") {
		t.Errorf("note should point at the replacement:\n%s", joined)
	}

	if !strings.Contains(joined, "made_up_key") {
		t.Errorf("unknown key not reported:\n%s", joined)
	}

	// Valid keys alongside them still apply.
	if loaded.DownloadConcurrency != 5 {
		t.Errorf("download_concurrency = %d, want 5", loaded.DownloadConcurrency)
	}
}

// Saving must not carry obsolete keys back to disk.
func TestSaveDropsObsoleteKeys(t *testing.T) {
	t.Chdir(t.TempDir())

	os.WriteFile(settingsFile, []byte(`{"quality":320,"lyrics":false}`), 0o644)

	loaded, _ := LoadSettings()

	if err := loaded.Save(); err != nil {
		t.Fatal(err)
	}

	raw, _ := os.ReadFile(settingsFile)

	if strings.Contains(string(raw), "quality") {
		t.Errorf("obsolete key survived a save:\n%s", raw)
	}

	if !strings.Contains(string(raw), `"lyrics": false`) {
		t.Error("a real setting was lost on save")
	}
}
