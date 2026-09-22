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

## The six modes

The mode is chosen from the argument shape. Quote URLs — they contain
`?` and `&`.

### 1. Search

```bash
./media-cli "Je suis fan"
./media-cli Je suis fan            # quotes optional
./media-cli search settings        # for a query that reads as a command
```

Anything that is **not a link and not a command** is searched for on
Apple Music. Tracks and albums come back, you pick, and the picks
download:

```
Searching Apple Music (US) for "a star is born soundtrack"...

  #   Type   Title                          Artist                Album / info              Time
  1   track  Shallow                        Lady Gaga & Bradle... A Star Is Born Soundtrack 3:35
  2   track  Always Remember Us This Way    Lady Gaga             A Star Is Born Soundtrack 3:30
  3   track  Maybe It's Time                Bradley Cooper        A Star Is Born Soundtrack 2:39

  4   album  A Star Is Born Soundtrack      Lady Gaga & Bradle... 35 tracks, 2018
  5   album  A Star Is Born                 Judy Garland          26 tracks, 1954

Pick a number (1-25), several like 1 3 5, a range like 1-3,
"a" for all, or Enter to cancel.
> 4
```

- A pick is a number, several numbers (`1 3 5`), a range (`1-3`), `a`
  for all, or Enter to cancel
- **`track`** downloads that one song into `./downloads/`
- **`album`** looks up every track on it and downloads the lot into
  `./downloads/<Album Name>/`, in running order
- An album has to be picked **on its own** — the two go to different
  places, so one pick cannot be both; picking a mix re-asks
- Tracks already in the archive are marked, so the copy you already have
  is obvious among several releases of the same song
- Explicit tracks are marked `[E]`
- `./album_details` is written either way, so a bare `./media-cli`
  replays the pick

Search uses Apple's public catalogue API, which returns the very same
`?i=<track id>` link you would have pasted — so a search result and a
pasted link take exactly the same path from there on. Storefronts differ:
`media-cli settings set search_country fr` searches the French catalogue.

**Playlists cannot be searched.** Apple's public catalogue indexes songs
and albums only; there is no playlist entity in it. Paste a playlist URL
instead — that is mode 2. That is also why the `Type` column only ever
says `track` or `album`.

#### Narrowing by artist

The catalogue needs every word you send it to match the **same** record,
so adding the artist to the query makes things worse, not better:
`tanhaeia` finds five songs, `shadmehr` finds plenty, and
`tanhaeia shadmehr` finds **nothing at all**. Name the artist separately
instead, and it narrows the results here rather than the search there:

```bash
./media-cli tanhaeeia /artist shadmehr      # title, narrowed by artist
./media-cli shallow /album a star is born   # title, narrowed by album
./media-cli /artist "shadmehr aghili"       # browse an artist
```

`/artist` takes everything after it until the next marker, so quoting is
optional. `--artist` and `/by` work too.

A filter is matched loosely — case, punctuation and doubled letters are
ignored — and in two passes. A word of the name **starting** with what
you typed wins first, so `/artist shad` means *Shadmehr*, not *Farshad*;
if nothing matches that way, anywhere in the name counts, so you get the
Farshads rather than an empty list.

When a filter is given, the search asks the catalogue for a much wider
set than it shows, because the filter is applied to what comes back.

**If the title search still finds nothing**, an `/artist` filter gives
one more way in: the artist is looked up, and *their own catalogue* is
read and matched here. That is what finds a song no spelling of the
title can reach —

```
$ ./media-cli tanhaeeia /artist shadmehr
Searching Apple Music (US) for "tanhaeeia" by artist matching "shadmehr"...

No match for "tanhaeeia" by artist matching "shadmehr"; showing songs by Shadmehr Aghili.

  #   Type   Title                          Artist                Album / info              Time
  1   track  Tanhaeiam                      Shadmehr Aghili       Tajrobeh Kon              4:04
```

— because `tanhaeeia` reduces to `tanhaeia`, which is the start of
`Tanhaeiam`.

The fragment has to be enough for Apple to find the artist, though.
`/artist shad` does **not** reach Shadmehr Aghili: asked for artists
named `shad`, Apple returns two hundred acts literally called *Shad* and
he is not among them. Type more of the name.

#### Near spellings

A name the catalogue stores romanised has no single correct spelling,
and the catalogue matches loosely but not evenly. `tanhaeeiam` finds
*Tanhaeiam*; `tanhaeeia` finds nothing at all, though `tanhaeia` finds
five. So a search that comes back empty is retried against near
spellings — the doubled letters collapsed first, then the longest word
cut back — and the one that worked is named:

```
Searching Apple Music (US) for "tanhaeeia"...

No match for "tanhaeeia"; showing results for "tanhaeia".

  #   Type   Title                          Artist                Album / info              Time
  1   track  Tanhaei                        Mohsen Yeganeh        Hobab                     5:08
```

Only a search that found **nothing** falls back, it stops at the first
spelling that finds something, and it is capped at three extra queries —
Apple rate-limits this endpoint. If nothing works, the message lists
what was tried.

### 2. Playlist

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

### 3. Album

```bash
./media-cli "https://music.apple.com/us/album/drama/1562865138"
```

An `/album/` URL with **no** `?i=` on it. Every track on the album is
looked up and downloaded into `./downloads/<Album Name>/`, numbered in
running order.

The storefront in the link is the one asked, so a `/tr/` link is looked
up against the Turkish catalogue whatever `search_country` says.

### 4. Single song

```bash
./media-cli "https://music.apple.com/us/album/some-album/123456?i=789012"
```

Matches a `music.apple.com` URL containing `/album/` **and** `?i=`. That
track id is what separates this from mode 3: with it, one song; without
it, the whole album. A `music.apple.com` link that matches no mode is
reported as a bad link rather than quietly searched for.

- Writes `./album_details`
- Downloads directly into `./downloads/` (no subfolder)

### 5. Spotify link

```bash
./media-cli "https://open.spotify.com/track/4PTG3Z6ehGkBFwjybzWkR8"
./media-cli "https://open.spotify.com/album/6N9PS4QXF1D0OWPk0Sxtb4"
./media-cli "https://open.spotify.com/playlist/37i9dQZF1DXcBWIGoYBM5M"
```

**Nothing is downloaded from Spotify, and nothing could be** — its audio
is encrypted and its API needs credentials. What a share link does give,
to anyone, is *which songs are meant*: titles, artists and durations,
read from the player page behind the link. Each one is then matched
against the Apple Music catalogue and downloaded from there.

So a Spotify link is a way of naming songs, not a source of them.

```
Spotify album: Whenever You Need Somebody (10 track(s))
Spotify itself is not the source: each track is matched against the
Apple Music catalogue and downloaded from there.

[  1/10] Never Gonna Give You Up - Rick Astley ... Never Gonna Give You Up - Rick Astley
[  2/10] Whenever You Need Somebody - Rick Astley ... Whenever You Need Somebody - Rick Astley
...
```

Matching scores three things and takes the best total, refusing anything
below a floor rather than guessing:

| | |
|---|---|
| Title | equal, or one the start of the other, or one inside the other |
| Artist | a word of the name starting with it; **a different artist is rejected outright** |
| Duration | within 2s, within 5s, and **more than 20s apart is rejected** |

Duration matters because a title and an artist alone will also match a
live take, an extended mix or a sped-up edit. Anything that matched is
listed for you to pick from, and anything that did not is listed
separately as not found — never silently substituted.

The `intl-xx/` links the desktop app copies work, as do `spotify:` URIs.
Lookups are spaced ~0.6s apart because Apple rate-limits search, so a
50-track playlist takes about half a minute to resolve.

A track goes into `./downloads/`; an album or playlist keeps its name as
the folder, the same as every other mode.

### 6. Replay from `album_details`

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
./album_details                          manifest from the last fetch or search
./downloaded.json                        every track already downloaded
./downloads/
    <Playlist Name>/
        01 - Song Name - Artist.mp3
        02 - Another Song - Artist.mp3
    <Album Name>/                        (album URL, or an album picked
        01 - Song Name - Artist.mp3       from a search)
    01 - Single Song - Artist.mp3        (single song, or tracks picked
                                          from a search)
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

A track that has already been downloaded is skipped **before any network
request is made**. Re-running a playlist after a partial failure, or
after new songs were added to it, costs nothing for the tracks that were
already fetched.

Two things recognise a track, in this order.

**1. `./downloaded.json`** — the archive of everything ever downloaded,
keyed by the Apple Music track id. It answers the cases a folder listing
cannot:

- the playlist was **reordered**, or a song was added to the top, so
  every file's `NN -` prefix now points at a different track
- the same song turns up in a **second playlist**, which downloads into
  a different folder
- the track was already fetched on its own, in **single-song mode**
- the file was **renamed**, or moved somewhere else in the library

**2. The output directory** — for anything the archive has not heard of.
A file found this way is recorded in the archive on the spot, so an
existing `./downloads` folder is adopted on the first run and answered
from the archive thereafter.

Both naming schemes are recognised on disk:

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

To force a re-download, drop the track from the archive (see below) and
delete the file.

---

## The download archive

`./downloaded.json` is a plain JSON array, one object per finished track:

```json
[
  {
    "key": "am:1440782870",
    "name": "Still D.R.E. (feat. Snoop Dogg)",
    "artist": "Dr. Dre",
    "album": "2001",
    "path": "downloads/2001/01 - Still D.R.E. (feat. Snoop Dogg) - Dr. Dre.mp3",
    "bytes": 6710886,
    "audio_mode": "transcode",
    "bitrate": 320,
    "downloaded_at": "2026-09-21T14:42:00Z"
  }
]
```

`key` is what matching uses: `am:<apple music track id>`, or
`name:<letters and digits of name+artist>` for the rare entry with no
id. Everything else is there so the file can be read and edited by hand.

It is written out again each time a track finishes, through a temporary
file that is renamed into place, so an interrupted run keeps every record
up to that point and a crash mid-write cannot truncate it.

```bash
media-cli archive                 how many tracks are recorded
media-cli archive list            every recorded track
media-cli archive prune           drop records whose file is gone
media-cli archive forget rasputin drop records matching a name, artist or id
media-cli archive clear           drop every record
media-cli archive path            print the archive file path
```

A record is kept even when its file is deleted, so a track you removed
on purpose stays removed. `archive prune` is the opposite choice: it
drops records whose file is missing, and those tracks download again on
the next run.

Duplicates are preserved. A playlist that lists one song twice is backed
by two records, so one record satisfies exactly one entry.

Turn it off with `media-cli settings set archive false`, and matching
falls back to filenames alone.

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
| `output_dir` | `./downloads` | base directory downloads are written under |
| `resolve_concurrency` | `3` | parallel resolve requests (1-16) |
| `save_concurrency` | `3` | parallel MP3 generation requests (1-16) |
| `download_concurrency` | `4` | parallel file transfers (1-16) |
| `resolve_attempts` | `3` | attempts per track at the resolve stage (1-10) |
| `save_attempts` | `2` | attempts at MP3 generation (kept low, it transcodes) |
| `download_attempts` | `3` | attempts per file transfer (1-10) |
| `skip_existing` | `true` | skip tracks already downloaded, before any request |
| `archive` | `true` | record finished tracks and skip anything already recorded |
| `archive_file` | `downloaded.json` | file the record of finished tracks is kept in |
| `search_country` | `us` | storefront searched when a query is given instead of a link |
| `search_limit` | `20` | how many search results to offer (1-50) |
| `detect_legacy_names` | `true` | also recognise files saved under the old naming scheme |
| `audio_mode` | `service` | `service` (128k, remote) or `transcode` (local encode) |
| `transcode_bitrate` | `320` | kbps for the local encode (128, 192, 256, 320) |
| `ffmpeg_path` | *(empty)* | path to ffmpeg; empty means look it up on `PATH` |
| `lyrics` | `true` | look up synced lyrics on LRCLIB and embed them |
| `lyrics_language` | `und` | 3-letter code recorded in the lyrics frame |
| `lyrics_concurrency` | `4` | parallel lyrics lookups (1-16) |

`settings set` rejects out-of-range values and leaves the file untouched.
A `settings.json` edited by hand is clamped instead, with a warning, so a
bad value can never stop a run. Malformed JSON falls back to defaults.

## Audio quality

There are two paths, chosen with `audio_mode`.

| | `service` (default) | `transcode` |
|---|---|---|
| Source | the service's own MP3 | the AAC the resolver points at |
| Bitrate | **128 kbps, fixed** | `transcode_bitrate`, default **320** |
| Typical size (4:41) | 6.5 MB | ~11 MB |
| Server-side transcode | yes | **skipped entirely** |
| Needs ffmpeg | no | **yes** |
| ID3 tags, artwork, lyrics | yes | yes |

```bash
brew install ffmpeg
media-cli settings set audio_mode transcode
```

**Why there is no `quality` setting.** The resolver accepts a `quality`
parameter and ignores it: at 128 and at 320 it returns the same `.m4a`,
differing only in which CDN node serves it. The MP3 is then built by
`saveid3.php`, which takes no bitrate at all. Measured across 224 files,
every one was 128 kbps. A `quality` setting existed briefly and was
removed rather than left as a control that does nothing.

`transcode_bitrate` is different: it is passed to your local encoder and
genuinely changes the output.

**An honest caveat.** The source measures ~273 kbps AAC. Encoding that
to a 320 kbps MP3 is a second lossy pass, so it is very slightly worse
than the AAC itself while being much better than the service's 128 kbps
MP3. The larger number does not add information. If you want the best
available audio with no re-encoding at all, the untouched `.m4a` is it —
but MP4 uses atom tagging rather than ID3, so artwork and SYLT lyrics
would need separate work.

In `transcode` mode the service's `saveid3.php` step is never called, so
its transcode queue is skipped and runs are faster.

### Environment overrides

These win over `settings.json` for a single run, without editing it.

Concurrency is set per stage. Defaults total 10 concurrent requests
against the service.

| Variable | Default | Stage |
|---|---|---|
| `MEDIA_CLI_RESOLVE_CONCURRENCY` | `3` | resolve track → media URL |
| `MEDIA_CLI_SAVE_CONCURRENCY` | `3` | server-side MP3 generation |
| `MEDIA_CLI_DOWNLOAD_CONCURRENCY` | `4` | transfer the generated file |
| `MEDIA_CLI_OUTPUT_DIR` | `./downloads` | base download directory |
| `MEDIA_CLI_ARCHIVE_FILE` | `./downloaded.json` | record of finished tracks |
| `MEDIA_CLI_SEARCH_COUNTRY` | `us` | storefront to search |
| `MEDIA_CLI_SEARCH_LIMIT` | `20` | results offered per search |

Spotify matching uses `search_country` too: the storefront it looks for
matches in is the one that is searched.
| `MEDIA_CLI_LYRICS_CONCURRENCY` | `4` | parallel lyrics lookups |

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
Download directory: ./downloads/WORKOUT PLAYLIST 2026
Archive: downloaded.json (312 track(s) already downloaded)

[SESSION] Establishing session (no active session)
[SKIP 03]     Already downloaded (archived): 03 - Song - Artist
[SKIP 04]     Already downloaded (current naming): 04 - Song - Artist
[RESOLVE 01]  ok: Song Name
[ID3 01]      created: Song Name - Artist.mp3
[MP3 01]      done: 01 - Song Name - Artist (6.38 MiB)
```

`archived` means the archive recognised it; `current naming` and
`legacy naming` mean a file was found in the output folder, and that
track has now been added to the archive too.

End-of-run summary:

```
Total:       40
Success:     2
Skipped:     38  (archived 36, current naming 2)
Failed:      0
Downloaded:  13.6 MiB
Time:        18.2s

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

## Lyrics

After a track downloads, synchronised lyrics are looked up on
[LRCLIB](https://lrclib.net) and written into the MP3 as an ID3
**SYLT** frame. There is no separate `.lrc` file.

Matching uses the title, artist and — where available — the track
duration, so a cover or a remaster of a different length is not picked
by mistake. A recording more than two seconds adrift is rejected.

**Missing lyrics are never a failure.** A track with none, an
instrumental, or an LRCLIB outage all leave the MP3 exactly as
downloaded and the track still counts as a success. Each run reports
the outcomes:

```
Lyrics
  none           14
  synced         22
```

| Status | Meaning |
|---|---|
| `synced` | timed lyrics found and embedded |
| `none` | LRCLIB has no synced lyrics for this recording |
| `cached` | the file already had a SYLT frame; no lookup made |
| `lookup failed` | LRCLIB was unreachable or errored |
| `embed failed` | the tag could not be edited safely; audio untouched |
| `off` | `lyrics` setting is false |

Embedding is idempotent: an existing SYLT frame is replaced, not
appended to, so re-running never accumulates duplicates. Existing tags
and the audio payload are preserved byte for byte. If a tag uses
unsynchronisation, an extended header or a footer, the file is left
strictly alone — a missing lyric is a non-event, a corrupted audio file
is not.

Lyrics run as a fourth pipeline stage against a different host, so they
add no load to the download service.

## How it works

A pre-flight pass, then four independently bounded stages connected by
channels:

```
tracks → [already downloaded?] → [resolve ×3] → [saveid3 ×3] →
         [download ×4] → [lyrics ×4] → results
                │
                └──→ downloaded.json
```

The pre-flight pass is single-threaded and runs before the first
request: it asks the archive, then the output directory, and everything
recognised is settled as skipped there and then. A finished download
records itself in the archive as it leaves the download stage — not
after lyrics, because a track whose lyrics lookup never finishes is
still a track that does not need downloading again.

Each stage has its own worker pool, so a slow 7 MiB download does not
occupy a slot that a resolve needs. One `http.Transport` is shared by
everything, so connections stay pooled across all stages and across
session rotations.

A note for anyone modifying `newTransport` in `main.go`: setting
`TLSClientConfig`, or adding a custom dialer without
`ForceAttemptHTTP2: true`, will silently disable HTTP/2 for every
request. Both are deliberate as written.
