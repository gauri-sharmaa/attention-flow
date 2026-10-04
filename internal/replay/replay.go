// Package replay drives a recorded event stream through the exact engine used
// live and scores everything it predicted against what happened next.
//
// Every number here is out-of-sample: a forecast made at bar t is scored at
// bar t+H using only what the engine knew at t, and the models only learn from
// a bar after scoring it.
package replay

import (
	"math"
	"sort"
	"time"

	"github.com/gauri-sharmaa/attention-flow/internal/core"
	"github.com/gauri-sharmaa/attention-flow/internal/engine"
	"github.com/gauri-sharmaa/attention-flow/internal/semantic"
	"github.com/gauri-sharmaa/attention-flow/internal/sim"
)

// Options configures a replay.
type Options struct {
	EvalStart   int        // first bar whose forecasts are scored
	Truth       *sim.Truth // optional answer key (simulated data only)
	Checkpoints []int      // bars at which to score the learned graph against Truth
	OnBar       func(*engine.Engine)
}

// Report is the full scorecard.
type Report struct {
	Data     DataInfo        `json:"data"`
	Forecast ForecastScore   `json:"forecast"`
	Signals  []Decile        `json:"signals"`
	Calib    []CalibBin      `json:"calibration"`
	Brier    float64         `json:"brier"`
	BrierRef float64         `json:"brier_coinflip"`
	Shocks   ShockScore      `json:"shocks"`
	Graph    []GraphScore    `json:"graph,omitempty"`
	Latency  Latency         `json:"latency"`
	Engine   engine.Counters `json:"engine"`
}

// DataInfo describes the replayed stream.
type DataInfo struct {
	Entities, Bars, Events, Candidates int
	BarSeconds                         int64
	Horizon                            int
}

// ForecastScore compares H-bar level forecasts. "Excess" is the move beyond
// the decay-only path, i.e. the part the graph and factor claim to predict.
type ForecastScore struct {
	N          int     `json:"n"`
	MAEZero    float64 `json:"mae_zero"`
	MAEDecay   float64 `json:"mae_decay"`
	MAEFull    float64 `json:"mae_full"`
	R2Decay    float64 `json:"r2_decay"`
	R2Full     float64 `json:"r2_full"`
	ExcessR2   float64 `json:"excess_r2"`   // 1 - SSE(excess - disl)/SS(excess)
	ExcessCorr float64 `json:"excess_corr"` // corr(dislocation, realised excess)
}

// Decile is signal quality for one decile of |dislocation|/σ (10 = strongest).
type Decile struct {
	Decile  int     `json:"decile"`
	N       int     `json:"n"`
	MeanAbs float64 `json:"mean_abs_disl"`
	HitRate float64 `json:"hit_rate"` // realised excess has the predicted sign
	Capture float64 `json:"capture"`  // Σ realised·sign / Σ |predicted|
}

// CalibBin compares stated confidence with realised hit frequency.
type CalibBin struct {
	Lo, Hi  float64
	N       int
	MeanP   float64
	HitRate float64
}

// ShockScore checks predicted shock spread against realised moves.
type ShockScore struct {
	Shocks     int     `json:"shocks"`
	Children   int     `json:"children"`
	Corr       float64 `json:"corr"`        // predicted vs realised child moves
	SignHit    float64 `json:"sign_hit"`    // child moved the predicted way
	BigHit     float64 `json:"big_hit"`     // ... and at least half as far
	TrueChild  float64 `json:"true_child"`  // predicted children that truly descend (sim only)
	SourceTrue float64 `json:"source_true"` // detected shocks matching a planted one (sim only)
	Recall     float64 `json:"recall"`      // planted shocks that were detected (sim only)
	ByDepth    []DepthScore
}

// DepthScore is shock accuracy by hop distance.
type DepthScore struct {
	Depth   int
	N       int
	SignHit float64
	Corr    float64
}

// GraphScore compares the learned graph with the planted one at one bar.
type GraphScore struct {
	Bar          int     `json:"bar"`
	Learned      int     `json:"learned"`
	True         int     `json:"true"`
	Precision    float64 `json:"precision"`
	Recall       float64 `json:"recall"`
	RecallInCand float64 `json:"recall_in_candidates"`
	CandRecall   float64 `json:"candidate_recall"`
	LagExact     float64 `json:"lag_exact"`
	LagWithin1   float64 `json:"lag_within_1"`
	Indirect     int     `json:"false_indirect"` // false edges that shortcut a true path
	Reversed     int     `json:"false_reversed"`
	Spurious     int     `json:"false_other"`
}

// Latency summarises engine cost.
type Latency struct {
	IngestNsP50 float64 `json:"ingest_ns_p50"`
	CloseUsP50  float64 `json:"close_us_p50"`
	CloseUsP99  float64 `json:"close_us_p99"`
	CloseUsMax  float64 `json:"close_us_max"`
	EventsPerS  float64 `json:"events_per_sec"`
	WallSeconds float64 `json:"wall_seconds"`
}

type pending struct {
	x, fd, fc, sig []float64
	valid          bool
}

type sample struct{ score, disl, excess, p float64 }

// Run replays evs through a fresh engine and scores it.
func Run(u *core.Universe, evs []core.Event, cand []semantic.Pair, cfg engine.Config, opt Options) *Report {
	e := engine.New(u, cand, cfg)
	n := len(u.Entities)
	H := cfg.Horizon
	ring := make([]pending, H+1)
	for i := range ring {
		ring[i] = pending{x: make([]float64, n), fd: make([]float64, n), fc: make([]float64, n), sig: make([]float64, n)}
	}
	var samples []sample
	var sumAbs [3]float64
	var sse [3]float64
	var sst float64
	nScored := 0

	var shocks []*engine.Shock
	e.SetShockHook(func(s *engine.Shock) { shocks = append(shocks, s) })

	rep := &Report{}
	checkIdx := 0
	onClose := func() {
		t := e.T - 1
		// Score forecasts made H bars ago.
		if old := &ring[(t-H+len(ring)*4)%len(ring)]; old.valid && t-H >= opt.EvalStart {
			for j := 0; j < n; j++ {
				y := e.Level(j) - old.x[j]
				exc := e.Level(j) - old.fd[j]
				disl := old.fc[j] - old.fd[j]
				preds := [3]float64{0, old.fd[j] - old.x[j], old.fc[j] - old.x[j]}
				for k, p := range preds {
					sumAbs[k] += math.Abs(y - p)
					sse[k] += (y - p) * (y - p)
				}
				sst += y * y
				nScored++
				sc := 0.0
				if old.sig[j] > 0 {
					sc = math.Abs(disl) / old.sig[j]
				}
				p := 0.5 * math.Erfc(-sc/math.Sqrt2)
				samples = append(samples, sample{score: sc, disl: disl, excess: exc, p: p})
			}
		}
		cur := &ring[t%len(ring)]
		for j := 0; j < n; j++ {
			cur.x[j] = e.Level(j)
			cur.fd[j] = e.DecayPath(j, H)
			cur.fc[j] = e.FullPath(j, H)
			cur.sig[j] = e.Sig[j]
		}
		cur.valid = true
		for checkIdx < len(opt.Checkpoints) && t >= opt.Checkpoints[checkIdx] {
			if opt.Truth != nil {
				rep.Graph = append(rep.Graph, scoreGraph(e, opt.Truth, t))
			}
			checkIdx++
		}
		if opt.OnBar != nil {
			opt.OnBar(e)
		}
	}

	ingestNs := make([]float64, 0, len(evs))
	var closeUs []float64
	start := time.Now()
	lastT := 0
	for _, ev := range evs {
		t0 := time.Now()
		e.Ingest(ev)
		d := time.Since(t0)
		if e.T != lastT {
			// This event crossed a bar boundary and paid for the close(s).
			closeUs = append(closeUs, float64(d.Nanoseconds())/1e3/float64(e.T-lastT))
			for ; lastT < e.T; lastT++ {
				onClose()
			}
		} else {
			ingestNs = append(ingestNs, float64(d.Nanoseconds()))
		}
	}
	e.Flush()
	for ; lastT < e.T; lastT++ {
		onClose()
	}
	wall := time.Since(start).Seconds()

	rep.Data = DataInfo{Entities: n, Bars: e.T, Events: len(evs), Candidates: e.NumPairs(), BarSeconds: cfg.BarSeconds, Horizon: H}
	if nScored > 0 {
		f := &rep.Forecast
		f.N = nScored
		f.MAEZero, f.MAEDecay, f.MAEFull = sumAbs[0]/float64(nScored), sumAbs[1]/float64(nScored), sumAbs[2]/float64(nScored)
		f.R2Decay = 1 - sse[1]/sst
		f.R2Full = 1 - sse[2]/sst
		var se, ss float64
		xs, ys := make([]float64, len(samples)), make([]float64, len(samples))
		for i, s := range samples {
			se += (s.excess - s.disl) * (s.excess - s.disl)
			ss += s.excess * s.excess
			xs[i], ys[i] = s.disl, s.excess
		}
		f.ExcessR2 = 1 - se/ss
		f.ExcessCorr = corr(xs, ys)
	}
	rep.Signals = deciles(samples)
	rep.Calib, rep.Brier, rep.BrierRef = calibration(samples)
	rep.Shocks = scoreShocks(shocks, opt.Truth, e, cfg)
	rep.Latency = Latency{
		IngestNsP50: pct(ingestNs, 0.5),
		CloseUsP50:  pct(closeUs, 0.5), CloseUsP99: pct(closeUs, 0.99), CloseUsMax: pct(closeUs, 1),
		EventsPerS: float64(len(evs)) / wall, WallSeconds: wall,
	}
	rep.Engine = e.Counters()
	return rep
}

func pct(xs []float64, q float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	i := int(q * float64(len(s)-1))
	return s[i]
}

func corr(x, y []float64) float64 {
	n := float64(len(x))
	if n < 2 {
		return math.NaN()
	}
	var mx, my float64
	for i := range x {
		mx += x[i]
		my += y[i]
	}
	mx /= n
	my /= n
	var sxy, sxx, syy float64
	for i := range x {
		dx, dy := x[i]-mx, y[i]-my
		sxy += dx * dy
		sxx += dx * dx
		syy += dy * dy
	}
	return sxy / math.Sqrt(sxx*syy)
}

func deciles(ss []sample) []Decile {
	if len(ss) < 10 {
		return nil
	}
	s := append([]sample(nil), ss...)
	sort.Slice(s, func(i, j int) bool { return s[i].score < s[j].score })
	out := make([]Decile, 10)
	for d := 0; d < 10; d++ {
		part := s[d*len(s)/10 : (d+1)*len(s)/10]
		var hits int
		var abs, capNum, capDen float64
		for _, x := range part {
			sg := math.Copysign(1, x.disl)
			if x.excess*sg > 0 {
				hits++
			}
			abs += math.Abs(x.disl)
			capNum += x.excess * sg
			capDen += math.Abs(x.disl)
		}
		out[d] = Decile{Decile: d + 1, N: len(part), MeanAbs: abs / float64(len(part)),
			HitRate: float64(hits) / float64(len(part)), Capture: capNum / capDen}
	}
	return out
}

func calibration(ss []sample) ([]CalibBin, float64, float64) {
	edges := []float64{0.5, 0.55, 0.6, 0.65, 0.7, 0.75, 0.8, 0.85, 0.9, 0.95, 1.0001}
	bins := make([]CalibBin, len(edges)-1)
	for i := range bins {
		bins[i].Lo, bins[i].Hi = edges[i], edges[i+1]
	}
	var brier, ref float64
	for _, s := range ss {
		hit := 0.0
		if s.excess*math.Copysign(1, s.disl) > 0 {
			hit = 1
		}
		brier += (s.p - hit) * (s.p - hit)
		ref += 0.25
		for i := range bins {
			if s.p >= bins[i].Lo && s.p < bins[i].Hi {
				bins[i].N++
				bins[i].MeanP += s.p
				bins[i].HitRate += hit
				break
			}
		}
	}
	for i := range bins {
		if bins[i].N > 0 {
			bins[i].MeanP /= float64(bins[i].N)
			bins[i].HitRate /= float64(bins[i].N)
		}
	}
	if len(ss) == 0 {
		return bins, 0, 0
	}
	return bins, brier / float64(len(ss)), ref / float64(len(ss))
}

// trueGraph returns active planted edges at bar t keyed by (from,to).
func trueGraph(tr *sim.Truth, t int) map[[2]int]sim.TrueEdge {
	m := map[[2]int]sim.TrueEdge{}
	for _, e := range tr.Edges {
		if e.ActiveAt(t, tr.MidBar) {
			m[[2]int{e.From, e.To}] = e
		}
	}
	return m
}

// reach returns whether b is reachable from a in the planted graph (2+ hops).
func reach(adj map[int][]int, a, b int) bool {
	seen := map[int]bool{a: true}
	q := []int{a}
	for len(q) > 0 {
		x := q[0]
		q = q[1:]
		for _, y := range adj[x] {
			if y == b {
				return true
			}
			if !seen[y] {
				seen[y] = true
				q = append(q, y)
			}
		}
	}
	return false
}

func scoreGraph(e *engine.Engine, tr *sim.Truth, t int) GraphScore {
	truth := trueGraph(tr, t)
	adj := map[int][]int{}
	for k := range truth {
		adj[k[0]] = append(adj[k[0]], k[1])
	}
	g := GraphScore{Bar: t, True: len(truth)}
	learned := e.Edges()
	g.Learned = len(learned)
	found := map[[2]int]bool{}
	tp, exact, within := 0, 0, 0
	for _, le := range learned {
		k := [2]int{le.From, le.To}
		te, ok := truth[k]
		switch {
		case ok:
			tp++
			found[k] = true
			if le.Lag == te.Lag {
				exact++
			}
			if absInt(le.Lag-te.Lag) <= 1 {
				within++
			}
		case truth[[2]int{le.To, le.From}] != (sim.TrueEdge{}):
			g.Reversed++
		case reach(adj, le.From, le.To):
			g.Indirect++
		default:
			g.Spurious++
		}
	}
	inCand, candTotal := 0, 0
	for k := range truth {
		if e.IsCandidate(k[0], k[1]) {
			candTotal++
			if found[k] {
				inCand++
			}
		}
	}
	if g.Learned > 0 {
		g.Precision = float64(tp) / float64(g.Learned)
	}
	if g.True > 0 {
		g.Recall = float64(tp) / float64(g.True)
		g.CandRecall = float64(candTotal) / float64(g.True)
	}
	if candTotal > 0 {
		g.RecallInCand = float64(inCand) / float64(candTotal)
	}
	if tp > 0 {
		g.LagExact = float64(exact) / float64(tp)
		g.LagWithin1 = float64(within) / float64(tp)
	}
	return g
}

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func scoreShocks(shocks []*engine.Shock, tr *sim.Truth, e *engine.Engine, cfg engine.Config) ShockScore {
	ss := ShockScore{Shocks: len(shocks)}
	var xs, ys []float64
	var sign, big, trueKids int
	depthX := map[int][]float64{}
	depthY := map[int][]float64{}
	for _, s := range shocks {
		var adj map[int][]int
		if tr != nil {
			adj = map[int][]int{}
			for k := range trueGraph(tr, s.Bar) {
				adj[k[0]] = append(adj[k[0]], k[1])
			}
		}
		for _, c := range s.Children {
			p, r := c.Final(), c.Realized
			xs, ys = append(xs, p), append(ys, r)
			depthX[c.Depth] = append(depthX[c.Depth], p)
			depthY[c.Depth] = append(depthY[c.Depth], r)
			if p*r > 0 {
				sign++
				if math.Abs(r) >= 0.5*math.Abs(p) {
					big++
				}
			}
			if adj != nil && reach(adj, s.Source, c.Entity) {
				trueKids++
			}
		}
	}
	ss.Children = len(xs)
	if len(xs) > 0 {
		ss.Corr = corr(xs, ys)
		ss.SignHit = float64(sign) / float64(len(xs))
		ss.BigHit = float64(big) / float64(len(xs))
		if tr != nil {
			ss.TrueChild = float64(trueKids) / float64(len(xs))
		}
	}
	for d := 1; d <= 4; d++ {
		x, y := depthX[d], depthY[d]
		if len(x) == 0 {
			continue
		}
		h := 0
		for i := range x {
			if x[i]*y[i] > 0 {
				h++
			}
		}
		ss.ByDepth = append(ss.ByDepth, DepthScore{Depth: d, N: len(x), SignHit: float64(h) / float64(len(x)), Corr: corr(x, y)})
	}
	if tr != nil && len(shocks) > 0 {
		planted := map[[2]int]bool{}
		for _, p := range tr.Shocks {
			planted[[2]int{p.Bar, p.Entity}] = true
		}
		match := 0
		for _, s := range shocks {
			if planted[[2]int{s.Bar, s.Source}] {
				match++
			}
		}
		ss.SourceTrue = float64(match) / float64(len(shocks))
		// Recall over planted shocks large enough to matter, after warm-up.
		eligible, hit := 0, 0
		det := map[[2]int]bool{}
		for _, s := range shocks {
			det[[2]int{s.Bar, s.Source}] = true
		}
		for _, p := range tr.Shocks {
			if p.Bar < cfg.Warmup || p.Bar > e.T-cfg.Horizon-1 {
				continue
			}
			eligible++
			if det[[2]int{p.Bar, p.Entity}] {
				hit++
			}
		}
		if eligible > 0 {
			ss.Recall = float64(hit) / float64(eligible)
		}
	}
	return ss
}
