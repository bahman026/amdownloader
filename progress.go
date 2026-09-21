package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

type DownloadProgress struct {
	Index   int
	Label   string
	Current int64
	Total   int64
	Done    bool
	Error   error
}

type ProgressManager struct {
	mu       sync.Mutex
	progress map[int]*DownloadProgress

	total     int
	skipped   int
	failed    int
	startedAt time.Time
}

func NewProgressManager() *ProgressManager {
	return &ProgressManager{
		progress:  make(map[int]*DownloadProgress),
		startedAt: time.Now(),
	}
}

// SetTotal records how many tracks the run covers, so the display can
// show position rather than just activity.
func (p *ProgressManager) SetTotal(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.total = n
}

// NoteSkipped and NoteFailed keep the counters honest for tracks that
// never reach the download stage.
func (p *ProgressManager) NoteSkipped() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.skipped++
}

func (p *ProgressManager) NoteFailed() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.failed++
}

func bar(fraction float64, width int) string {
	if fraction < 0 {
		fraction = 0
	}

	if fraction > 1 {
		fraction = 1
	}

	filled := int(fraction * float64(width))

	if filled > width {
		filled = width
	}

	return strings.Repeat("█", filled) +
		strings.Repeat("░", width-filled)
}

// Lines renders the live display: one summary line, then the transfers
// currently in flight.
//
// Only active transfers are listed, so a 600 track run still occupies a
// handful of lines rather than scrolling off the screen.
func (p *ProgressManager) Lines(maxActive int) []string {
	p.mu.Lock()
	defer p.mu.Unlock()

	var (
		done    int
		active  []*DownloadProgress
		dlBytes int64
	)

	indexes := make([]int, 0, len(p.progress))

	for index := range p.progress {
		indexes = append(indexes, index)
	}

	sort.Ints(indexes)

	for _, index := range indexes {
		item := p.progress[index]

		dlBytes += item.Current

		if item.Done {
			done++

			continue
		}

		active = append(active, item)
	}

	finished := done + p.skipped
	elapsed := time.Since(p.startedAt)

	summary := fmt.Sprintf(
		"  %d/%d  %s  %s",
		finished,
		p.total,
		bar(fractionOf(finished, p.total), 24),
		formatBytes(dlBytes),
	)

	if elapsed > time.Second && dlBytes > 0 {
		summary += fmt.Sprintf(
			"  %s/s",
			formatBytes(int64(float64(dlBytes)/elapsed.Seconds())),
		)
	}

	// Estimate from completed tracks rather than bytes: track sizes
	// vary, but the rate of completion is steady enough to be useful.
	if finished > 0 && finished < p.total {
		perTrack := elapsed / time.Duration(finished)
		remaining := (perTrack * time.Duration(p.total-finished)).
			Round(time.Second)

		summary += "  eta " + remaining.String()
	}

	if p.skipped > 0 {
		summary += fmt.Sprintf("  (%d skipped)", p.skipped)
	}

	if p.failed > 0 {
		summary += fmt.Sprintf("  (%d failed)", p.failed)
	}

	lines := []string{summary}

	for i, item := range active {
		if i >= maxActive {
			lines = append(lines, fmt.Sprintf(
				"    ... and %d more", len(active)-maxActive))

			break
		}

		var fraction float64

		if item.Total > 0 {
			fraction = float64(item.Current) / float64(item.Total)
		}

		size := formatBytes(item.Current)

		if item.Total > 0 {
			size += " / " + formatBytes(item.Total)
		}

		lines = append(lines, fmt.Sprintf(
			"    %-34s %s  %s",
			truncate(item.Label, 34),
			bar(fraction, 18),
			size,
		))
	}

	return lines
}

func fractionOf(done, total int) float64 {
	if total <= 0 {
		return 0
	}

	return float64(done) / float64(total)
}

func (p *ProgressManager) Start(
	index int,
	label string,
	total int64,
) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.progress[index] = &DownloadProgress{
		Index: index,
		Label: label,
		Total: total,
	}
}

func (p *ProgressManager) Update(
	index int,
	current int64,
) {
	p.mu.Lock()
	defer p.mu.Unlock()

	item, ok := p.progress[index]
	if !ok {
		return
	}

	item.Current = current
}

func (p *ProgressManager) Complete(
	index int,
	err error,
) {
	p.mu.Lock()
	defer p.mu.Unlock()

	item, ok := p.progress[index]
	if !ok {
		return
	}

	item.Done = true
	item.Error = err

	if err == nil && item.Total > 0 {
		item.Current = item.Total
	}
}

// Snapshot returns a copy of the current state, ordered by track index.
func (p *ProgressManager) Snapshot() []DownloadProgress {
	p.mu.Lock()
	defer p.mu.Unlock()

	indexes := make([]int, 0, len(p.progress))

	for index := range p.progress {
		indexes = append(indexes, index)
	}

	sort.Ints(indexes)

	items := make([]DownloadProgress, 0, len(indexes))

	for _, index := range indexes {
		items = append(items, *p.progress[index])
	}

	return items
}

// Render draws the current state of every download.
//
// The previous version walked indexes 0..len-1 and looked each one up as
// a map key. Keys are track indexes and are sparse while a run is in
// flight, so with tracks 5 and 7 downloading it probed keys 0 and 1 and
// drew nothing. It now iterates the entries that actually exist.
func (p *ProgressManager) Render(out io.Writer) {
	items := p.Snapshot()

	fmt.Fprint(out, "\033[H\033[2J")

	fmt.Fprintln(out, "Downloads")
	fmt.Fprintln(out, strings.Repeat("─", 100))

	for _, item := range items {
		label := truncate(item.Label, 32)

		const barWidth = 30

		var percent float64

		if item.Total > 0 {
			percent = float64(item.Current) /
				float64(item.Total) * 100

			if percent > 100 {
				percent = 100
			}
		}

		filled := int(
			percent / 100 * float64(barWidth),
		)

		if filled > barWidth {
			filled = barWidth
		}

		if filled < 0 {
			filled = 0
		}

		bar := strings.Repeat("█", filled) +
			strings.Repeat("░", barWidth-filled)

		status := fmt.Sprintf(
			"%6.1f%%",
			percent,
		)

		if item.Done {
			if item.Error != nil {
				status = "ERROR "
			} else {
				status = "DONE  "
			}
		}

		fmt.Fprintf(
			out,
			"[%02d] %-32s [%s] %s  %s / %s\n",
			item.Index+1,
			label,
			bar,
			status,
			formatBytes(item.Current),
			formatBytes(item.Total),
		)
	}

	fmt.Fprintln(out, strings.Repeat("─", 100))
}

type ProgressReader struct {
	Reader   io.Reader
	TrackID  int
	Progress *ProgressManager
	current  int64
}

func (r *ProgressReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)

	if n > 0 {
		r.current += int64(n)

		if r.Progress != nil {
			r.Progress.Update(
				r.TrackID,
				r.current,
			)
		}
	}

	return n, err
}
