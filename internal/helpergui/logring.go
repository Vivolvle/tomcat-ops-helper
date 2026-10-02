package helpergui

import (
	"sync"
	"time"
)

// LogLine is one entry in the GUI log pane.
type LogLine struct {
	Seq    uint64    `json:"seq"`
	TS     time.Time `json:"ts"`
	Stream string    `json:"stream"` // "stdout" (helper output) | "system" (lifecycle markers)
	Text   string    `json:"text"`
}

// LogRing is a fixed-capacity ring buffer of log lines with a global
// monotonic sequence number. The seq never resets within a GUI session,
// so the browser's `since` cursor stays valid across helper restarts.
type LogRing struct {
	mu     sync.Mutex
	lines  []LogLine
	cap    int
	nextSeq uint64
}

// NewLogRing builds a ring holding up to capacity lines.
func NewLogRing(capacity int) *LogRing {
	return &LogRing{lines: make([]LogLine, 0, capacity), cap: capacity}
}

// Append adds a line, evicting the oldest when full.
func (r *LogRing) Append(stream, text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextSeq++
	line := LogLine{Seq: r.nextSeq, TS: time.Now(), Stream: stream, Text: text}
	if len(r.lines) >= r.cap {
		copy(r.lines, r.lines[1:])
		r.lines[len(r.lines)-1] = line
	} else {
		r.lines = append(r.lines, line)
	}
}

// Since returns up to max lines with Seq greater than since, plus the
// cursor to pass next time (next), the oldest seq still buffered
// (oldest; 0 when empty), and wrapped=true when the caller's cursor
// fell behind the evicted range (UI prints a truncation notice and
// resyncs to next).
//
// next always resumes right AFTER the last line actually returned, so a
// max-clamped response never makes the client skip lines; when the
// caller is caught up (empty result) next is the last buffered seq+1.
func (r *LogRing) Since(since uint64, max int) (lines []LogLine, next, oldest uint64, wrapped bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if max <= 0 {
		max = 500
	}
	if len(r.lines) > 0 {
		oldest = r.lines[0].Seq
		if since > 0 && since < oldest {
			wrapped = true
		}
	}
	for _, l := range r.lines {
		if len(lines) >= max {
			break
		}
		if l.Seq > since {
			lines = append(lines, l)
		}
	}
	switch {
	case len(lines) > 0:
		next = lines[len(lines)-1].Seq + 1
	case len(r.lines) > 0:
		next = r.lines[len(r.lines)-1].Seq + 1
	}
	return lines, next, oldest, wrapped
}
