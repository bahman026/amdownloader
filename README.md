# media-cli

A Go CLI that reads an Apple Music playlist or song page, resolves each
track through `aaplmusicdownloader.com`, and downloads the generated MP3s.

Pure standard library — no dependencies.

---

## Build

```bash
go build -o media-cli .
```

Or run without building:

```bash
go run . <args>
```

---

## The three modes

The mode is chosen from the argument shape. Quote URLs — they contain
`?` and `&`.

### 1. Playlist

```bash
./media-cli "https://music.apple.com/us/playlist/some-name/pl.abc123"
```

Matches any `music.apple.com` URL containing `/playlist/`.

- Writes `./album_details` (the track manifest)
- Downloads into `./downloads/<playlist name>/`
- The folder is named after the **playlist**, not the first track's
  album, because a playlist can span many albums
- Duplicate tracks are kept; a playlist may intentionally list the same
  song twice

### 2. Single song

```bash
./media-cli "https://music.apple.com/us/album/some-album/123456?i=789012"
```

Matches a `music.apple.com` URL containing `/album/` **and** `?i=`. The
`?i=` track id is required.

- Writes `./album_details`
- Downloads directly into `./downloads/` (no subfolder)

### 3. Replay from `album_details`

```bash
./media-cli
```

With no arguments, reads `./album_details` from the current directory and
processes it. Downloads into `./downloads/<first track's album>/`.

This is the mode to use for retrying a run without re-fetching the
Apple Music page.

---

## Output layout

```
./album_details                          manifest from the last fetch
./downloads/
    <Playlist Name>/
        01 - Song Name - Artist.mp3
        02 - Another Song - Artist.mp3
    Single Song - Artist.mp3             (single-song mode)
```

Files are named `NN - Name - Artist.ext`. The number is the track's
position in the manifest, which is what keeps two copies of the same song
from overwriting each other.

**In-flight downloads are written as `.part`** and renamed into place only
once complete. A killed or failed run never leaves a partial file that
looks finished.

### If a download breaks

| | |
|---|---|
| Transfer breaks mid-file | **Resumed** from where it stopped via an HTTP `Range` request |
| Server ignores `Range` | Falls back to refetching the whole file; never appends to a stale partial |
| Retries exhausted | The `.part` is removed, the track is reported as `[FAILED nn]` |
| One track fails | Every other track carries on; failures are listed at the end |
| Ctrl-C | In-flight transfers abandoned, partials cleaned up |
| Next run | Completed tracks skipped; only the failures are retried |

Resume applies **within a run**. Partials never carry across runs — they
are swept at startup, because the service regenerates the file each time
and appending to a stale partial would corrupt it.

A finished transfer is checked against the size the server declared, so a
truncated file is never renamed into place.

---

## Re-running

A track whose final file already exists is skipped **before any network
request is made**. Re-running a playlist after a partial failure costs
nothing for the tracks that already succeeded.

Both naming schemes are recognised:

- **Current** — `01 - Song - Artist.mp3`, matched exactly by index, so
  duplicate tracks are each recognised independently.
- **Legacy** — files from before the naming change, named after whatever
  the service returned. Those names had punctuation stripped, so matching
  compares only letters and digits: `Rain (Lyrics By Ali Mo'allem)` on
  disk as `Rain Lyrics By Ali Moallem` still matches, as does
  `Mohammad-Reza` against `Mohammadreza`.

Because the old scheme collapsed duplicates onto one file, a legacy file
satisfies exactly one of the duplicate tracks; the remaining copies
download under their own names. On a real 40-track playlist with 36
legacy files this skips 36 and downloads the 4 duplicate copies that were
previously lost.

```bash
./media-cli            # first run: 40 tracks
./media-cli            # second run: 40 skipped, no requests
```

To force a re-download, delete the file (or the folder).

---

## Settings

Persisted in `settings.json` in the working directory.

```bash
media-cli settings                          # show everything
media-cli settings set quality 320
media-cli settings set output_dir ./music
media-cli settings reset                    # back to defaults
media-cli settings path                     # where the file lives
```

| Key | Default | Meaning |
|---|---|---|
| `quality` | `128` | bitrate requested from the resolver (128, 256, 320) |
| `output_dir` | `./downloads` | base directory downloads are written under |
| `resolve_concurrency` | `3` | parallel resolve requests (1-16) |
| `save_concurrency` | `3` | parallel MP3 generation requests (1-16) |
| `download_concurrency` | `4` | parallel file transfers (1-16) |
| `resolve_attempts` | `3` | attempts per track at the resolve stage (1-10) |
| `save_attempts` | `2` | attempts at MP3 generation (kept low, it transcodes) |
| `download_attempts` | `3` | attempts per file transfer (1-10) |
| `skip_existing` | `true` | skip tracks already on disk, before any request |
| `detect_legacy_names` | `true` | also recognise files saved under the old naming scheme |

`settings set` rejects out-of-range values and leaves the file untouched.
A `settings.json` edited by hand is clamped instead, with a warning, so a
bad value can never stop a run. Malformed JSON falls back to defaults.

**Audio format is not configurable.** The remote service decides the
container and codec; the file extension follows whatever it returns.
A format setting here would not change what you get, so there isn't one.

### Environment overrides

These win over `settings.json` for a single run, without editing it.

Concurrency is set per stage. Defaults total 10 concurrent requests
against the service.

| Variable | Default | Stage |
|---|---|---|
| `MEDIA_CLI_RESOLVE_CONCURRENCY` | `3` | resolve track → media URL |
| `MEDIA_CLI_SAVE_CONCURRENCY` | `3` | server-side MP3 generation |
| `MEDIA_CLI_DOWNLOAD_CONCURRENCY` | `4` | transfer the generated file |
| `MEDIA_CLI_QUALITY` | `128` | requested bitrate |
| `MEDIA_CLI_OUTPUT_DIR` | `./downloads` | base download directory |

```bash
MEDIA_CLI_DOWNLOAD_CONCURRENCY=6 ./media-cli
MEDIA_CLI_RESOLVE_CONCURRENCY=4 MEDIA_CLI_SAVE_CONCURRENCY=2 ./media-cli
```

The prefix form works in fish, zsh and bash.

Standard proxy variables (`HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY`) are
honoured.

### How to tune

Don't raise these blind. Every run prints per-stage timings at the end:

```
Stage timings (per track)
  resolve   n=40   avg=812ms     max=2.1s
  saveid3   n=40   avg=4.2s      max=11.3s
  download  n=40   avg=3.4s      max=9.8s
```

Raise the stage with the highest `avg`, one at a time, and watch the
failure count. If failures rise, you have found the service's limit —
go back down. `saveid3` is a server-side transcode; it is usually the
slowest and the least tolerant of concurrency.

---

## Reading the output

```
[SESSION] Establishing session (no active session)
[SKIP 03]     Already downloaded: 03 - Song - Artist
[RESOLVE 01]  ok: Song Name
[ID3 01]      created: Song Name - Artist.mp3
[MP3 01]      done: 01 - Song Name - Artist (6.38 MiB)
```

End-of-run summary:

```
Total:       40
Success:     38
Skipped:     0
Failed:      2
Downloaded:  271.4 MiB
Time:        3m12s

Sessions established: 1
```

`Sessions established: 1` is the healthy case. A higher number means the
service refused the session mid-run and the client rebuilt it — which is
handled automatically, but see below.

---

## Sessions, and the limit you used to clear by hand

All four request types (`album.php`, `swd.php`, `saveid3.php`,
`saved/<file>`) share **one cookie jar**, so the service sees a single
coherent client rather than a new anonymous one per request.

If the service starts refusing a session, the client:

1. discards it and establishes a fresh one,
2. records how many requests that session had **accepted**,
3. rotates future sessions just before that number.

So the limit is discovered, not assumed. When it learns one you'll see:

```
[SESSION] Learned service session limit: ~9 uses
Observed service session limit: ~9 uses
```

There is no manual cache-clearing step, and there shouldn't need to be.
If you find yourself wanting one, that's a bug worth reporting rather
than working around.

---

## Failures

Errors are classified, and only transient ones are retried:

| Error | Behaviour |
|---|---|
| `429`, `408`, `5xx`, network faults | retried with exponential backoff + jitter; `Retry-After` honoured |
| `401` / `403` on the wire | session discarded, retried on a fresh one (does not consume an attempt) |
| `resolver API error: 403 Forbidden` | **permanent** — costs exactly one request |
| `4xx`, empty song name/artist/URL | permanent, not retried |

Attempts per stage: resolve 3, saveid3 2 (it triggers a server-side
transcode, so it is retried least), download 3.

### `resolver API error: 403 Forbidden`

This arrives inside an HTTP **200** response body. It is the service
reporting that *its own* upstream fetch was refused — most often because
the track isn't available in that storefront. It is not a cookie, header
or session problem on our side, so it fails fast rather than retrying.

If a run of these happens consecutively on one session, that is treated
as a session-level signal instead and the session is rotated.

### Interrupting

Ctrl-C cancels cleanly. In-flight transfers are abandoned and their
`.part` files removed — nothing half-written survives as a real file.

---

## Testing

The suite runs against a fake service over `httptest`. It never touches
the real site and needs no network.

```bash
go test ./...            # all tests
go test -race ./...      # thread-safety
go test -v ./...         # per-test output
go test -run TestSession -v ./...
```

```bash
gofmt -l .
go build ./...
go vet ./...
```

---

## How it works

Three independently bounded stages, connected by channels:

```
tracks → [resolve ×3] → [saveid3 ×3] → [download ×4] → results
```

Each stage has its own worker pool, so a slow 7 MiB download does not
occupy a slot that a resolve needs. One `http.Transport` is shared by
everything, so connections stay pooled across all stages and across
session rotations.

A note for anyone modifying `newTransport` in `main.go`: setting
`TLSClientConfig`, or adding a custom dialer without
`ForceAttemptHTTP2: true`, will silently disable HTTP/2 for every
request. Both are deliberate as written.
