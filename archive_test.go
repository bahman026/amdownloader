package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// newArchivePipeline is newTestPipeline with the download archive wired
// in, which is how the program itself runs.
func newArchivePipeline(
	t *testing.T,
	svc *fakeService,
	dir string,
	archivePath string,
) (*Processor, *Archive) {

	t.Helper()

	processor, _ := newTestPipeline(t, svc, dir)

	archive, err := LoadArchive(archivePath)
	if err != nil {
		t.Fatalf("load archive: %v", err)
	}

	processor.Archive = archive

	return processor, archive
}

func archivePath(t *testing.T) string {
	t.Helper()

	return filepath.Join(t.TempDir(), "downloaded.json")
}

func countOutcomes(results []ProcessResult) (success, skipped, failed int) {
	for _, r := range results {
		switch {
		case r.Skipped:
			skipped++
		case r.Error != nil:
			failed++
		default:
			success++
		}
	}

	return success, skipped, failed
}

// TestArchiveSkipsTrackAlreadyDownloadedElsewhere is the case the output
// directory cannot answer: the same song reached by a second playlist,
// which writes to a different folder.
func TestArchiveSkipsTrackAlreadyDownloadedElsewhere(t *testing.T) {
	svc := newFakeService(t, 0)
	path := archivePath(t)

	first := t.TempDir()
	second := t.TempDir()

	tracks := makeTracks(5)

	processor, _ := newArchivePipeline(t, svc, first, path)

	if _, skipped, failed := countOutcomes(
		processor.Process(context.Background(), tracks),
	); skipped != 0 || failed != 0 {
		t.Fatalf("first run skipped %d and failed %d", skipped, failed)
	}

	swdBefore := svc.swdHits.Load()

	// A different folder entirely: nothing here is on disk.
	rerun, _ := newArchivePipeline(t, svc, second, path)

	success, skipped, failed := countOutcomes(
		rerun.Process(context.Background(), tracks),
	)

	if skipped != 5 {
		t.Errorf(
			"archive skipped %d of 5 tracks (success %d, failed %d)",
			skipped, success, failed,
		)
	}

	if got := svc.swdHits.Load(); got != swdBefore {
		t.Errorf("rerun made %d resolver requests, want 0", got-swdBefore)
	}

	if files := listFiles(t, second); len(files) != 0 {
		t.Errorf("second folder should be empty, has %v", files)
	}
}

// TestArchiveSurvivesPlaylistReorder is the reason the archive exists.
//
// Adding one song to the top of a playlist shifts every other track's
// position, and the position is part of the filename, so nothing on disk
// matches any more. Only the new song should download.
func TestArchiveSurvivesPlaylistReorder(t *testing.T) {
	svc := newFakeService(t, 0)
	dir := t.TempDir()
	path := archivePath(t)

	original := makeTracks(3)

	processor, _ := newArchivePipeline(t, svc, dir, path)

	if _, _, failed := countOutcomes(
		processor.Process(context.Background(), original),
	); failed != 0 {
		t.Fatalf("first run failed %d tracks", failed)
	}

	// The playlist gains a song at the top; everything below moves down
	// one place.
	shifted := []Track{{
		Index:  0,
		Name:   "Brand New",
		Artist: "Artist",
		Album:  "Album",
		Link:   "https://music.apple.com/x/99?i=99",
	}}

	for i, track := range original {
		track.Index = i + 1

		shifted = append(shifted, track)
	}

	rerun, _ := newArchivePipeline(t, svc, dir, path)

	success, skipped, failed := countOutcomes(
		rerun.Process(context.Background(), shifted),
	)

	if skipped != 3 {
		t.Errorf("skipped %d of the 3 tracks already downloaded", skipped)
	}

	if success != 1 || failed != 0 {
		t.Errorf(
			"downloaded %d tracks and failed %d, want exactly the 1 new one",
			success, failed,
		)
	}
}

// TestPlaylistReorderWithoutArchiveRefetches documents what the archive
// is fixing: with filename matching alone, the same shift costs a full
// re-download.
func TestPlaylistReorderWithoutArchiveRefetches(t *testing.T) {
	svc := newFakeService(t, 0)
	dir := t.TempDir()

	original := makeTracks(3)

	processor, _ := newTestPipeline(t, svc, dir)

	processor.Process(context.Background(), original)

	shifted := make([]Track, 0, len(original))

	for i, track := range original {
		track.Index = i + 1

		shifted = append(shifted, track)
	}

	rerun, _ := newTestPipeline(t, svc, dir)

	_, skipped, _ := countOutcomes(
		rerun.Process(context.Background(), shifted),
	)

	if skipped != 0 {
		t.Errorf(
			"filename matching skipped %d shifted tracks; "+
				"if it now handles this, the archive comment is stale",
			skipped,
		)
	}
}

// TestArchiveAdoptsFilesAlreadyOnDisk covers the upgrade path: a folder
// filled by an earlier version has no archive behind it, and should get
// one without downloading anything.
func TestArchiveAdoptsFilesAlreadyOnDisk(t *testing.T) {
	svc := newFakeService(t, 0)
	dir := t.TempDir()
	path := archivePath(t)

	tracks := makeTracks(2)

	// What the previous run would have left behind.
	for _, track := range tracks {
		name := fmt.Sprintf(
			"%02d - %s - %s.mp3",
			track.Index+1,
			track.Name,
			track.Artist,
		)

		if err := os.WriteFile(
			filepath.Join(dir, name),
			[]byte("audio"),
			0o644,
		); err != nil {
			t.Fatalf("seed file: %v", err)
		}
	}

	processor, archive := newArchivePipeline(t, svc, dir, path)

	if _, skipped, _ := countOutcomes(
		processor.Process(context.Background(), tracks),
	); skipped != 2 {
		t.Fatalf("skipped %d of 2 files already on disk", skipped)
	}

	if got := archive.Len(); got != 2 {
		t.Fatalf("archive holds %d entries after adoption, want 2", got)
	}

	for _, entry := range archive.Entries() {
		if entry.Path == "" {
			t.Errorf("adopted entry %q has no path", entry.Name)

			continue
		}

		if _, err := os.Stat(entry.Path); err != nil {
			t.Errorf("adopted path does not exist: %v", err)
		}
	}

	// And the adoption survives: a third run reads it back from file.
	reloaded, err := LoadArchive(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}

	if got := reloaded.Len(); got != 2 {
		t.Errorf("reloaded archive holds %d entries, want 2", got)
	}
}

// TestArchiveKeepsIntentionalDuplicates protects the behaviour the
// pipeline already has: a playlist may list one song twice, and one
// archive record satisfies exactly one of those entries.
func TestArchiveKeepsIntentionalDuplicates(t *testing.T) {
	svc := newFakeService(t, 0)
	dir := t.TempDir()
	path := archivePath(t)

	twice := []Track{
		{Index: 0, Name: "Masnavi", Artist: "Shajarian",
			Link: "https://music.apple.com/x/1?i=1"},
		{Index: 1, Name: "Masnavi", Artist: "Shajarian",
			Link: "https://music.apple.com/x/1?i=1"},
	}

	seed, err := LoadArchive(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if err := seed.Add(newArchiveEntry(
		twice[0],
		filepath.Join(dir, "01 - Masnavi - Shajarian.mp3"),
		1024,
		audioModeService,
		128,
	)); err != nil {
		t.Fatalf("seed archive: %v", err)
	}

	processor, archive := newArchivePipeline(t, svc, dir, path)

	success, skipped, failed := countOutcomes(
		processor.Process(context.Background(), twice),
	)

	if skipped != 1 || success != 1 || failed != 0 {
		t.Fatalf(
			"one record should satisfy one entry: skipped %d, downloaded %d, failed %d",
			skipped, success, failed,
		)
	}

	if got := archive.Len(); got != 2 {
		t.Errorf("archive holds %d entries, want 2", got)
	}
}

// TestArchiveRecordsWhatWasDownloaded checks the record is usable: the
// path it names is the file that was actually written.
func TestArchiveRecordsWhatWasDownloaded(t *testing.T) {
	svc := newFakeService(t, 0)
	dir := t.TempDir()
	path := archivePath(t)

	tracks := makeTracks(2)

	processor, archive := newArchivePipeline(t, svc, dir, path)

	processor.Process(context.Background(), tracks)

	entries := archive.Entries()

	if len(entries) != 2 {
		t.Fatalf("archive holds %d entries, want 2", len(entries))
	}

	for _, entry := range entries {
		info, err := os.Stat(entry.Path)
		if err != nil {
			t.Errorf("recorded path is not on disk: %v", err)

			continue
		}

		if entry.Bytes != info.Size() {
			t.Errorf(
				"recorded %d bytes for %s, file holds %d",
				entry.Bytes, entry.Name, info.Size(),
			)
		}

		if entry.Key == "" || entry.DownloadedAt.IsZero() {
			t.Errorf("incomplete record: %+v", entry)
		}
	}
}

// TestArchiveDisabledDownloadsEverything confirms the mechanism is off
// when it is off, and that nothing is written to disk for it.
func TestArchiveDisabledDownloadsEverything(t *testing.T) {
	svc := newFakeService(t, 0)
	path := archivePath(t)

	tracks := makeTracks(3)

	processor, _ := newArchivePipeline(t, svc, t.TempDir(), path)

	processor.Process(context.Background(), tracks)

	// Same tracks, new folder, archive turned off.
	plain, _ := newTestPipeline(t, svc, t.TempDir())

	if success, skipped, _ := countOutcomes(
		plain.Process(context.Background(), tracks),
	); skipped != 0 || success != 3 {
		t.Errorf(
			"archive off: downloaded %d and skipped %d, want 3 and 0",
			success, skipped,
		)
	}
}

func TestAppleMusicTrackID(t *testing.T) {
	cases := []struct {
		link string
		want string
	}{
		{
			"https://music.apple.com/tr/album/still-d-r-e/1440782221?i=1440782870",
			"1440782870",
		},
		{
			"https://music.apple.com/us/song/no-broke-boys/1818761158",
			"1818761158",
		},
		{
			// An album page names an album, not a track.
			"https://music.apple.com/tr/album/rasputin/1553504894",
			"",
		},
		{"", ""},
		{"not a url at all", ""},
	}

	for _, c := range cases {
		if got := appleMusicTrackID(c.link); got != c.want {
			t.Errorf("appleMusicTrackID(%q) = %q, want %q", c.link, got, c.want)
		}
	}
}

// TestTrackIdentityFallsBackToName covers the entry with no id: the key
// must still ignore punctuation, exactly as legacy filename matching
// does, or one song ends up under two keys.
func TestTrackIdentityFallsBackToName(t *testing.T) {
	withID := Track{
		Name:   "Rain",
		Artist: "Shajarian",
		Link:   "https://music.apple.com/tr/album/rain/1?i=42",
	}

	if got := trackIdentity(withID); got != "am:42" {
		t.Errorf("trackIdentity = %q, want am:42", got)
	}

	punctuated := Track{
		Name:   "Rain (Lyrics By Ali Mo'allem)",
		Artist: "Mohammad-Reza Shajarian",
	}

	plain := Track{
		Name:   "Rain Lyrics By Ali Moallem",
		Artist: "Mohammadreza Shajarian",
	}

	if trackIdentity(punctuated) != trackIdentity(plain) {
		t.Errorf(
			"punctuation split one song into two keys: %q vs %q",
			trackIdentity(punctuated),
			trackIdentity(plain),
		)
	}

	if trackIdentity(Track{}) != "" {
		t.Errorf("a track with nothing to key on should have no identity")
	}
}

// TestArchiveFileIsPlainJSON: the whole point of the file is that it can
// be read, and edited, without this program.
func TestArchiveFileIsPlainJSON(t *testing.T) {
	svc := newFakeService(t, 0)
	dir := t.TempDir()
	path := archivePath(t)

	processor, _ := newArchivePipeline(t, svc, dir, path)

	processor.Process(context.Background(), makeTracks(3))

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read archive: %v", err)
	}

	var entries []ArchiveEntry

	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatalf("archive is not valid JSON: %v\n%s", err, raw)
	}

	if len(entries) != 3 {
		t.Fatalf("file holds %d entries, want 3", len(entries))
	}

	for _, entry := range entries {
		if entry.Key == "" || entry.Name == "" {
			t.Errorf("entry is missing fields: %+v", entry)
		}
	}
}

// TestArchiveReadsLineFormat keeps a file written one object per line
// readable, so a hand-appended record, or one from an older build, is
// not silently ignored.
func TestArchiveReadsLineFormat(t *testing.T) {
	path := archivePath(t)

	content := `{"key":"am:1","name":"One","artist":"A","downloaded_at":"2026-01-01T00:00:00Z"}
{"key":"am:2","name":"Two","artist":"A","downloaded_at":"2026-01-01T00:00:00Z"}

{"key":"am:3","name":"Thr
{"name":"No key","artist":"A"}
`

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	archive, err := LoadArchive(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if got := archive.Len(); got != 2 {
		t.Errorf("loaded %d entries, want the 2 usable ones", got)
	}

	if got := archive.Malformed(); got != 2 {
		t.Errorf("reported %d unusable records, want 2", got)
	}

	if !archive.Claim(Track{Link: "https://music.apple.com/x/1?i=1"}) {
		t.Error("a usable record was lost")
	}

	// The next recorded track converts the file to the JSON array
	// form, without losing what was readable.
	if err := archive.Add(newArchiveEntry(
		Track{Name: "Four", Artist: "A",
			Link: "https://music.apple.com/x/4?i=4"},
		"", 0, audioModeService, 128,
	)); err != nil {
		t.Fatalf("add: %v", err)
	}

	reloaded, err := LoadArchive(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}

	if got := reloaded.Len(); got != 3 {
		t.Errorf("after conversion the file holds %d entries, want 3", got)
	}
}

// TestArchiveRefusesAnUnreadableFile is a data-loss guard: if the file
// cannot be understood, loading must fail so the caller turns archiving
// off, rather than starting from empty and overwriting it.
func TestArchiveRefusesAnUnreadableFile(t *testing.T) {
	for _, content := range []string{
		"[ this is not json at all",
		"oops, wrong file entirely",
	} {
		path := archivePath(t)

		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}

		if _, err := LoadArchive(path); err == nil {
			t.Errorf("loading %q as an archive should fail", content)
		}

		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read back: %v", err)
		}

		if string(raw) != content {
			t.Error("a failed load must not touch the file")
		}
	}
}

func TestArchiveAddIsIdempotentForTheSameFile(t *testing.T) {
	path := archivePath(t)

	archive, err := LoadArchive(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	track := Track{
		Name:   "One",
		Artist: "A",
		Link:   "https://music.apple.com/x/1?i=1",
	}

	for i := 0; i < 3; i++ {
		if err := archive.Add(newArchiveEntry(
			track, "/tmp/one.mp3", 10, audioModeService, 128,
		)); err != nil {
			t.Fatalf("add: %v", err)
		}
	}

	if got := archive.Len(); got != 1 {
		t.Errorf("re-recording the same file produced %d entries", got)
	}

	// A second copy of the same song at its own path is a real second
	// record, and must not be collapsed into the first.
	if err := archive.Add(newArchiveEntry(
		track, "/tmp/two.mp3", 10, audioModeService, 128,
	)); err != nil {
		t.Fatalf("add: %v", err)
	}

	if got := archive.Len(); got != 2 {
		t.Errorf("archive holds %d entries, want 2", got)
	}
}

// TestArchiveRewriteReplacesTheFile covers what prune, forget and clear
// rely on, including the claim census being rebuilt.
func TestArchiveRewriteReplacesTheFile(t *testing.T) {
	path := archivePath(t)

	archive, err := LoadArchive(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	first := Track{Name: "One", Artist: "A",
		Link: "https://music.apple.com/x/1?i=1"}

	second := Track{Name: "Two", Artist: "A",
		Link: "https://music.apple.com/x/2?i=2"}

	for _, track := range []Track{first, second} {
		if err := archive.Add(newArchiveEntry(
			track, "", 0, audioModeService, 128,
		)); err != nil {
			t.Fatalf("add: %v", err)
		}
	}

	kept := []ArchiveEntry{archive.Entries()[1]}

	if err := archive.Rewrite(kept); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	if archive.Claim(first) {
		t.Error("a dropped track is still claimable in memory")
	}

	if !archive.Claim(second) {
		t.Error("a kept track stopped being claimable")
	}

	reloaded, err := LoadArchive(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}

	if got := reloaded.Len(); got != 1 {
		t.Fatalf("file holds %d entries after rewrite, want 1", got)
	}

	if reloaded.Claim(first) {
		t.Error("a dropped track came back from the file")
	}

	if !reloaded.Claim(second) {
		t.Error("a kept track did not survive the rewrite")
	}
}
