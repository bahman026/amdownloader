package main

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// Redirected output must contain no cursor movement.
//
// The live block only makes sense on a terminal; in a pipe, a log file
// or CI it would just be escape-code noise.
func TestNoEscapeCodesWhenPiped(t *testing.T) {
	var buf bytes.Buffer

	logger := NewLogger(&buf) // a buffer is not a terminal

	pm := NewProgressManager()
	pm.SetTotal(3)
	pm.Start(0, "track", 100)
	pm.Update(0, 50)

	logger.SetProgress(func() []string { return pm.Lines(4) })
	logger.Printf("[RESOLVE 01] ok\n")
	logger.Printf("[MP3 01] done\n")
	logger.Close()

	out := buf.String()

	if strings.Contains(out, "\033[") {
		t.Errorf("escape codes leaked into redirected output: %q", out)
	}

	for _, want := range []string{"[RESOLVE 01] ok", "[MP3 01] done"} {
		if !strings.Contains(out, want) {
			t.Errorf("log line %q was lost", want)
		}
	}
}

func TestProgressSummary(t *testing.T) {
	pm := NewProgressManager()
	pm.SetTotal(10)
	pm.startedAt = time.Now().Add(-10 * time.Second)

	for i := 0; i < 4; i++ {
		pm.Start(i, fmt.Sprintf("track %d", i), 1000)
		pm.Complete(i, nil)
	}

	pm.NoteSkipped()
	pm.NoteFailed()

	pm.Start(5, "in flight", 1000)
	pm.Update(5, 250)

	lines := pm.Lines(6)

	if len(lines) < 2 {
		t.Fatalf("expected a summary and an active line, got %v", lines)
	}

	summary := lines[0]

	// 4 done + 1 skipped = 5 of 10.
	for _, want := range []string{"5/10", "skipped", "failed", "eta"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary missing %q: %s", want, summary)
		}
	}

	// Only unfinished transfers are listed.
	if len(lines) != 2 {
		t.Errorf("expected exactly one active line, got %d: %v", len(lines)-1, lines)
	}

	if !strings.Contains(lines[1], "in flight") {
		t.Errorf("active line wrong: %s", lines[1])
	}
}

// A long run must not produce a screenful of lines.
func TestProgressCapsActiveLines(t *testing.T) {
	pm := NewProgressManager()
	pm.SetTotal(600)

	for i := 0; i < 40; i++ {
		pm.Start(i, fmt.Sprintf("track %d", i), 1000)
		pm.Update(i, 100)
	}

	lines := pm.Lines(6)

	if len(lines) > 8 {
		t.Errorf("block is %d lines; it should stay compact", len(lines))
	}

	if !strings.Contains(lines[len(lines)-1], "more") {
		t.Errorf("overflow should be summarised, got %q", lines[len(lines)-1])
	}
}

// The display is read from one goroutine while workers update it from
// several others.
func TestProgressConcurrentUpdatesAndRender(t *testing.T) {
	pm := NewProgressManager()
	pm.SetTotal(50)

	var wg sync.WaitGroup

	for i := 0; i < 20; i++ {
		wg.Add(1)

		go func(n int) {
			defer wg.Done()

			pm.Start(n, fmt.Sprintf("track %d", n), 5000)

			for b := 0; b < 50; b++ {
				pm.Update(n, int64(b*100))
			}

			pm.Complete(n, nil)
		}(i)
	}

	wg.Add(1)

	go func() {
		defer wg.Done()

		for i := 0; i < 200; i++ {
			_ = pm.Lines(6)
		}
	}()

	wg.Wait()

	if lines := pm.Lines(6); len(lines) == 0 {
		t.Error("no output after concurrent use")
	}
}

func TestBarClampsOutOfRange(t *testing.T) {
	for _, f := range []float64{-1, 0, 0.5, 1, 2} {
		if got := len([]rune(bar(f, 10))); got != 10 {
			t.Errorf("bar(%v) is %d runes wide, want 10", f, got)
		}
	}
}
