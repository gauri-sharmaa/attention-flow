package main

import (
	"flag"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gauri-sharmaa/attention-flow/internal/core"
)

// cmdDirection asks: right after a market's price moves 2¢, can we tell which
// way its next 2¢ move will go? Moves are found the same way the live view
// finds them (2¢ from the price at the previous move). Features use only
// what is known at the moment of a move; the outcome (the next move)
// happens strictly after it, so nothing leaks.
//
// Three predictors, each fit on one period and scored on the next:
//   - base rate: always the share of up moves in the training period;
//   - last move: the direction of the move that just happened (bid-ask
//     bounce and momentum both live here);
//   - order flow: last move plus net buying pressure over three windows,
//     trade-count imbalance, price level and time since the last move.
//
// If order flow beats "last move" out of sample, buying pressure carries
// direction information beyond simple bounce.
func cmdDirection(args []string) error {
	fs := flag.NewFlagSet("direction", flag.ExitOnError)
	dir := fs.String("dir", "data/pmt365", "polytrades output with ticks.csv")
	periods := fs.Int("periods", 12, "walk-forward periods")
	size := fs.Float64("size", 0.02, "price move that counts")
	smooth := fs.Int("smooth", 1, "price = mean of the last N trade prices (N > 1 averages out bid-ask bounce)")
	fs.Parse(args)

	streams, err := loadStreams(*dir)
	if err != nil {
		return err
	}
	ticks, t0, t1, err := loadSignedTicks(*dir, len(streams))
	if err != nil {
		return err
	}
	var all []moveEx
	for m, tk := range ticks {
		all = append(all, directionExamples(m, tk, *size, *smooth)...)
	}
	sort.Slice(all, func(a, b int) bool { return all[a].t < all[b].t })
	step := (t1 - t0) / float64(*periods)
	ups := 0
	for _, e := range all {
		if e.up {
			ups++
		}
	}
	fmt.Printf("direction %d price moves of ≥ %.0f¢ across %d markets · %.1f%% up · %d periods of %.0f days · price = mean of last %d trades\n",
		len(all), *size*100, len(streams), 100*float64(ups)/float64(len(all)), *periods, step/86400, *smooth)
	same := 0
	for _, e := range all {
		if e.up == e.up0() {
			same++
		}
	}
	fmt.Printf("          next move goes the same way as the one before it %.1f%% of the time (above 50%% = momentum, below = reversal)\n", 100*float64(same)/float64(len(all)))
	fmt.Println("          features: net buying (5 min, 30 min, 2 h), trade-count imbalance, previous move, price level, time since last move")
	fmt.Println("          scores out of sample: accuracy and log-loss (lower is better); 'sure' = model ≥ 60% either way")

	var tests []scored
	for p := 1; p < *periods; p++ {
		a, b, c := t0+float64(p-1)*step, t0+float64(p)*step, t0+float64(p+1)*step
		var tr, te []moveEx
		for _, e := range all {
			switch {
			case e.t >= a && e.t < b:
				tr = append(tr, e)
			case e.t >= b && e.t < c:
				te = append(te, e)
			}
		}
		if len(tr) < 200 || len(te) < 200 {
			continue
		}
		base := 0.0
		for _, e := range tr {
			if e.up {
				base++
			}
		}
		base /= float64(len(tr))
		last := fitLogit(tr, []int{0})
		flow := fitLogit(tr, nil)
		s := scored{period: fmt.Sprintf("%s–%s", dayStr(b), dayStr(c))}
		for _, e := range te {
			s.add(e, base, last.prob(e), flow.prob(e))
		}
		tests = append(tests, s)
		fmt.Printf("\n  period %2d  %s  %6d moves  %4.1f%% up\n", p+1, s.period, s.n, 100*s.ups/float64(s.n))
		s.print("    ")
	}
	var tot scored
	for _, s := range tests {
		tot.merge(s)
	}
	fmt.Printf("\nall test periods  %d moves\n", tot.n)
	tot.print("  ")
	beat := 0
	for _, s := range tests {
		if s.ll[2] < s.ll[1] {
			beat++
		}
	}
	fmt.Printf("  order flow beats last move on log-loss in %d of %d periods\n", beat, len(tests))

	// Which kinds of market is direction most predictable in?
	groups := directionGroups(streams, ticks)
	fmt.Printf("\nby group (all test periods; accuracy: last move → order flow)\n")
	for _, g := range groups {
		var gs scored
		for _, s := range tests {
			gs.merge(s.sub(g.markets))
		}
		if gs.n < 500 {
			continue
		}
		fmt.Printf("  %-22s %7d moves  %5.1f%% → %5.1f%%  log-loss %.4f → %.4f  sure %4.1f%% of moves at %5.1f%%\n",
			g.name, gs.n, 100*gs.acc[1]/float64(gs.n), 100*gs.acc[2]/float64(gs.n), gs.ll[1]/float64(gs.n), gs.ll[2]/float64(gs.n),
			100*float64(gs.sure)/float64(gs.n), 100*gs.sureOK/float64(max(gs.sure, 1)))
	}
	return nil
}

// moveEx is one price move with what was known just before it.
type moveEx struct {
	t      float64
	market int
	up     bool
	x      []float64
}

const nDirFeatures = 8

// directionExamples walks one market's trades and emits a moveEx at each
// 2¢ move, labelled with the direction of the move after it. Features:
//
//	0 direction of the move that just happened (+1 up, -1 down)
//	1-3 net signed dollars / gross dollars over 5 min, 30 min, 2 h
//	4 trade-count imbalance over 5 min
//	5 price now, centered (p - 0.5)
//	6 how close the price is to 0 or 1: |p - 0.5|
//	7 log(1 + minutes since the move before this one)
func directionExamples(market int, tk []sTick, size float64, smooth int) []moveEx {
	var out []moveEx
	ref, lastMove := -1.0, 0.0
	wins := []float64{300, 1800, 7200}
	lo := make([]int, len(wins)) // window start indices
	net, gross := make([]float64, len(wins)), make([]float64, len(wins))
	cnt5, sgn5 := 0, 0
	i := 0
	for j := 0; j < len(tk); {
		// All trades in this second form one step; the move (if any) is
		// judged at the step's last price, from features before it.
		k := j
		for k < len(tk) && tk[k].t == tk[j].t {
			k++
		}
		t := tk[j].t
		// Advance windows: include trades up to this step (indices < k), drop old.
		for ; i < k; i++ {
			for w := range wins {
				net[w] += tk[i].flow
				gross[w] += math.Abs(tk[i].flow)
			}
			cnt5++
			sgn5 += sign(tk[i].flow)
		}
		for w, span := range wins {
			for lo[w] < k && tk[lo[w]].t < t-span {
				net[w] -= tk[lo[w]].flow
				gross[w] -= math.Abs(tk[lo[w]].flow)
				if w == 0 {
					cnt5--
					sgn5 -= sign(tk[lo[w]].flow)
				}
				lo[w]++
			}
		}
		// Price: mean of the last `smooth` trade prices up to this step.
		px, np := 0.0, 0
		for q := k - 1; q >= 0 && np < smooth; q-- {
			px += tk[q].px
			np++
		}
		px /= float64(np)
		if ref < 0 {
			ref, lastMove = px, t
		} else if math.Abs(px-ref) >= size-1e-9 {
			up := px > ref
			x := make([]float64, nDirFeatures)
			x[0] = map[bool]float64{true: 1, false: -1}[up]
			for w := range wins {
				if gross[w] > 0 {
					x[1+w] = net[w] / gross[w]
				}
			}
			if cnt5 > 0 {
				x[4] = float64(sgn5) / float64(cnt5)
			}
			x[5], x[6] = px-0.5, math.Abs(px-0.5)
			x[7] = math.Log1p((t - lastMove) / 60)
			out = append(out, moveEx{t: t, market: market, up: up, x: x})
			ref, lastMove = px, t
		}
		j = k
	}
	// The label is the NEXT move's direction: everything in x is known at
	// the moment of this move, and the outcome happens strictly after it.
	for m := 0; m+1 < len(out); m++ {
		out[m].up = out[m+1].up0()
	}
	if len(out) > 0 {
		out = out[:len(out)-1]
	}
	return out
}

// up0 is the direction of the move itself (x[0]), before labels shift.
func (e moveEx) up0() bool { return e.x[0] > 0 }

func sign(x float64) int {
	switch {
	case x > 0:
		return 1
	case x < 0:
		return -1
	}
	return 0
}

// logit is an L2-regularised logistic regression on standardised features.
type logit struct {
	use      []int
	mean, sd []float64
	w        []float64 // w[0] intercept
}

func fitLogit(tr []moveEx, use []int) logit {
	if use == nil {
		for f := 0; f < nDirFeatures; f++ {
			use = append(use, f)
		}
	}
	m := logit{use: use, mean: make([]float64, len(use)), sd: make([]float64, len(use)), w: make([]float64, len(use)+1)}
	n := float64(len(tr))
	for fi, f := range use {
		for _, e := range tr {
			m.mean[fi] += e.x[f]
		}
		m.mean[fi] /= n
		for _, e := range tr {
			d := e.x[f] - m.mean[fi]
			m.sd[fi] += d * d
		}
		m.sd[fi] = math.Sqrt(m.sd[fi]/n) + 1e-9
	}
	// Newton's method: few features, so the Hessian is tiny.
	d := len(use) + 1
	z := make([]float64, d)
	for it := 0; it < 25; it++ {
		g := make([]float64, d)
		h := make([][]float64, d)
		for a := range h {
			h[a] = make([]float64, d)
			h[a][a] = 1e-3 * n // ridge
		}
		for a := 1; a < d; a++ {
			g[a] = 1e-3 * n * m.w[a]
		}
		for _, e := range tr {
			z[0] = 1
			for fi, f := range use {
				z[fi+1] = (e.x[f] - m.mean[fi]) / m.sd[fi]
			}
			p := m.raw(z)
			y := 0.0
			if e.up {
				y = 1
			}
			for a := 0; a < d; a++ {
				g[a] += (p - y) * z[a]
				for b := 0; b < d; b++ {
					h[a][b] += p * (1 - p) * z[a] * z[b]
				}
			}
		}
		step := solve(h, g)
		done := true
		for a := range m.w {
			m.w[a] -= step[a]
			if math.Abs(step[a]) > 1e-6 {
				done = false
			}
		}
		if done {
			break
		}
	}
	return m
}

func (m logit) raw(z []float64) float64 {
	s := 0.0
	for a, v := range z {
		s += m.w[a] * v
	}
	return 1 / (1 + math.Exp(-s))
}

func (m logit) prob(e moveEx) float64 {
	z := make([]float64, len(m.w))
	z[0] = 1
	for fi, f := range m.use {
		z[fi+1] = (e.x[f] - m.mean[fi]) / m.sd[fi]
	}
	return m.raw(z)
}

// solve does Gaussian elimination with partial pivoting on a copy.
func solve(a [][]float64, b []float64) []float64 {
	n := len(b)
	m := make([][]float64, n)
	for i := range m {
		m[i] = append(append([]float64(nil), a[i]...), b[i])
	}
	for c := 0; c < n; c++ {
		p := c
		for r := c + 1; r < n; r++ {
			if math.Abs(m[r][c]) > math.Abs(m[p][c]) {
				p = r
			}
		}
		m[c], m[p] = m[p], m[c]
		for r := c + 1; r < n; r++ {
			f := m[r][c] / m[c][c]
			for k := c; k <= n; k++ {
				m[r][k] -= f * m[c][k]
			}
		}
	}
	x := make([]float64, n)
	for r := n - 1; r >= 0; r-- {
		s := m[r][n]
		for k := r + 1; k < n; k++ {
			s -= m[r][k] * x[k]
		}
		x[r] = s / m[r][r]
	}
	return x
}

// scored accumulates out-of-sample results for the three predictors
// (0 base rate, 1 last move, 2 order flow), keeping per-move rows so groups
// can be scored afterwards.
type scored struct {
	period  string
	n       int
	ups     float64
	acc, ll [3]float64
	sure    int
	sureOK  float64
	rows    []dirRow
}

type dirRow struct {
	market int
	up     bool
	p      [3]float64
}

func (s *scored) add(e moveEx, ps ...float64) {
	var r dirRow
	r.market, r.up = e.market, e.up
	copy(r.p[:], ps)
	s.addRow(r)
}

func (s *scored) addRow(r dirRow) {
	s.n++
	s.rows = append(s.rows, r)
	y := 0.0
	if r.up {
		y = 1
		s.ups++
	}
	for i, p := range r.p {
		p = math.Min(math.Max(p, 1e-6), 1-1e-6)
		if (p >= 0.5) == r.up {
			s.acc[i]++
		}
		s.ll[i] -= y*math.Log(p) + (1-y)*math.Log(1-p)
	}
	if p := r.p[2]; p >= 0.6 || p <= 0.4 {
		s.sure++
		if (p >= 0.5) == r.up {
			s.sureOK++
		}
	}
}

func (s *scored) merge(o scored) {
	for _, r := range o.rows {
		s.addRow(r)
	}
}

func (s scored) sub(markets []int) scored {
	in := map[int]bool{}
	for _, m := range markets {
		in[m] = true
	}
	var out scored
	for _, r := range s.rows {
		if in[r.market] {
			out.addRow(r)
		}
	}
	return out
}

func (s scored) print(ind string) {
	n := float64(s.n)
	names := []string{"base rate", "last move", "order flow"}
	for i, name := range names {
		fmt.Printf("%s%-11s accuracy %5.1f%%  log-loss %.4f\n", ind, name, 100*s.acc[i]/n, s.ll[i]/n)
	}
	fmt.Printf("%ssure calls  %4.1f%% of moves, right %5.1f%% of the time\n", ind, 100*float64(s.sure)/n, 100*s.sureOK/float64(max(s.sure, 1)))
}

// loadStreams reads market names and sectors without loading trades.
func loadStreams(dir string) ([]stream, error) {
	u, err := core.LoadUniverse(filepath.Join(dir, "universe.txt"))
	if err != nil {
		return nil, err
	}
	var streams []stream
	for i := 0; i+1 < len(u.Entities); i += 2 {
		e := u.Entities[i]
		sec, ev, _ := strings.Cut(e.Subtopic, "/")
		streams = append(streams, stream{name: strings.TrimPrefix(e.Name, "Activity: "), sector: strings.TrimSuffix(sec, "-activity"), event: ev})
	}
	return streams, nil
}

// directionGroups reuses the topic and lifespan split from the Hawkes test.
func directionGroups(streams []stream, ticks [][]sTick) []marketGroup {
	bySec := map[string][]int{}
	var short, long []int
	for i, s := range streams {
		bySec[s.sector] = append(bySec[s.sector], i)
		if tk := ticks[i]; len(tk) > 0 && tk[len(tk)-1].t-tk[0].t < 30*86400 {
			short = append(short, i)
		} else {
			long = append(long, i)
		}
	}
	var gs []marketGroup
	for _, sec := range []string{"world", "politics", "econ", "crypto", "tech", "sports", "culture", "other"} {
		if len(bySec[sec]) > 0 {
			gs = append(gs, marketGroup{"topic: " + sec, bySec[sec]})
		}
	}
	return append(gs, marketGroup{"short-lived (<30d)", short}, marketGroup{"long-running", long})
}
