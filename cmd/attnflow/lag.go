package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/gauri-sharmaa/attention-flow/internal/source"
)

// cmdLag measures how quiet markets react when a busy market in the same
// event jumps. A jump is the leader's traded price moving at least -jump
// within ten minutes (at most one per half hour, the biggest -max per family
// kept). For each follower it reads Polymarket's minute price history around
// the jump and records where the follower's price was just before and 1, 2,
// 5, 10, 15, 30 and 60 minutes after: a follower that sits still while its
// leader moves is a quiet market slow to reprice.
//
// Every jump × follower is saved (-out) with the features a model can learn
// from (topic, volumes, jump size, price level, time to resolution...).
func cmdLag(args []string) error {
	fs := flag.NewFlagSet("lag", flag.ExitOnError)
	dir := fs.String("dir", "data/pmt365", "polytrades output (cache/, families.json)")
	jump := fs.Float64("jump", 0.05, "leader move that counts as a jump")
	maxJumps := fs.Int("max", 25, "biggest jumps kept per family")
	maxFol := fs.Int("followers", 4, "followers per family (the busiest of the quiet ones)")
	out := fs.String("out", "data/pmt365/lag_events.json", "every jump × follower reaction, for training")
	fs.Parse(args)
	b, err := os.ReadFile(filepath.Join(*dir, "families.json"))
	if err != nil {
		return err
	}
	var fams []family
	if err := json.Unmarshal(b, &fams); err != nil {
		return err
	}
	cache := filepath.Join(*dir, "cache")
	phDir := filepath.Join(*dir, "pricehist")
	os.MkdirAll(phDir, 0o755)
	from := time.Now().AddDate(-1, 0, 0).Unix()

	type jumpT struct {
		t    int64
		move float64
		p    float64 // leader price after the jump
	}
	jumpsOf := func(f family) []jumpT {
		tr, err := cachedTradesOnly(cache, f.Leader.ConditionID, from)
		if err != nil || len(tr) < 50 || len(f.Leader.Tokens) != 2 {
			return nil
		}
		sort.Slice(tr, func(a, c int) bool { return tr[a].T < tr[c].T })
		ts, ps := make([]int64, len(tr)), make([]float64, len(tr))
		for i, t := range tr {
			p := t.Price
			if t.Asset != f.Leader.Tokens[0] {
				p = 1 - p
			}
			ts[i], ps[i] = t.T, p
		}
		var js []jumpT
		last := int64(0)
		for i := range ts {
			if ts[i]-last < 1800 {
				continue
			}
			k := sort.Search(len(ts), func(j int) bool { return ts[j] > ts[i]-600 }) - 1
			if k < 0 {
				continue
			}
			if d := ps[i] - ps[k]; math.Abs(d) >= *jump {
				js = append(js, jumpT{ts[i], d, ps[i]})
				last = ts[i]
			}
		}
		sort.Slice(js, func(a, c int) bool { return math.Abs(js[a].move) > math.Abs(js[c].move) })
		if len(js) > *maxJumps {
			js = js[:*maxJumps]
		}
		return js
	}

	// Minute price history, cached per token and 5-day window.
	const win = 5 * 86400
	var mu sync.Mutex
	history := func(token string, t int64) []source.PricePoint {
		w0 := t / win * win
		path := filepath.Join(phDir, fmt.Sprintf("%s-%d.json", token, w0))
		var pts []source.PricePoint
		if b, err := os.ReadFile(path); err == nil && json.Unmarshal(b, &pts) == nil {
			return pts
		}
		pts, err := source.PolyPriceHistoryFine(token, time.Unix(w0-3600, 0), time.Unix(w0+win+7200, 0))
		if err != nil {
			return nil
		}
		b, _ := json.Marshal(pts)
		mu.Lock()
		os.WriteFile(path, b, 0o644)
		mu.Unlock()
		return pts
	}
	priceAt := func(pts []source.PricePoint, t int64) (float64, bool) {
		k := sort.Search(len(pts), func(i int) bool { return pts[i].T > t }) - 1
		if k < 0 || t-pts[k].T > 6*3600 {
			return 0, false
		}
		return pts[k].P, true
	}

	type ev struct {
		Topic, Event, Leader, Follower string
		FollowerID                     string
		T                              int64
		LeaderMove, LeaderPrice        float64
		FollowerVol, LeaderVol         float64
		Before                         float64
		Moves                          []float64 // follower change at 1, 2, 5, 10, 15, 30, 60 minutes
		LeaderMoves                    []float64 // the leader's own price-history change at the same times (confirms the jump was real)
		HoursToEnd                     float64
		Siblings                       int
	}
	horiz := []int64{60, 120, 300, 600, 900, 1800, 3600}
	type job struct {
		f family
		j jumpT
		m source.PolyMeta
	}
	var jobs []job
	nj := 0
	for _, f := range fams {
		js := jumpsOf(f)
		nj += len(js)
		fol := f.Followers
		if len(fol) > *maxFol {
			fol = fol[:*maxFol]
		}
		for _, j := range js {
			for _, m := range fol {
				if len(m.Tokens) == 2 {
					jobs = append(jobs, job{f, j, m})
				}
			}
		}
	}
	fmt.Fprintf(os.Stderr, "%d leader jumps, %d jump × follower pairs to read\n", nj, len(jobs))
	evs := make([]*ev, len(jobs))
	var wg sync.WaitGroup
	next := make(chan int)
	var done int64
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				jb := jobs[i]
				pts := history(jb.m.Tokens[0], jb.j.t)
				mu.Lock()
				done++
				if done%500 == 0 {
					fmt.Fprintf(os.Stderr, "read %d/%d\n", done, len(jobs))
				}
				mu.Unlock()
				before, ok := priceAt(pts, jb.j.t-30)
				if !ok {
					continue
				}
				e := &ev{Topic: jb.f.Topic, Event: jb.f.Event, Leader: jb.f.Leader.Question, Follower: jb.m.Question, FollowerID: jb.m.ConditionID,
					T: jb.j.t, LeaderMove: jb.j.move, LeaderPrice: jb.j.p, FollowerVol: jb.m.Volume, LeaderVol: jb.f.Leader.Volume,
					Before: before, Siblings: len(jb.f.Followers)}
				if !jb.m.End.IsZero() {
					e.HoursToEnd = float64(jb.m.End.Unix()-jb.j.t) / 3600
				}
				for _, h := range horiz {
					p, ok := priceAt(pts, jb.j.t+h)
					if !ok {
						p = before
					}
					e.Moves = append(e.Moves, p-before)
				}
				if lpts := history(jb.f.Leader.Tokens[0], jb.j.t); lpts != nil {
					if lb, ok := priceAt(lpts, jb.j.t-600); ok {
						for _, h := range horiz {
							p, ok := priceAt(lpts, jb.j.t+h)
							if !ok {
								p = lb
							}
							e.LeaderMoves = append(e.LeaderMoves, p-lb)
						}
					}
				}
				evs[i] = e
			}
		}()
	}
	for i := range jobs {
		next <- i
	}
	close(next)
	wg.Wait()
	var kept []ev
	for _, e := range evs {
		if e != nil {
			kept = append(kept, *e)
		}
	}
	bj, _ := json.Marshal(kept)
	if err := os.WriteFile(*out, bj, 0o644); err != nil {
		return err
	}
	fmt.Printf("%d jump × follower reactions saved to %s\n", len(kept), *out)
	return nil
}

// cachedTradesOnly reads a market's cached trades without downloading.
func cachedTradesOnly(dir, cid string, from int64) ([]source.HistTrade, error) {
	files, _ := filepath.Glob(filepath.Join(dir, cid+"-*.json"))
	if len(files) == 0 {
		return nil, os.ErrNotExist
	}
	sort.Strings(files)
	b, err := os.ReadFile(files[len(files)-1])
	if err != nil {
		return nil, err
	}
	var tr []struct {
		Asset     string  `json:"asset"`
		Price     float64 `json:"price"`
		Size      float64 `json:"size"`
		Timestamp int64   `json:"timestamp"`
	}
	if err := json.Unmarshal(b, &tr); err != nil {
		return nil, err
	}
	out := make([]source.HistTrade, 0, len(tr))
	for _, t := range tr {
		if t.Timestamp >= from {
			out = append(out, source.HistTrade{Asset: t.Asset, Price: t.Price, Size: t.Size, T: t.Timestamp})
		}
	}
	return out, nil
}
