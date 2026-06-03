package shared

import (
	"sync"
)

// LogRingBuffer stores the last N log lines in memory.
type LogRingBuffer struct {
	lines []string
	size  int
	mu    sync.Mutex
}

// Global log buffer for the bridge
var GlobalLogBuffer = NewLogRingBuffer(200)

// NewLogRingBuffer creates a new ring buffer with the given size.
func NewLogRingBuffer(size int) *LogRingBuffer {
	return &LogRingBuffer{
		lines: make([]string, 0, size),
		size:  size,
	}
}

// Write implements io.Writer to intercept logs.
func (r *LogRingBuffer) Write(p []byte) (n int, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	line := string(p)
	if len(r.lines) >= r.size {
		// pop front
		r.lines = r.lines[1:]
	}
	r.lines = append(r.lines, line)
	return len(p), nil
}

// GetLines returns a copy of the current log lines.
func (r *LogRingBuffer) GetLines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	res := make([]string, len(r.lines))
	copy(res, r.lines)
	return res
}
