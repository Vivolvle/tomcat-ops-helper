package helpergui

import (
	"fmt"
	"testing"
)

// TestLogRingSinceAndWrap: seq monotonicity, max clamp, wrap detection.
func TestLogRingSinceAndWrap(t *testing.T) {
	r := NewLogRing(10)
	for i := 0; i < 20; i++ {
		r.Append("stdout", fmt.Sprintf("line-%d", i))
	}
	lines, next, oldest, wrapped := r.Since(0, 500)
	if len(lines) != 10 || wrapped {
		t.Fatalf("Since(0) should return the buffered window, got %d lines wrapped=%v", len(lines), wrapped)
	}
	if lines[0].Text != "line-10" || lines[9].Text != "line-19" {
		t.Fatalf("window wrong: %s … %s", lines[0].Text, lines[9].Text)
	}
	if oldest != 11 || next != 21 {
		t.Fatalf("oldest=%d next=%d, want 11/21", oldest, next)
	}

	// Cursor behind the evicted range → wrapped, resync to next.
	lines, next2, _, wrapped2 := r.Since(5, 500)
	if !wrapped2 || len(lines) != 10 || next2 != 21 {
		t.Fatalf("wrapped fetch wrong: wrapped=%v len=%d next=%d", wrapped2, len(lines), next2)
	}

	// Incremental fetch.
	lines, next3, _, wrapped3 := r.Since(15, 2)
	if wrapped3 || len(lines) != 2 || lines[0].Seq != 16 || next3 != 18 {
		t.Fatalf("incremental wrong: %+v next=%d", lines, next3)
	}
}

// TestLogRingEmpty: an empty ring reports zero cursors.
func TestLogRingEmpty(t *testing.T) {
	r := NewLogRing(4)
	lines, next, oldest, wrapped := r.Since(0, 10)
	if len(lines) != 0 || next != 0 || oldest != 0 || wrapped {
		t.Fatalf("empty ring: %+v next=%d oldest=%d wrapped=%v", lines, next, oldest, wrapped)
	}
}
