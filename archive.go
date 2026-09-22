package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// defaultArchiveFile is the record of everything that has finished
// downloading, kept in the working directory next to settings.json and
// album_details.
//
// It exists because the output directory alone cannot answer "have I
// already got this track". A file on disk is only recognised when it
// sits in the folder this run happens to be writing to, and only under
// the name this run would give it -- and that name starts with the
// track's position in the playlist. Insert one song at the top of a
// playlist and every following entry shifts by one, so every file stops
// matching and the whole playlist downloads again. The same song pulled
// in by a second playlist, or downloaded earlier as a single, is not
// recognised either, because it is in another folder.
//
// The archive is keyed by the track's Apple Music id instead, so it
// answers the question across playlists, folders and renames, and a
// rerun fetches only what is genuinely new.
const defaultArchiveFile = "downloaded.json"

// ArchiveEntry is one finished download.
//
// Key is what matching uses; everything else is there so the file can be
// read, searched and pruned by a human, which a file of bare ids could
// not be.
type ArchiveEntry struct {
	Key    string `json:"key"`
	Name   string `json:"name"`
	Artist string `json:"artist"`
	Album  string `json:"album,omitempty"`

	Path  string `json:"path,omitempty"`
	Bytes int64  `json:"bytes,omitempty"`

	AudioMode string `json:"audio_mode,omitempty"`
	Bitrate   int    `json:"bitrate,omitempty"`

	DownloadedAt time.Time `json:"downloaded_at"`
}

// Archive is the persisted set of finished downloads.
//
// The file is a JSON array of ArchiveEntry, written out again each time
// a track finishes, so a run that is interrupted keeps everything
// recorded up to that point. Each write goes to a temporary file that is
// renamed over the old one, which is what makes rewriting the whole
// array safe: the previous archive stays intact until the new one is
// complete on disk, so a crash mid-write cannot truncate it.
//
// counts is the census taken when the file was read, and Claim spends
// from it. Add deliberately does not put anything back: every claim is
// made in the single-threaded pre-flight pass before the first Add, and
// a track recorded during this run must not make a later duplicate of
// itself look like something already on disk.
type Archive struct {
	path string

	mu      sync.Mutex
	entries []ArchiveEntry

	// counts is what Claim spends; seen is every key ever recorded, and
	// is what answers "do I already have this" for a human reading a
	// list. They differ once a claim has been made, which is why the
	// display cannot use counts.
	counts map[string]int
	seen   map[string]bool

	malformed int
}

// appleMusicTrackID extracts the track id from an Apple Music URL.
//
// A playlist entry and a song page both carry it as ?i=. A /song/ URL
// carries it as the last path segment. An /album/ URL without ?i= names
// the album, not a track, so it deliberately yields nothing: using the
// album id would make every track on that album look like the same one.
func appleMusicTrackID(link string) string {
	link = strings.TrimSpace(link)

	if link == "" {
		return ""
	}

	parsed, err := url.Parse(link)
	if err != nil {
		return ""
	}

	if id := strings.TrimSpace(parsed.Query().Get("i")); id != "" {
		return id
	}

	if strings.Contains(parsed.Path, "/song/") {
		if base := path.Base(parsed.Path); isDigits(base) {
			return base
		}
	}

	return ""
}

func isDigits(value string) bool {
	if value == "" {
		return false
	}

	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}

	return true
}

// trackIdentity is the archive key for a track.
//
// The Apple Music id is preferred because it survives a retitling, a
// different storefront's spelling and a move between playlists. Name and
// artist are the fallback for the rare entry that has no id, canonical-
// ised the same way legacy filenames are, so punctuation differences do
// not split one song into two keys.
func trackIdentity(track Track) string {
	if id := appleMusicTrackID(track.Link); id != "" {
		return "am:" + id
	}

	if key := canonicalKey(track.Name + track.Artist); key != "" {
		return "name:" + key
	}

	return ""
}

// LoadArchive reads the archive at path, creating nothing.
//
// A missing file is not an error: it is simply an empty archive, which
// is what the first run has. A file that cannot be understood at all is
// an error, and the caller turns archiving off for the run rather than
// starting from empty -- starting from empty would overwrite whatever
// the file actually held on the first finished track.
func LoadArchive(path string) (*Archive, error) {
	archive := &Archive{
		path:   strings.TrimSpace(path),
		counts: make(map[string]int),
		seen:   make(map[string]bool),
	}

	if archive.path == "" {
		archive.path = defaultArchiveFile
	}

	raw, err := os.ReadFile(archive.path)

	if err != nil {
		if os.IsNotExist(err) {
			return archive, nil
		}

		return archive, fmt.Errorf(
			"read %s: %w",
			archive.path,
			err,
		)
	}

	entries, malformed, err := decodeArchive(raw)

	if err != nil {
		return archive, fmt.Errorf(
			"read %s: %w",
			archive.path,
			err,
		)
	}

	archive.malformed = malformed

	for _, entry := range entries {
		archive.entries = append(archive.entries, entry)
		archive.counts[entry.Key]++
		archive.seen[entry.Key] = true
	}

	return archive, nil
}

// decodeArchive reads the file's contents.
//
// The format is a JSON array. One object per line is also accepted, so a
// file appended to by hand, or written by an older build, still loads;
// entries missing a key are counted as unusable rather than silently
// matching nothing.
func decodeArchive(raw []byte) ([]ArchiveEntry, int, error) {
	trimmed := strings.TrimSpace(string(raw))

	if trimmed == "" {
		return nil, 0, nil
	}

	var entries []ArchiveEntry

	if strings.HasPrefix(trimmed, "[") {
		if err := json.Unmarshal([]byte(trimmed), &entries); err != nil {
			return nil, 0, err
		}

		kept, unusable := keyedEntries(entries)

		return kept, unusable, nil
	}

	scanner := bufio.NewScanner(strings.NewReader(trimmed))

	// Entries are small; the ceiling only guards against a corrupted
	// file with no newline in it.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	malformed := 0

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		if line == "" {
			continue
		}

		var entry ArchiveEntry

		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			malformed++

			continue
		}

		entries = append(entries, entry)
	}

	if err := scanner.Err(); err != nil && err != io.EOF {
		return nil, malformed, err
	}

	kept, unusable := keyedEntries(entries)

	unusable += malformed

	// Not one usable record, yet something was there: this is not an
	// archive. Saying so is what stops the next finished track from
	// overwriting a file that holds something else.
	if len(kept) == 0 && unusable > 0 {
		return nil, unusable, fmt.Errorf(
			"not a recognisable archive (%d unreadable record(s))",
			unusable,
		)
	}

	return kept, unusable, nil
}

// keyedEntries drops records with nothing to match on and reports how
// many were dropped.
func keyedEntries(entries []ArchiveEntry) ([]ArchiveEntry, int) {
	kept := entries[:0]

	dropped := 0

	for _, entry := range entries {
		if strings.TrimSpace(entry.Key) == "" {
			dropped++

			continue
		}

		kept = append(kept, entry)
	}

	return kept, dropped
}

// Path is where this archive is stored.
func (a *Archive) Path() string {
	if a == nil {
		return ""
	}

	return a.path
}

// Len is how many entries were loaded.
func (a *Archive) Len() int {
	if a == nil {
		return 0
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	return len(a.entries)
}

// Malformed is how many unreadable lines were skipped on load.
func (a *Archive) Malformed() int {
	if a == nil {
		return 0
	}

	return a.malformed
}

// Entries returns a copy of the loaded records, oldest first.
func (a *Archive) Entries() []ArchiveEntry {
	if a == nil {
		return nil
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	out := make([]ArchiveEntry, len(a.entries))
	copy(out, a.entries)

	return out
}

// Claim reports whether the track is already recorded, consuming the
// record that matched.
//
// It consumes rather than merely reads because a playlist may list the
// same song twice on purpose, and the pipeline keeps both copies. Two
// entries in the playlist backed by one archive record means one skip
// and one download, exactly as the on-disk index behaves.
func (a *Archive) Claim(track Track) bool {
	if a == nil {
		return false
	}

	key := trackIdentity(track)

	if key == "" {
		return false
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if a.counts[key] <= 0 {
		return false
	}

	a.counts[key]--

	return true
}

// Has reports whether the track was ever recorded, without consuming
// anything. Claim is what decides a skip; this is for telling someone
// which of several search results they already have.
func (a *Archive) Has(track Track) bool {
	if a == nil {
		return false
	}

	key := trackIdentity(track)

	if key == "" {
		return false
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	return a.seen[key]
}

// Add records a finished download and writes the file out again.
//
// Callers come from the pipeline's concurrent stages, so the write is
// serialised here. Rewriting the whole array once per track is nothing
// next to a several megabyte transfer, and it keeps the file a plain
// JSON document anyone can read or edit.
//
// A record identical to one already held is dropped, so re-downloading
// with skipping turned off does not grow the file without bound.
func (a *Archive) Add(entry ArchiveEntry) error {
	if a == nil {
		return nil
	}

	if strings.TrimSpace(entry.Key) == "" {
		return nil
	}

	if entry.DownloadedAt.IsZero() {
		entry.DownloadedAt = time.Now()
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	for _, held := range a.entries {
		if held.Key == entry.Key && held.Path == entry.Path {
			return nil
		}
	}

	// Full slice expression: the write must not be able to see, or
	// disturb, the entries this Archive is still holding.
	updated := append(
		a.entries[:len(a.entries):len(a.entries)],
		entry,
	)

	if err := a.writeLocked(updated); err != nil {
		return err
	}

	a.entries = updated

	if a.seen == nil {
		a.seen = make(map[string]bool)
	}

	a.seen[entry.Key] = true

	// malformed lines, if there were any, are gone now: the file has
	// just been replaced by what was successfully read plus this.
	a.malformed = 0

	return nil
}

// Rewrite replaces the file with entries, and re-takes the census Claim
// spends from. Used by prune, forget and clear.
func (a *Archive) Rewrite(entries []ArchiveEntry) error {
	if a == nil {
		return nil
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if err := a.writeLocked(entries); err != nil {
		return err
	}

	a.entries = make([]ArchiveEntry, len(entries))
	copy(a.entries, entries)

	a.counts = make(map[string]int)
	a.seen = make(map[string]bool)

	for _, entry := range entries {
		a.counts[entry.Key]++
		a.seen[entry.Key] = true
	}

	a.malformed = 0

	return nil
}

// writeLocked stores entries as a JSON array.
//
// The document is built in a temporary file in the same directory and
// renamed over the old one. Rename is atomic within a directory, so a
// reader either sees the whole previous archive or the whole new one,
// and a crash part way through costs nothing.
func (a *Archive) writeLocked(entries []ArchiveEntry) error {
	if entries == nil {
		entries = []ArchiveEntry{}
	}

	encoded, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}

	encoded = append(encoded, '\n')

	dir := filepath.Dir(a.path)

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	temp, err := os.CreateTemp(dir, ".archive-*")
	if err != nil {
		return fmt.Errorf("write %s: %w", a.path, err)
	}

	tempName := temp.Name()

	_, writeErr := temp.Write(encoded)

	if writeErr == nil {
		writeErr = temp.Sync()
	}

	closeErr := temp.Close()

	if writeErr == nil {
		writeErr = closeErr
	}

	if writeErr != nil {
		os.Remove(tempName)

		return fmt.Errorf("write %s: %w", a.path, writeErr)
	}

	// CreateTemp makes the file 0600; the archive is meant to be read
	// by its owner like any other file in the working directory.
	if err := os.Chmod(tempName, 0o644); err != nil {
		os.Remove(tempName)

		return err
	}

	if err := os.Rename(tempName, a.path); err != nil {
		os.Remove(tempName)

		return fmt.Errorf("replace %s: %w", a.path, err)
	}

	return nil
}

// newArchiveEntry describes a finished track for the archive.
func newArchiveEntry(
	track Track,
	filePath string,
	bytes int64,
	audioMode string,
	bitrate int,
) ArchiveEntry {

	return ArchiveEntry{
		Key:    trackIdentity(track),
		Name:   track.Name,
		Artist: track.Artist,
		Album:  track.Album,

		Path:  filePath,
		Bytes: bytes,

		AudioMode: audioMode,
		Bitrate:   bitrate,

		DownloadedAt: time.Now(),
	}
}

// runArchive implements the `archive` subcommand.
func runArchive(args []string) {
	settings, notes := LoadSettings()

	for _, note := range notes {
		fmt.Println(note)
	}

	archive, err := LoadArchive(settings.ArchiveFile)

	if err != nil {
		fmt.Printf("ERROR: %v\n", err)

		return
	}

	action := "show"

	if len(args) > 0 {
		action = strings.ToLower(strings.TrimSpace(args[0]))
	}

	switch action {

	case "show", "status":
		printArchiveStatus(archive)

	case "list":
		printArchiveList(archive.Entries())

	case "path":
		fmt.Println(archive.Path())

	case "prune":
		pruneArchive(archive)

	case "forget", "remove":
		if len(args) < 2 {
			fmt.Println("Usage: media-cli archive forget <text or track id>")

			return
		}

		forgetArchive(archive, strings.Join(args[1:], " "))

	case "clear":
		clearArchive(archive)

	default:
		fmt.Printf("Unknown command %q\n\n", action)
		printArchiveUsage()
	}
}

func printArchiveUsage() {
	fmt.Println("Usage:")
	fmt.Println("  media-cli archive                 how many tracks are recorded")
	fmt.Println("  media-cli archive list            every recorded track")
	fmt.Println("  media-cli archive prune           drop records whose file is gone")
	fmt.Println("  media-cli archive forget <text>   drop records matching a name or id")
	fmt.Println("  media-cli archive clear           drop every record")
	fmt.Println("  media-cli archive path            print the archive file path")
	fmt.Println()
	fmt.Println("A recorded track is skipped before any request is made,")
	fmt.Println("whatever playlist or folder it turns up in next.")
}

func printArchiveStatus(archive *Archive) {
	entries := archive.Entries()

	if _, err := os.Stat(archive.Path()); os.IsNotExist(err) {
		fmt.Printf(
			"No downloads recorded yet (%s does not exist).\n",
			archive.Path(),
		)

		return
	}

	fmt.Printf("Archive: %s\n", archive.Path())
	fmt.Printf("Tracks:  %d\n", len(entries))

	if archive.Malformed() > 0 {
		fmt.Printf(
			"Ignored: %d unusable record(s); the next download rewrites the file\n",
			archive.Malformed(),
		)
	}

	missing := 0

	for _, entry := range entries {
		if entry.Path == "" {
			continue
		}

		if _, err := os.Stat(entry.Path); err != nil {
			missing++
		}
	}

	if missing > 0 {
		fmt.Printf(
			"Missing: %d recorded file(s) are no longer on disk\n",
			missing,
		)
		fmt.Println()
		fmt.Println("Those tracks are still skipped. `archive prune` drops them,")
		fmt.Println("so the next run downloads them again.")
	}

	if len(entries) == 0 {
		return
	}

	fmt.Println()
	fmt.Println("Most recent:")

	recent := entries

	if len(recent) > 10 {
		recent = recent[len(recent)-10:]
	}

	printArchiveList(recent)
}

func printArchiveList(entries []ArchiveEntry) {
	if len(entries) == 0 {
		fmt.Println("No tracks recorded.")

		return
	}

	for _, entry := range entries {
		fmt.Printf(
			"  %s  %-14s %s - %s\n",
			entry.DownloadedAt.Local().Format("2006-01-02 15:04"),
			entry.Key,
			truncate(entry.Name, 40),
			truncate(entry.Artist, 30),
		)
	}
}

func pruneArchive(archive *Archive) {
	entries := archive.Entries()

	kept := make([]ArchiveEntry, 0, len(entries))

	dropped := 0

	for _, entry := range entries {
		// A record with no path predates path tracking, or came from
		// adopting a legacy file; there is nothing to check, so it
		// stays.
		if entry.Path != "" {
			if _, err := os.Stat(entry.Path); err != nil {
				dropped++

				continue
			}
		}

		kept = append(kept, entry)
	}

	if dropped == 0 && archive.Malformed() == 0 {
		fmt.Printf(
			"Nothing to prune: all %d recorded files are on disk.\n",
			len(entries),
		)

		return
	}

	if err := archive.Rewrite(kept); err != nil {
		fmt.Printf("ERROR: %v\n", err)

		return
	}

	fmt.Printf(
		"Dropped %d record(s) whose file is gone; %d left.\n",
		dropped,
		len(kept),
	)

	fmt.Println("Those tracks will download again on the next run.")
}

func forgetArchive(archive *Archive, query string) {
	query = strings.ToLower(strings.TrimSpace(query))

	if query == "" {
		fmt.Println("Nothing to match.")

		return
	}

	entries := archive.Entries()

	kept := make([]ArchiveEntry, 0, len(entries))

	var dropped []ArchiveEntry

	for _, entry := range entries {
		if archiveEntryMatches(entry, query) {
			dropped = append(dropped, entry)

			continue
		}

		kept = append(kept, entry)
	}

	if len(dropped) == 0 {
		fmt.Printf("No record matches %q.\n", query)

		return
	}

	if err := archive.Rewrite(kept); err != nil {
		fmt.Printf("ERROR: %v\n", err)

		return
	}

	fmt.Printf("Dropped %d record(s):\n", len(dropped))

	printArchiveList(dropped)

	fmt.Println()
	fmt.Println("Those tracks will download again on the next run.")
}

func archiveEntryMatches(entry ArchiveEntry, query string) bool {
	for _, field := range []string{
		entry.Key,
		entry.Name,
		entry.Artist,
		entry.Album,
		entry.Path,
	} {
		if field == "" {
			continue
		}

		if strings.Contains(strings.ToLower(field), query) {
			return true
		}
	}

	return false
}

func clearArchive(archive *Archive) {
	count := archive.Len()

	if count == 0 && archive.Malformed() == 0 {
		fmt.Println("Nothing recorded.")

		return
	}

	if err := archive.Rewrite(nil); err != nil {
		fmt.Printf("ERROR: %v\n", err)

		return
	}

	fmt.Printf("Dropped all %d record(s).\n", count)
	fmt.Println("Everything will download again on the next run.")
}
