package source

import (
	"strings"
	"testing"
	"time"
)

// Shape of a real per-article response from the Wikimedia pageviews API.
const sample = `{"items":[
 {"project":"en.wikipedia","article":"OpenAI","granularity":"hourly","timestamp":"2026090100","access":"all-access","agent":"user","views":1520},
 {"project":"en.wikipedia","article":"OpenAI","granularity":"hourly","timestamp":"2026090101","access":"all-access","agent":"user","views":0}
]}`

func TestParsePageviews(t *testing.T) {
	evs, err := ParsePageviews(strings.NewReader(sample), 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 {
		t.Fatalf("got %d events", len(evs))
	}
	want := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).Unix()
	if evs[0].TS != want || evs[0].Entity != 3 || evs[0].Value != 1521 {
		t.Errorf("first event = %+v", evs[0])
	}
	if evs[1].TS-evs[0].TS != 3600 || evs[1].Value != 1 {
		t.Errorf("second event = %+v (zero views must stay a valid observation)", evs[1])
	}
}
