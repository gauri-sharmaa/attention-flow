// Package core holds the types shared by every stage of the pipeline.
package core

import (
	"bufio"
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
)

// Entity is one thing whose attention we track (a company, person, topic).
type Entity struct {
	ID       int      `json:"id"`
	Name     string   `json:"name"`
	Wiki     string   `json:"wiki"`
	Cluster  string   `json:"cluster"`  // e.g. "ai"
	Subtopic string   `json:"subtopic"` // e.g. "ai/lab"
	Tags     []string `json:"tags"`
	Text     string   `json:"text,omitempty"` // optional description (Wikipedia extract)
}

// Universe is the full set of tracked entities.
type Universe struct {
	Entities []Entity `json:"entities"`
	Clusters []string `json:"clusters"`
}

// ClusterIndex maps each entity to the index of its cluster in u.Clusters.
func (u *Universe) ClusterIndex() []int {
	idx := map[string]int{}
	for i, c := range u.Clusters {
		idx[c] = i
	}
	out := make([]int, len(u.Entities))
	for i, e := range u.Entities {
		out[i] = idx[e.Cluster]
	}
	return out
}

// ParseUniverse reads the pipe-separated universe file.
func ParseUniverse(r io.Reader) (*Universe, error) {
	u := &Universe{}
	seen := map[string]bool{}
	sc := bufio.NewScanner(r)
	line := 0
	for sc.Scan() {
		line++
		s := strings.TrimSpace(sc.Text())
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		parts := strings.Split(s, "|")
		if len(parts) != 4 {
			return nil, fmt.Errorf("universe line %d: want 4 fields, got %d", line, len(parts))
		}
		sub := strings.TrimSpace(parts[2])
		cluster, _, ok := strings.Cut(sub, "/")
		if !ok {
			return nil, fmt.Errorf("universe line %d: subtopic %q must look like cluster/sub", line, sub)
		}
		e := Entity{
			ID:       len(u.Entities),
			Name:     strings.TrimSpace(parts[0]),
			Wiki:     strings.TrimSpace(parts[1]),
			Cluster:  cluster,
			Subtopic: sub,
			Tags:     strings.Fields(parts[3]),
		}
		u.Entities = append(u.Entities, e)
		if !seen[cluster] {
			seen[cluster] = true
			u.Clusters = append(u.Clusters, cluster)
		}
	}
	return u, sc.Err()
}

// LoadUniverse reads a universe file from disk.
func LoadUniverse(path string) (*Universe, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ParseUniverse(f)
}

// Event is one attention observation: entity had attention Value at time TS.
// TS is in seconds; the engine buckets events into fixed-width bars.
type Event struct {
	TS     int64
	Entity int32
	Value  float64
}

// WriteEvents writes events as CSV: ts,entity,value.
func WriteEvents(w io.Writer, evs []Event) error {
	bw := bufio.NewWriterSize(w, 1<<20)
	if _, err := bw.WriteString("ts,entity,value\n"); err != nil {
		return err
	}
	buf := make([]byte, 0, 64)
	for _, e := range evs {
		buf = buf[:0]
		buf = strconv.AppendInt(buf, e.TS, 10)
		buf = append(buf, ',')
		buf = strconv.AppendInt(buf, int64(e.Entity), 10)
		buf = append(buf, ',')
		buf = strconv.AppendFloat(buf, e.Value, 'g', 8, 64)
		buf = append(buf, '\n')
		if _, err := bw.Write(buf); err != nil {
			return err
		}
	}
	return bw.Flush()
}

// ReadEvents reads the CSV written by WriteEvents and returns events sorted by time.
func ReadEvents(r io.Reader) ([]Event, error) {
	cr := csv.NewReader(bufio.NewReaderSize(r, 1<<20))
	cr.ReuseRecord = true
	if _, err := cr.Read(); err != nil {
		return nil, fmt.Errorf("events header: %w", err)
	}
	var out []Event
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		ts, err1 := strconv.ParseInt(rec[0], 10, 64)
		id, err2 := strconv.ParseInt(rec[1], 10, 32)
		v, err3 := strconv.ParseFloat(rec[2], 64)
		if err1 != nil || err2 != nil || err3 != nil {
			return nil, fmt.Errorf("bad event row %v", rec)
		}
		out = append(out, Event{TS: ts, Entity: int32(id), Value: v})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TS < out[j].TS })
	return out, nil
}

// LoadEvents reads one or more comma-separated events CSVs and merges them in time order.
func LoadEvents(paths string) ([]Event, error) {
	var all []Event
	for _, p := range strings.Split(paths, ",") {
		evs, err := loadEvents(strings.TrimSpace(p))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		all = append(all, evs...)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].TS < all[j].TS })
	return all, nil
}

func loadEvents(path string) ([]Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ReadEvents(f)
}

// ResampleCounts sums count events (stored as count+1) into wider bars, keeping
// the +1 offset so a bar with no mentions is still a valid observation.
func ResampleCounts(evs []Event, barSeconds int64) []Event {
	type key struct {
		bar int64
		id  int32
	}
	sum := map[key]float64{}
	var order []key
	for _, e := range evs {
		k := key{e.TS / barSeconds * barSeconds, e.Entity}
		if _, ok := sum[k]; !ok {
			order = append(order, k)
		}
		sum[k] += e.Value - 1
	}
	out := make([]Event, len(order))
	for i, k := range order {
		out[i] = Event{TS: k.bar, Entity: k.id, Value: sum[k] + 1}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TS < out[j].TS })
	return out
}

// SmoothCounts turns count series (stored as count+1, one event per bar)
// into a causal burst intensity: an exponentially weighted rate over past
// bars only, plus a small floor so quiet periods stay finite after the log.
// Raw counts are mostly zeros with rare spikes; the log of a smoothed rate
// behaves like the attention levels the engine models.
func SmoothCounts(evs []Event, ids map[int32]bool, halfLifeBars float64) []Event {
	a := 1 - math.Exp(-math.Ln2/halfLifeBars)
	rate := map[int32]float64{}
	out := make([]Event, len(evs))
	for i, e := range evs {
		if ids[e.Entity] {
			r := rate[e.Entity]
			r += a * ((e.Value - 1) - r)
			rate[e.Entity] = r
			e.Value = r + 0.05
		}
		out[i] = e
	}
	return out
}
