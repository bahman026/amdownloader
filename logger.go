package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// Logger serialises output from the pipeline's concurrent stages.
//
// The previous code called fmt.Printf directly from every worker. That
// was not a measurable cost (the whole run's logging is under a
// millisecond), but two goroutines formatting into the same fd can
// interleave mid-line, which made the output hard to read. Buffering
// also collapses roughly 18 write syscalls per track into a handful.
type Logger struct {
	mu     sync.Mutex
	writer *bufio.Writer

	stop   chan struct{}
	closed bool
	wg     sync.WaitGroup

	// Live progress block, redrawn beneath the scrolling log.
	//
	// The two would otherwise overwrite each other: the block is
	// erased before any log line is written and redrawn after, so
	// output scrolls normally with the block always at the bottom.
	progress func() []string
	drawn    int
	tty      bool
}

// isTerminal reports whether w is an interactive terminal. Redirected
// output gets plain lines with no cursor movement in it.
func isTerminal(w io.Writer) bool {
	file, ok := w.(*os.File)
	if !ok {
		return false
	}

	info, err := file.Stat()
	if err != nil {
		return false
	}

	return info.Mode()&os.ModeCharDevice != 0
}

// NewLogger returns a Logger that flushes on an interval so output still
// appears promptly while a long download is in flight.
func NewLogger(out io.Writer) *Logger {
	logger := &Logger{
		writer: bufio.NewWriterSize(out, 32<<10),
		stop:   make(chan struct{}),
		tty:    isTerminal(out),
	}

	logger.wg.Add(1)

	go func() {
		defer logger.wg.Done()

		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {

			case <-logger.stop:
				return

			case <-ticker.C:
				logger.refresh()
			}
		}
	}()

	return logger
}

// SetProgress installs the live block. Passing nil removes it.
func (l *Logger) SetProgress(fn func() []string) {
	if l == nil {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	l.eraseLocked()
	l.progress = fn
	l.drawLocked()

	_ = l.writer.Flush()
}

// eraseLocked removes the block by walking the cursor back over it.
func (l *Logger) eraseLocked() {
	if l.drawn == 0 {
		return
	}

	fmt.Fprintf(l.writer, "\033[%dA\033[J", l.drawn)

	l.drawn = 0
}

func (l *Logger) drawLocked() {
	if !l.tty || l.progress == nil {
		return
	}

	for _, line := range l.progress() {
		// Truncated so a long line cannot wrap: wrapping would make
		// the cursor arithmetic above erase the wrong rows.
		fmt.Fprintln(l.writer, truncate(line, 110))

		l.drawn++
	}
}

// refresh redraws the block in place, for byte counts ticking over
// between log lines.
func (l *Logger) refresh() {
	if l == nil {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.tty && l.progress != nil {
		l.eraseLocked()
		l.drawLocked()
	}

	_ = l.writer.Flush()
}

func (l *Logger) Printf(format string, args ...any) {
	if l == nil {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	l.eraseLocked()

	fmt.Fprintf(l.writer, format, args...)

	l.drawLocked()

	if l.tty {
		_ = l.writer.Flush()
	}
}

func (l *Logger) Println(args ...any) {
	if l == nil {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	l.eraseLocked()

	fmt.Fprintln(l.writer, args...)

	l.drawLocked()

	if l.tty {
		_ = l.writer.Flush()
	}
}

func (l *Logger) Flush() {
	if l == nil {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	_ = l.writer.Flush()
}

// Close stops the background flusher and flushes what is left.
// It is safe to call more than once.
func (l *Logger) Close() {
	if l == nil {
		return
	}

	l.mu.Lock()

	if l.closed {
		l.mu.Unlock()

		return
	}

	l.closed = true
	close(l.stop)

	// Leave the terminal clean: the final summary is printed after
	// this, and a stale block above it would just be confusing.
	l.eraseLocked()
	l.progress = nil

	l.mu.Unlock()

	l.wg.Wait()

	l.Flush()
}
