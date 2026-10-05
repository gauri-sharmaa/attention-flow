package main

import (
	"bufio"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/gauri-sharmaa/attention-flow/internal/core"
	"github.com/gauri-sharmaa/attention-flow/internal/hawkes"
	"github.com/gauri-sharmaa/attention-flow/internal/semantic"
)

// cmdHawkes tests whether trading in one market excites trading in related
// markets, on raw trade times, out of sample, against a placebo.
func cmdHawkes(args []string) error {
	fs := flag.NewFlagSet("hawkes", flag.ExitOnError)
	dir := fs.String("dir", "data/pmt", "directory with ticks.csv, markets.txt, universe.txt from polytrades")
	extra := fs.String("extra", "", "optional extra event streams: CSV ts,stream,name plus candidate links (see outside attention)")
	k := fs.Int("k", 10, "semantic candidate parents per market")
	l1 := fs.Float64("l1", 5, "L1 penalty on cross-excitation")
	iters := fs.Int("iters", 400, "EM iterations")
	placebos := fs.Int("placebos", 3, "placebo runs with each stream shifted by a random whole number of days")
	season := fs.Bool("season", true, "model the shared hour-of-week activity cycle in the background rate")
	fs.Parse(args)

	streams, evs, err := loadTicks(*dir)
	if err != nil {
		return err
	}
	cand := marketCandidates(streams, *k)
	nMarkets := len(streams)
	var candNoLink [][]int // outside streams present, but not linked to markets
	if *extra != "" {
		var more []stream
		var mevs []hawkes.Event
		if more, mevs, err = loadExtra(*extra, len(streams)); err != nil {
			return err
		}
		cand = append(cand, make([][]int, len(more))...)
		candNoLink = make([][]int, len(cand))
		for i := range cand {
			candNoLink[i] = append([]int(nil), cand[i]...)
		}
		linkExtra(streams, more, cand)
		linkExtraOnly(streams, more, candNoLink)
		streams = append(streams, more...)
		evs = append(evs, mevs...)
		sort.Slice(evs, func(a, b int) bool { return evs[a].T < evs[b].T })
	}
	d := len(streams)
	t0, t1 := evs[0].T, evs[len(evs)-1].T+1
	split := t0 + 0.7*(t1-t0)
	nCand := 0
	for _, c := range cand {
		nCand += len(c)
	}
	fmt.Printf("events    %d across %d streams · %.1f days · %d candidate links · train 70%% / test 30%%\n",
		len(evs), d, (t1-t0)/86400, nCand)
	betas := []float64{1.0 / 10, 1.0 / 120, 1.0 / 1200}

	run := func(evs []hawkes.Event) (self, full *hawkes.Model, gain float64, n int) {
		self = hawkes.New(d, betas, nil)
		full = hawkes.New(d, betas, cand)
		if *season {
			self.UseSeason()
			full.UseSeason()
		}
		self.Fit(evs, hawkes.Options{Iters: *iters, Tol: 1e-8, Window: [2]float64{t0, split}})
		full.Fit(evs, hawkes.Options{Iters: *iters, Tol: 1e-8, L1: *l1, Window: [2]float64{t0, split}})
		ls, n := self.LogLik(evs, split, t1)
		lf, _ := full.LogLik(evs, split, t1)
		return self, full, (lf - ls) / float64(n), n
	}
	if candNoLink != nil {
		return outsideTest(streams, nMarkets, evs, cand, candNoLink, betas, t0, split, t1, *iters, *l1, *season, *placebos)
	}
	_, full, gain, n := run(evs)
	fmt.Printf("held-out  %d test events · cross-links improve log-likelihood by %+.4f nats/event\n", n, gain)

	rng := rand.New(rand.NewPCG(11, 12))
	var pg []string
	for p := 0; p < *placebos; p++ {
		_, _, g, _ := run(shiftStreams(evs, d, t0, t1, rng))
		pg = append(pg, fmt.Sprintf("%+.4f", g))
	}
	if len(pg) > 0 {
		fmt.Printf("placebo   timing destroyed (each stream shifted by a random whole 1-20 days, keeping time of day): %s nats/event\n", strings.Join(pg, " · "))
	}

	// Sibling outcomes of one event (e.g. "returns to normal" / "does not")
	// are expected to trade together; links between different events are the
	// interesting ones, so report them separately.
	edges := full.Edges(0.02)
	var sib, cross []hawkes.Edge
	for _, e := range edges {
		if streams[e.From].event == streams[e.To].event {
			sib = append(sib, e)
		} else {
			cross = append(cross, e)
		}
	}
	fmt.Printf("\nlinks     %d with branching ≥ 0.02 (extra follower trades per leader trade): %d within one event, %d across events\n",
		len(edges), len(sib), len(cross))
	show := func(title string, es []hawkes.Edge, n int) {
		fmt.Printf("\n%s\n", title)
		for i, e := range es {
			if i == n {
				break
			}
			fmt.Printf("  %.3f  %6s  %s\n                → %s\n", e.Branch, lagStr(e.MeanLag), clip(streams[e.From].name, 70), clip(streams[e.To].name, 70))
		}
	}
	show("strongest links across different events", cross, 15)
	show("strongest links between outcomes of the same event", sib, 5)
	return nil
}

type stream struct {
	name, sector, event string
	extra               bool
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func lagStr(s float64) string {
	if s < 90 {
		return fmt.Sprintf("%.0fs", s)
	}
	return fmt.Sprintf("%.1fm", s/60)
}

// loadTicks reads polytrades output. Trades in the same market within the
// same second are one order filled in pieces, so they become one event.
func loadTicks(dir string) ([]stream, []hawkes.Event, error) {
	u, err := core.LoadUniverse(filepath.Join(dir, "universe.txt"))
	if err != nil {
		return nil, nil, err
	}
	var streams []stream
	for i := 0; i+1 < len(u.Entities); i += 2 { // universe lists Activity, Price per market
		e := u.Entities[i]
		sec, ev, _ := strings.Cut(e.Subtopic, "/")
		streams = append(streams, stream{name: strings.TrimPrefix(e.Name, "Activity: "), sector: strings.TrimSuffix(sec, "-activity"), event: ev})
	}
	f, err := os.Open(filepath.Join(dir, "ticks.csv"))
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Scan()
	seen := map[[2]int64]bool{}
	var evs []hawkes.Event
	for sc.Scan() {
		parts := strings.Split(sc.Text(), ",")
		ts, _ := strconv.ParseInt(parts[0], 10, 64)
		mi, _ := strconv.Atoi(parts[1])
		key := [2]int64{ts, int64(mi)}
		if seen[key] || mi >= len(streams) {
			continue
		}
		seen[key] = true
		evs = append(evs, hawkes.Event{T: float64(ts), Dim: mi})
	}
	sort.Slice(evs, func(a, b int) bool { return evs[a].T < evs[b].T })
	return streams, evs, sc.Err()
}

// marketCandidates allows each market to be excited by its sibling outcomes
// (same event) and by its k most similar markets by wording.
func marketCandidates(streams []stream, k int) [][]int {
	u := &core.Universe{}
	for i, s := range streams {
		u.Entities = append(u.Entities, core.Entity{ID: i, Name: s.name, Cluster: s.sector, Subtopic: s.sector + "/" + s.event, Tags: strings.Split(s.event, "-")})
	}
	cand := make([][]int, len(streams))
	add := func(a, b int) {
		for _, x := range cand[a] {
			if x == b {
				return
			}
		}
		cand[a] = append(cand[a], b)
	}
	for _, p := range semantic.Candidates(u, k, 0.05) {
		add(p.A, p.B)
		add(p.B, p.A)
	}
	for i := range streams {
		for j := range streams {
			if i != j && streams[i].event == streams[j].event {
				add(i, j)
			}
		}
	}
	return cand
}

// shiftStreams circularly shifts each stream by its own random whole number
// of days. That keeps every stream's own burstiness and its time-of-day
// pattern, and destroys only event-specific timing between streams, so a
// shared daily cycle cannot pass for a real link.
func shiftStreams(evs []hawkes.Event, d int, t0, t1 float64, rng *rand.Rand) []hawkes.Event {
	span := t1 - t0
	off := make([]float64, d)
	for i := range off {
		off[i] = 86400 * float64(1+rng.IntN(20))
	}
	out := make([]hawkes.Event, len(evs))
	for i, e := range evs {
		t := t0 + mod(e.T-t0+off[e.Dim], span)
		out[i] = hawkes.Event{T: t, Dim: e.Dim}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].T < out[b].T })
	return out
}

func mod(a, b float64) float64 {
	r := a - b*float64(int64(a/b))
	if r < 0 {
		r += b
	}
	return r
}

// loadExtra reads outside mentions (ts,source,keyword) as one stream per
// source and keyword, numbered after the markets.
func loadExtra(path string, offset int) ([]stream, []hawkes.Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Scan()
	idx := map[string]int{}
	var streams []stream
	var evs []hawkes.Event
	for sc.Scan() {
		parts := strings.SplitN(sc.Text(), ",", 3)
		if len(parts) < 3 {
			continue
		}
		ts, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			continue
		}
		key := parts[1] + ": " + parts[2]
		i, ok := idx[key]
		if !ok {
			i = len(streams)
			idx[key] = i
			streams = append(streams, stream{name: key, sector: parts[1], event: parts[2], extra: true})
		}
		evs = append(evs, hawkes.Event{T: float64(ts), Dim: offset + i})
	}
	return streams, evs, sc.Err()
}

// linkExtraOnly adds just the links among outside streams (same keyword,
// different source), for the comparison model with no outside↔market links.
func linkExtraOnly(markets, extra []stream, cand [][]int) {
	n := len(markets)
	for xi, x := range extra {
		for yi, y := range extra {
			if yi != xi && y.event == x.event {
				cand[n+xi] = append(cand[n+xi], n+yi)
			}
		}
	}
}

// outsideTest asks two questions on held-out data, with identical events in
// both models so the only difference is which links are allowed:
// does outside attention help predict market trading, and does market
// trading help predict outside attention? A placebo shifts only the outside
// streams by whole days.
func outsideTest(streams []stream, nM int, evs []hawkes.Event, cand, noLink [][]int, betas []float64,
	t0, split, t1 float64, iters int, l1 float64, season bool, placebos int) error {
	d := len(streams)
	isM, isX := make([]bool, d), make([]bool, d)
	for i := range streams {
		isM[i] = i < nM
		isX[i] = i >= nM
	}
	nX := 0
	for _, e := range evs {
		if e.Dim >= nM {
			nX++
		}
	}
	fmt.Printf("outside   %d mentions in %d streams (source × name) next to %d markets\n", nX, d-nM, nM)
	fit := func(evs []hawkes.Event, c [][]int) *hawkes.Model {
		m := hawkes.New(d, betas, c)
		if season {
			m.UseSeason()
		}
		m.Fit(evs, hawkes.Options{Iters: iters, Tol: 1e-8, L1: l1, Window: [2]float64{t0, split}})
		return m
	}
	gains := func(evs []hawkes.Event) (gm, gx float64, full *hawkes.Model, base *hawkes.Model) {
		full, base = fit(evs, cand), fit(evs, noLink)
		lfm, nm := full.LogLikDims(evs, split, t1, isM)
		lbm, _ := base.LogLikDims(evs, split, t1, isM)
		lfx, nx := full.LogLikDims(evs, split, t1, isX)
		lbx, _ := base.LogLikDims(evs, split, t1, isX)
		return (lfm - lbm) / float64(nm), (lfx - lbx) / float64(max(nx, 1)), full, base
	}
	gm, gx, full, base := gains(evs)
	fmt.Printf("held-out  outside → markets: %+.4f nats per market trade · markets → outside: %+.4f nats per mention\n", gm, gx)
	rng := rand.New(rand.NewPCG(21, 22))
	var pm, px []string
	for p := 0; p < placebos; p++ {
		shifted := shiftSome(evs, d, nM, t0, t1, rng)
		a, b, _, _ := gains(shifted)
		pm = append(pm, fmt.Sprintf("%+.4f", a))
		px = append(px, fmt.Sprintf("%+.4f", b))
	}
	if placebos > 0 {
		fmt.Printf("placebo   outside streams shifted by whole days: %s (→ markets) · %s (→ outside)\n", strings.Join(pm, " "), strings.Join(px, " "))
	}
	// Direction and timing of the outside↔market links.
	var toM, toX []hawkes.Edge
	for _, e := range full.Edges(0.005) {
		switch {
		case e.From >= nM && e.To < nM:
			toM = append(toM, e)
		case e.From < nM && e.To >= nM:
			toX = append(toX, e)
		}
	}
	sum := func(es []hawkes.Edge) (b, lag float64) {
		for _, e := range es {
			b += e.Branch
			lag += e.Branch * e.MeanLag
		}
		if b > 0 {
			lag /= b
		}
		return
	}
	bm, lm := sum(toM)
	bx, lx := sum(toX)
	fmt.Printf("links     outside → market: %d (total branching %.2f, mean lag %s) · market → outside: %d (%.2f, %s)\n",
		len(toM), bm, lagStr(lm), len(toX), bx, lagStr(lx))
	// Confounding check: do market→market links shrink once news is in the model?
	mm := func(m *hawkes.Model) float64 {
		s := 0.0
		for _, e := range m.Edges(0) {
			if e.From < nM && e.To < nM && streams[e.From].event != streams[e.To].event {
				s += e.Branch
			}
		}
		return s
	}
	fmt.Printf("check     cross-event market→market branching: %.2f without outside links, %.2f with them\n", mm(base), mm(full))
	show := func(title string, es []hawkes.Edge) {
		fmt.Printf("\n%s\n", title)
		for i, e := range es {
			if i == 12 {
				break
			}
			fmt.Printf("  %.3f  %6s  %s\n                → %s\n", e.Branch, lagStr(e.MeanLag), clip(streams[e.From].name, 70), clip(streams[e.To].name, 70))
		}
	}
	show("strongest outside → market links", toM)
	show("strongest market → outside links", toX)
	return nil
}

// shiftSome shifts only streams with index ≥ from by whole days.
func shiftSome(evs []hawkes.Event, d, from int, t0, t1 float64, rng *rand.Rand) []hawkes.Event {
	span := t1 - t0
	off := make([]float64, d)
	for i := from; i < d; i++ {
		off[i] = 86400 * float64(1+rng.IntN(20))
	}
	out := make([]hawkes.Event, len(evs))
	for i, e := range evs {
		out[i] = hawkes.Event{T: t0 + mod(e.T-t0+off[e.Dim], span), Dim: e.Dim}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].T < out[b].T })
	return out
}

// linkExtra lets an outside stream about keyword k excite (and be excited by)
// every market whose question names k, and the other sources' streams for k.
func linkExtra(markets, extra []stream, cand [][]int) {
	n := len(markets)
	add := func(a, b int) {
		for _, x := range cand[a] {
			if x == b {
				return
			}
		}
		cand[a] = append(cand[a], b)
	}
	for xi, x := range extra {
		re := regexp.MustCompile(`\b` + regexp.QuoteMeta(x.event) + `\b`)
		for mi, m := range markets {
			if re.MatchString(m.name) {
				add(mi, n+xi)
				add(n+xi, mi)
			}
		}
		for yi, y := range extra {
			if yi != xi && y.event == x.event {
				add(n+xi, n+yi)
			}
		}
	}
}
