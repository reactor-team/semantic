package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
)

// statusLine reports work in progress on one rewritten line of stderr.
//
// It writes only to a terminal. Redirected output gets nothing, which is the
// point twice over: a log file full of half-overwritten counters is worse than
// no counter, and the end-to-end scripts assert on exact stderr, so a progress
// line that appeared under test would be a line every script had to know
// about. What survives redirection is the summary each command already prints
// when it finishes.
//
// Updates are rate-limited rather than emitted per event. Indexing calls this
// once per file and downloading once per chunk of bytes, both far faster than
// anyone can read.
type statusLine struct {
	mu      sync.Mutex
	enabled bool
	last    time.Time
	width   int
	dirty   bool
}

// newStatusLine returns a status line that draws only if stderr is a terminal.
func newStatusLine() *statusLine {
	fd := int(os.Stderr.Fd()) //nolint:gosec // G115: a file descriptor is small and non-negative
	if !term.IsTerminal(fd) {
		return &statusLine{}
	}
	w, _, err := term.GetSize(fd)
	if err != nil || w <= 0 {
		w = 80
	}
	return &statusLine{enabled: true, width: w}
}

// minRedraw is how often the line may change. Fast enough to look live, slow
// enough that the write itself never becomes the bottleneck in a tight loop.
const minRedraw = 80 * time.Millisecond

// set replaces the line with msg, unless it was redrawn too recently. Pass
// force for a message that must not be dropped, such as the last one.
func (s *statusLine) set(msg string, force bool) {
	if s == nil || !s.enabled {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if !force && now.Sub(s.last) < minRedraw {
		return
	}
	s.last = now
	// One column short of the width: a line that exactly fills the terminal
	// wraps, and the next redraw then erases only half of what it left.
	if len(msg) > s.width-1 {
		msg = msg[:s.width-1]
	}
	fmt.Fprintf(os.Stderr, "\r%-*s", s.width-1, msg)
	s.dirty = true
}

// done erases the line so whatever the command prints next starts clean.
// Safe to call when nothing was ever drawn.
func (s *statusLine) done() {
	if s == nil || !s.enabled {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty {
		return
	}
	fmt.Fprintf(os.Stderr, "\r%s\r", strings.Repeat(" ", s.width-1))
	s.dirty = false
}

// humanBytes formats a byte count for a progress line. Sizes here run from a
// few hundred KB (a tokenizer) to a few hundred MB (a model), so two units
// cover it.
func humanBytes(n int64) string {
	const mb = 1 << 20
	if n < mb {
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%.1f MB", float64(n)/mb)
}
