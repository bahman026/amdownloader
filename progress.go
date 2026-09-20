package main

import (
	"fmt"
	"io"
	"strings"
	"sync"
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
}

func NewProgressManager() *ProgressManager {
	return &ProgressManager{
		progress: make(map[int]*DownloadProgress),
	}
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

func (p *ProgressManager) Render() {
	p.mu.Lock()
	defer p.mu.Unlock()

	fmt.Print("\033[H\033[2J")

	fmt.Println("Downloads")
	fmt.Println(strings.Repeat("─", 100))

	for i := 0; i < len(p.progress); i++ {
		item, ok := p.progress[i]
		if !ok {
			continue
		}

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

		fmt.Printf(
			"[%02d] %-32s [%s] %s  %s / %s\n",
			item.Index+1,
			label,
			bar,
			status,
			formatBytes(item.Current),
			formatBytes(item.Total),
		)
	}

	fmt.Println(strings.Repeat("─", 100))
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
