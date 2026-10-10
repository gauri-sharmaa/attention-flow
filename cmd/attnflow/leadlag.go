package main

import (
	"bufio"
	"flag"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/gauri-sharmaa/attention-flow/internal/leadlag"
)

// cmdLeadLag asks which Polymarket prices move first, on raw trade times
// (Hoffmann–Rosenbaum–Yoshida), period by period, and whether the leaders in
// one period still lead in the next.
func cmdLeadLag(args []string) error {
	fs := flag.NewFlagSet("leadlag", flag.ExitOnError)
	dir := fs.String("dir", "data/pmt90", "polytrades output")
	periods := fs.Int("periods", 3, "split the span into this many periods")
	k := fs.Int("k", 10, "semantic candidate partners per market")
	nNull := fs.Int("null", 99, "whole-day shuffles per pair for the p-value (99+ to test at 1%)")
	minMoves := fs.Int("min", 40, "minimum price moves per market per period")
	screen := fs.Float64("screen", 0, "skip shuffles when |rho| < screen/sqrt(moves) (0 = test every pair)")
	fs.Parse(args)

	streams, _, err := loadTicks(*dir)
	if err != nil {
		return err
	}
	series, t0, t1, err := priceSeries(*dir, len(streams))
	if err != nil {
		return err
	}
	cand := marketCandidates(streams, *k)
	type pair struct{ a, b int }
	var pairs []pair
	for a, cs := range cand {
		for _, b := range cs {
			if a < b {
				pairs = append(pairs, pair{a, b})
			}
		}
	}
	step := (t1 - t0) / float64(*periods)
	fmt.Printf("prices    %d markets · %d candidate pairs · %d periods of %.0f days · %d shuffles per pair\n",
		len(streams), len(pairs), *periods, step/86400, *nNull)

	res := make([][]leadlag.Result, *periods)
	tested := make([][]bool, *periods)
	for p := 0; p < *periods; p++ {
		from, to := t0+float64(p)*step, t0+float64(p+1)*step
		res[p] = make([]leadlag.Result, len(pairs))
		tested[p] = make([]bool, len(pairs))
		var wg sync.WaitGroup
		jobs := make(chan int)
		for w := 0; w < runtime.NumCPU(); w++ {
			wg.Add(1)
			go func(seed uint64) {
				defer wg.Done()
				rng := rand.New(rand.NewPCG(seed, uint64(p)))
				for i := range jobs {
					x, y := series[pairs[i].a], series[pairs[i].b]
					if moves(x, from, to) < *minMoves || moves(y, from, to) < *minMoves {
						continue
					}
					tested[p][i] = true
					// Cheap screen first: a curve this flat cannot reach 1%
					// significance, so skip its shuffles (conservative).
					r := leadlag.Estimate(x, y, from, to, from, to, 0, rng)
					n := math.Min(float64(moves(x, from, to)), float64(moves(y, from, to)))
					if math.Abs(r.Rho) < *screen/math.Sqrt(n) {
						r.P = 1
					} else {
						r = leadlag.Estimate(x, y, from, to, from, to, *nNull, rng)
					}
					res[p][i] = r
				}
			}(uint64(w + 1))
		}
		for i := range pairs {
			jobs <- i
		}
		close(jobs)
		wg.Wait()

		nt, sig, lead, sib, p05 := 0, 0, 0, 0, 0
		var lags []float64
		for i := range pairs {
			if !tested[p][i] {
				continue
			}
			nt++
			if res[p][i].P <= 0.05 {
				p05++
			}
			if res[p][i].P <= 0.01 {
				sig++
				if streams[pairs[i].a].event == streams[pairs[i].b].event {
					sib++
				}
				if res[p][i].Lag != 0 {
					lead++
					lags = append(lags, math.Abs(res[p][i].Lag))
				}
			}
		}
		sort.Float64s(lags)
		med := 0.0
		if len(lags) > 0 {
			med = lags[len(lags)/2]
		}
		fmt.Printf("  period %d  %s–%s  tested %4d · p≤5%% %3d (chance ≈ %.0f) · p≤1%% %3d (chance ≈ %.0f) · %d same-event · %d with a lead (median %s)\n",
			p+1, dayStr(from), dayStr(to), nt, p05, 0.05*float64(nt), sig, 0.01*float64(nt), sib, lead, lagStr(med))
	}

	// Stability: a lead found in one period should point the same way in the next.
	fmt.Println("\nstability (pairs with a significant lead in one period, checked in the next)")
	for p := 0; p+1 < *periods; p++ {
		n, same, sigSame := 0, 0, 0
		for i := range pairs {
			r, q := res[p][i], res[p+1][i]
			if !tested[p][i] || !tested[p+1][i] || r.P > 0.01 || r.Lag == 0 {
				continue
			}
			n++
			if q.Lag != 0 && math.Signbit(q.Lag) == math.Signbit(r.Lag) {
				same++
				if q.P <= 0.05 {
					sigSame++
				}
			}
		}
		if n > 0 {
			fmt.Printf("  period %d → %d  %d leads · same leader next period %d (%.0f%%) · and still significant %d (%.0f%%)\n",
				p+1, p+2, n, same, 100*float64(same)/float64(n), sigSame, 100*float64(sigSame)/float64(n))
		}
	}

	// The strongest stable leads, across all periods.
	type row struct {
		i       int
		lag     float64
		minRho  float64
		periods int
	}
	var rows []row
	for i := range pairs {
		agree, lag, mr := 0, 0.0, math.Inf(1)
		for p := 0; p < *periods; p++ {
			r := res[p][i]
			if tested[p][i] && r.P <= 0.05 && r.Lag != 0 {
				if lag == 0 || math.Signbit(lag) == math.Signbit(r.Lag) {
					agree++
					lag = r.Lag
					mr = math.Min(mr, math.Abs(r.Rho))
				}
			}
		}
		if agree >= 2 {
			rows = append(rows, row{i, lag, mr, agree})
		}
	}
	sort.Slice(rows, func(a, b int) bool {
		if rows[a].periods != rows[b].periods {
			return rows[a].periods > rows[b].periods
		}
		return rows[a].minRho > rows[b].minRho
	})
	fmt.Printf("\nleads that hold in at least two periods: %d\n", len(rows))
	for j, r := range rows {
		if j == 15 {
			break
		}
		a, b := pairs[r.i].a, pairs[r.i].b
		if r.lag < 0 {
			a, b = b, a
		}
		fmt.Printf("  %d periods  %6s ahead  %s\n                          → %s\n", r.periods, lagStr(math.Abs(r.lag)), clip(streams[a].name, 64), clip(streams[b].name, 64))
	}
	return nil
}

func moves(s leadlag.Series, from, to float64) int {
	n := 0
	for i := 1; i < len(s.T); i++ {
		if s.T[i] >= from && s.T[i] < to && s.DX[i] != 0 {
			n++
		}
	}
	return n
}

// priceSeries builds each market's log-odds price at its price changes of at
// least two cents from the last recorded change. Polymarket spreads are
// usually one cent, so this hysteresis keeps trades alternating between bid
// and ask from registering as moves.
func priceSeries(dir string, nM int) ([]leadlag.Series, float64, float64, error) {
	f, err := os.Open(filepath.Join(dir, "ticks.csv"))
	if err != nil {
		return nil, 0, 0, err
	}
	defer f.Close()
	type tick struct {
		t  float64
		px float64
	}
	ticks := make([][]tick, nM)
	sc := bufio.NewScanner(f)
	sc.Scan()
	t0, t1 := math.Inf(1), math.Inf(-1)
	for sc.Scan() {
		p := strings.Split(sc.Text(), ",")
		t, _ := strconv.ParseFloat(p[0], 64)
		m, _ := strconv.Atoi(p[1])
		px, _ := strconv.ParseFloat(p[2], 64)
		if m >= nM {
			continue
		}
		ticks[m] = append(ticks[m], tick{t, px})
		t0, t1 = math.Min(t0, t), math.Max(t1, t)
	}
	out := make([]leadlag.Series, nM)
	for m, ts := range ticks {
		sort.Slice(ts, func(a, b int) bool { return ts[a].t < ts[b].t })
		var tt, xs []float64
		ref := -1.0
		for _, x := range ts {
			if ref < 0 || math.Abs(x.px-ref) >= 0.02-1e-9 {
				ref = x.px
				p := math.Min(0.99, math.Max(0.01, x.px))
				if len(tt) > 0 && tt[len(tt)-1] == x.t {
					xs[len(xs)-1] = math.Log(p / (1 - p))
					continue
				}
				tt = append(tt, x.t)
				xs = append(xs, math.Log(p/(1-p)))
			}
		}
		out[m] = leadlag.FromLevels(tt, xs)
	}
	return out, t0, t1 + 1, sc.Err()
}
