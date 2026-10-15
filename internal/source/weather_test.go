package source

import "testing"

func TestEventDate(t *testing.T) {
	d, err := eventDate("KXHIGHNY-26OCT14")
	if err != nil || d.Format("2006-01-02") != "2026-10-14" {
		t.Fatalf("got %v %v", d, err)
	}
}
