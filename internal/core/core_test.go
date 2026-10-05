package core

import "testing"

func TestResampleCounts(t *testing.T) {
	// Minute counts stored as count+1: 2, 0, 3 mentions in one hour; 1 in the next.
	evs := []Event{{60, 0, 3}, {120, 0, 1}, {180, 0, 4}, {3700, 0, 2}}
	got := ResampleCounts(evs, 3600)
	if len(got) != 2 || got[0] != (Event{0, 0, 6}) || got[1] != (Event{3600, 0, 2}) {
		t.Errorf("got %v", got)
	}
}
