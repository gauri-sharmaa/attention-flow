package main

import (
	"bufio"
	"flag"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// cmdBacktest paper-trades the order-flow signal on Polymarket's own trade
// history: when recent net buying of YES (or NO) in a market is unusually
// strong, take that side, hold for a while, and pay realistic costs.
// Parameters are chosen on one period and frozen for the next (walk-forward).
func cmdBacktest(args []string) error {
	fs := flag.NewFlagSet("backtest", flag.ExitOnError)
	dir := fs.String("dir", "data/pmt90", "polytrades output with ticks.csv (needs the dir column)")
	periods := fs.Int("periods", 3, "walk-forward periods")
	cost := fs.Float64("cost", 0.01, "cost per side in price units (half-spread + slippage; 0.01 = 1¢)")
	stake := fs.Float64("stake", 100, "dollars per trade")
	fs.Parse(args)

	streams, _, err := loadTicks(*dir)
	if err != nil {
		return err
	}
	ticks, t0, t1, err := loadSignedTicks(*dir, len(streams))
	if err != nil {
		return err
	}
	step := (t1 - t0) / float64(*periods)
	grid := []params{}
	for _, w := range []float64{300, 900, 1800} {
		for _, z := range []float64{2, 3, 4} {
			for _, h := range []float64{1800, 3600, 4 * 3600} {
				grid = append(grid, params{window: w, z: z, hold: h})
			}
		}
	}
	fmt.Printf("paper     %d markets · %d trades · %d periods of %.0f days · $%.0f per trade · cost %.0f¢ per side\n",
		len(streams), countTicks(ticks), *periods, step/86400, *stake, *cost*100)
	fmt.Println("          signal: net dollars buying YES vs NO over the last `window`, in std units of that market's own flow")
	rng := rand.New(rand.NewPCG(51, 52))
	var all []trade
	for p := 1; p < *periods; p++ {
		a, b, c := t0+float64(p-1)*step, t0+float64(p)*step, t0+float64(p+1)*step
		best, bestPnL := grid[0], math.Inf(-1)
		for _, g := range grid {
			tr := simulate(ticks, g, a, b, *cost, *stake, nil)
			if pnl := total(tr); pnl > bestPnL && len(tr) >= 20 {
				best, bestPnL = g, pnl
			}
		}
		test := simulate(ticks, best, b, c, *cost, *stake, nil)
		noCost := simulate(ticks, best, b, c, 0, *stake, nil)
		random := simulate(ticks, best, b, c, *cost, *stake, rng)
		all = append(all, test...)
		fmt.Printf("\n  period %d  %s–%s  chosen on the previous period: window %s, z ≥ %.0f, hold %s (in-sample $%.0f)\n",
			p+1, dayStr(b), dayStr(c), lagStr(best.window), best.z, lagStr(best.hold), bestPnL)
		report("    out of sample", test, *stake)
		report("    same, no costs", noCost, *stake)
		report("    placebo: random side", random, *stake)
	}
	fmt.Println()
	report("all test periods", all, *stake)
	return nil
}

type params struct{ window, z, hold float64 }

type sTick struct {
	t, px, flow float64 // flow: signed dollars (+ pushes YES up)
}

type trade struct {
	m          int
	t, pnl, rt float64 // rt: return per dollar
}

func loadSignedTicks(dir string, nM int) ([][]sTick, float64, float64, error) {
	f, err := os.Open(filepath.Join(dir, "ticks.csv"))
	if err != nil {
		return nil, 0, 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Scan()
	if !strings.HasSuffix(sc.Text(), ",dir") {
		return nil, 0, 0, fmt.Errorf("%s/ticks.csv has no trade direction; re-run polytrades", dir)
	}
	out := make([][]sTick, nM)
	t0, t1 := math.Inf(1), math.Inf(-1)
	for sc.Scan() {
		p := strings.Split(sc.Text(), ",")
		t, _ := strconv.ParseFloat(p[0], 64)
		m, _ := strconv.Atoi(p[1])
		px, _ := strconv.ParseFloat(p[2], 64)
		usd, _ := strconv.ParseFloat(p[3], 64)
		d, _ := strconv.ParseFloat(p[4], 64)
		if m >= nM {
			continue
		}
		out[m] = append(out[m], sTick{t, px, d * usd})
		t0, t1 = math.Min(t0, t), math.Max(t1, t)
	}
	for m := range out {
		sort.Slice(out[m], func(a, b int) bool { return out[m][a].t < out[m][b].t })
	}
	return out, t0, t1 + 1, sc.Err()
}

func countTicks(ts [][]sTick) int {
	n := 0
	for _, t := range ts {
		n += len(t)
	}
	return n
}

// simulate runs the strategy on [from, to). The flow's scale is estimated
// causally (EW over past windows) so no future data sets the threshold.
// Entry is at the first trade strictly after the signal, exit at the last
// trade price at or before entry+hold; each side pays `cost`. With rng set,
// the side is random (placebo).
func simulate(ticks [][]sTick, g params, from, to, cost, stake float64, rng *rand.Rand) []trade {
	var out []trade
	for m, ts := range ticks {
		var flowSum float64 // signed flow inside the trailing window
		lo := 0
		var ewVar float64
		var nObs int
		busyUntil := math.Inf(-1)
		for i := 0; i < len(ts); i++ {
			tk := ts[i]
			flowSum += tk.flow
			for ts[lo].t <= tk.t-g.window {
				flowSum -= ts[lo].flow
				lo++
			}
			// Update the scale of window flow from the previous state only.
			sd := math.Sqrt(ewVar)
			z := 0.0
			if nObs > 50 && sd > 0 {
				z = flowSum / sd
			}
			ewVar += 0.01 * (flowSum*flowSum - ewVar)
			nObs++
			if tk.t < from || tk.t >= to || tk.t < busyUntil || math.Abs(z) < g.z || i+1 >= len(ts) {
				continue
			}
			side := math.Copysign(1, z) // +1: buy YES, -1: buy NO
			if rng != nil {
				side = float64(2*rng.IntN(2) - 1)
			}
			entry := ts[i+1]
			if entry.t >= to {
				continue
			}
			j := i + 1
			for j+1 < len(ts) && ts[j+1].t <= entry.t+g.hold {
				j++
			}
			exit := ts[j]
			// Buying YES at p costs p; buying NO costs 1-p. Pay cost each side.
			var buy, sell float64
			if side > 0 {
				buy, sell = entry.px+cost, exit.px-cost
			} else {
				buy, sell = (1-entry.px)+cost, (1-exit.px)-cost
			}
			if buy <= 0.02 || buy >= 0.98 {
				continue // too close to 0 or 1 to trade sensibly
			}
			r := sell/buy - 1
			out = append(out, trade{m: m, t: entry.t, pnl: stake * r, rt: r})
			busyUntil = entry.t + g.hold
		}
	}
	return out
}

func total(tr []trade) float64 {
	s := 0.0
	for _, t := range tr {
		s += t.pnl
	}
	return s
}

func report(label string, tr []trade, stake float64) {
	if len(tr) == 0 {
		fmt.Printf("%-26s no trades\n", label)
		return
	}
	win, sum, sq := 0, 0.0, 0.0
	for _, t := range tr {
		if t.pnl > 0 {
			win++
		}
		sum += t.rt
		sq += t.rt * t.rt
	}
	n := float64(len(tr))
	mean := sum / n
	sd := math.Sqrt(math.Max(sq/n-mean*mean, 0))
	tstat := 0.0
	if sd > 0 {
		tstat = mean / sd * math.Sqrt(n)
	}
	fmt.Printf("%-26s %5d trades · win %4.1f%% · avg %+6.2f%% per trade (t = %+.1f) · total $%+.0f\n",
		label, len(tr), 100*float64(win)/n, 100*mean, tstat, total(tr))
}
