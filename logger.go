package main

import (
	"bufio"
	"fmt"
	"io"
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
}

// NewLogger returns a Logger that flushes on an interval so output still
// appears promptly while a long download is in flight.
func NewLogger(out io.Writer) *Logger {
	logger := &Logger{
		writer: bufio.NewWriterSize(out, 32<<10),
		stop:   make(chan struct{}),
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
				logger.Flush()
			}
		}
	}()

	return logger
}

func (l *Logger) Printf(format string, args ...any) {
	if l == nil {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	fmt.Fprintf(l.writer, format, args...)
}

func (l *Logger) Println(args ...any) {
	if l == nil {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	fmt.Fprintln(l.writer, args...)
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

	l.mu.Unlock()

	l.wg.Wait()

	l.Flush()
}
