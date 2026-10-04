// Package engine is the streaming inference core.
//
// Events arrive in any order within a bar. Ingest is O(1): it only records the
// latest value. When the bar clock advances, Close runs one incremental update:
//
//  1. log-attention innovations r for every entity
//  2. cluster attention factors (cross-sectional GLS, leave-one-out per entity)
//  3. per-target RLS models score their one-step forecast, then learn from it
//  4. lead-lag statistics for every candidate pair, computed on model residuals
//  5. graph edits: promote significant new leaders, drop ones the model no longer needs
//  6. shock detection, then an H-bar forecast of every entity through the graph
//
// Nothing is ever refit from scratch, so the same code runs live and in replay.
package engine

import (
	"math"
	"sort"

	"github.com/gauri-sharmaa/attention-flow/internal/core"
	"github.com/gauri-sharmaa/attention-flow/internal/semantic"
	"github.com/gauri-sharmaa/attention-flow/internal/stats"
)

// Config tunes the engine. Times are in bars.
type Config struct {
	BarSeconds    int64
	MaxLag        int     // lead-lag search range 1..MaxLag
	Horizon       int     // forecast horizon H
	StatHalfLife  float64 // lead-lag statistics window
	ModelHalfLife float64 // RLS forgetting window
	FactorHalf    float64 // loading / variance window
	PromoteZ      float64 // z-score needed to add an edge
	GapZ          float64 // forward z must beat reverse z by this much
	DropT         float64 // |t| below which a mature edge is removed
	MinEdgeAge    int     // bars before an edge can be removed
	MaxInDeg      int
	DecideEvery   int
	Warmup        int // bars before any edge is added
	ShockZ        float64
	FactorLags    int
}

// DefaultConfig suits minute bars; scale the windows for hourly data.
func DefaultConfig(barSeconds int64) Config {
	return Config{
		BarSeconds: barSeconds, MaxLag: 8, Horizon: 16,
		StatHalfLife: 1500, ModelHalfLife: 2000, FactorHalf: 1000,
		PromoteZ: 5.5, GapZ: 2, DropT: 2.0, MinEdgeAge: 600,
		MaxInDeg: 6, DecideEvery: 10, Warmup: 800, ShockZ: 7, FactorLags: 3,
	}
}

// Edge is a learned lead-lag link From → To.
type Edge struct {
	From, To int
	Lag      int
	Z        float64 // promotion strength (residual cross-correlation z)
	Born     int     // bar it was added
	Weight   float64 // summed RLS coefficient across its lag window
	T        float64 // t-statistic of Weight
	Hits     int     // shock propagation hits
	// Health: EW E[v·c] and E[c²] where c is this edge's contribution to the
	// child's forecast and v the child's forecast error. A live edge has
	// E[v·c] ≈ 0; a dead one has its whole contribution show up as error,
	// E[v·c] ≈ -E[c²]. Health = E[v·c]/E[c²] reacts in a few hundred bars,
	// far faster than the RLS coefficient decays.
	vc, cc  float64
	healthN int
	Trials  int
}

// HitRate is the share of source shocks that propagated to the child.
func (e *Edge) HitRate() float64 {
	if e.Trials == 0 {
		return math.NaN()
	}
	return float64(e.Hits) / float64(e.Trials)
}

type featKind uint8

const (
	featParent featKind = iota
	featFactorNow
	featFactorLag
	featDecay
	featConst
)

type feat struct {
	kind featKind
	src  int // parent entity, or factor lag
	lag  int
}

type model struct {
	rls   *stats.RLS
	feats []feat
	x     []float64 // features for the bar being predicted
}

const (
	calBins  = 24
	calWidth = 0.25 // score units per bin; the last bin is open-ended
)

func calBin(score float64) int { return min(calBins-1, int(score/calWidth)) }

type pastFc struct {
	fd, disl, sig []float64 // raw (uncalibrated) sig
	valid         bool
}

type pairStat struct {
	a, b   int
	ab, ba []float64 // EW E[u_a(t-l) v_b(t)] and E[u_b(t-l) v_a(t)], index l-1
	sim    float64
}

// Engine is not safe for concurrent use; one goroutine owns it.
type Engine struct {
	cfg       Config
	U         *core.Universe
	n         int
	clusterOf []int
	members   [][]int

	started bool
	curBar  int64
	pending []float64

	T       int // closed bars
	hist    int
	x       []float64 // log level, last closed bar
	hasX    []bool
	missing []bool
	r       [][]float64 // r[t%hist][j]
	u       [][]float64 // factor-removed innovation (leader signal)
	f       [][]float64 // f[t%hist][c] full cluster factor
	fLOO    []float64   // leave-one-out factor at current bar, per entity
	lvl     []stats.EW  // slow baseline of the level
	rStd    []stats.EW
	uStd    []stats.EW
	vStd    []stats.EW
	fStd    []stats.EW // per cluster
	lam     []float64
	sig2    []float64
	covRF   []stats.EW
	varF    []stats.EW
	robust  []*stats.Robust

	alphaStat float64
	pairs     []pairStat
	pairIdx   map[[2]int]int
	parents   [][]*Edge
	models    []*model
	v         []float64 // current-bar model residual

	// Forecast state, refreshed every bar.
	fc    [][]float64 // fc[h][j], h=0..H: log level path, full model
	fd    [][]float64 // decay-only path
	rf    [][]float64 // forecast innovations
	Disl  []float64   // fc[H]-fd[H]
	Sig   []float64   // forecast std of the excess move, calibrated
	Catch []int       // bars until 90% of the dislocation is expected

	// Self-calibration: every forecast is scored H bars later and the running
	// mean squared standardised error rescales Sig, so stated confidence keeps
	// matching realised accuracy as the data drifts.
	past  []pastFc
	calib []stats.EW
	pBins [calBins][2]float64 // EW (hits, count) by score bin, pooled over entities

	Shocks    []*Shock
	onShock   ShockHook
	lastShock []int
	irR, irX  [][]float64
	bfsDepth  []int
	bfsProb   []float64
	bfsTested []bool
	children  [][]*Edge
	lagRows   [][]float64
	levels    [][]float64 // levels[t%histLevels][j], for charts
	stats     Counters
}

// Counters are cumulative engine statistics.
type Counters struct {
	Events, Bars, Promoted, Dropped, Shocks int
}

// New builds an engine for the universe with the given semantic candidates.
func New(u *core.Universe, cand []semantic.Pair, cfg Config) *Engine {
	n := len(u.Entities)
	e := &Engine{cfg: cfg, U: u, n: n, clusterOf: u.ClusterIndex()}
	e.members = make([][]int, len(u.Clusters))
	for j, c := range e.clusterOf {
		e.members[c] = append(e.members[c], j)
	}
	e.hist = cfg.MaxLag + cfg.Horizon + cfg.FactorLags + 4
	mk := func(rows, cols int) [][]float64 {
		m := make([][]float64, rows)
		for i := range m {
			m[i] = make([]float64, cols)
		}
		return m
	}
	e.pending = make([]float64, n)
	for j := range e.pending {
		e.pending[j] = math.NaN()
	}
	e.x, e.hasX, e.missing = make([]float64, n), make([]bool, n), make([]bool, n)
	e.r, e.u, e.f = mk(e.hist, n), mk(e.hist, n), mk(e.hist, len(u.Clusters))
	e.fLOO, e.v = make([]float64, n), make([]float64, n)
	aF := stats.Alpha(cfg.FactorHalf)
	e.lvl = make([]stats.EW, n)
	e.rStd, e.uStd, e.vStd = make([]stats.EW, n), make([]stats.EW, n), make([]stats.EW, n)
	e.covRF, e.varF = make([]stats.EW, n), make([]stats.EW, n)
	e.lam, e.sig2 = make([]float64, n), make([]float64, n)
	e.robust = make([]*stats.Robust, n)
	e.alphaStat = stats.Alpha(cfg.StatHalfLife)
	for j := 0; j < n; j++ {
		e.lvl[j].Alpha = stats.Alpha(4 * cfg.ModelHalfLife)
		e.rStd[j].Alpha, e.covRF[j].Alpha, e.varF[j].Alpha = aF, aF, aF
		e.uStd[j].Alpha, e.vStd[j].Alpha = e.alphaStat, e.alphaStat
		e.lam[j], e.sig2[j] = 1, 1
		e.robust[j] = stats.NewRobust(0.005)
	}
	e.fStd = make([]stats.EW, len(u.Clusters))
	for c := range e.fStd {
		e.fStd[c].Alpha = aF
	}
	e.pairIdx = map[[2]int]int{}
	for _, p := range cand {
		e.pairIdx[[2]int{p.A, p.B}] = len(e.pairs)
		e.pairs = append(e.pairs, pairStat{a: p.A, b: p.B, sim: p.Sim,
			ab: make([]float64, cfg.MaxLag), ba: make([]float64, cfg.MaxLag)})
	}
	e.parents = make([][]*Edge, n)
	e.models = make([]*model, n)
	for j := 0; j < n; j++ {
		e.models[j] = e.newModel(j, nil)
	}
	H := cfg.Horizon
	e.fc, e.fd, e.rf = mk(H+1, n), mk(H+1, n), mk(H+1, n)
	e.Disl, e.Sig, e.Catch = make([]float64, n), make([]float64, n), make([]int, n)
	e.irR, e.irX = mk(H+1, n), mk(H+1, n)
	e.bfsDepth, e.bfsProb, e.bfsTested = make([]int, n), make([]float64, n), make([]bool, n)
	e.lastShock = make([]int, n)
	for j := range e.lastShock {
		e.lastShock[j] = math.MinInt32
	}
	e.children = make([][]*Edge, n)
	e.lagRows = make([][]float64, cfg.MaxLag)
	e.levels = mk(histLevels, n)
	e.past = make([]pastFc, H+1)
	for i := range e.past {
		e.past[i] = pastFc{fd: make([]float64, n), disl: make([]float64, n), sig: make([]float64, n)}
	}
	e.calib = make([]stats.EW, n)
	for j := range e.calib {
		e.calib[j].Alpha = stats.Alpha(cfg.ModelHalfLife)
	}
	return e
}

// Config returns the engine configuration.
func (e *Engine) Config() Config { return e.cfg }

// Counters returns cumulative statistics.
func (e *Engine) Counters() Counters { return e.stats }

// NumPairs is the number of candidate pairs under test.
func (e *Engine) NumPairs() int { return len(e.pairs) }

// IsCandidate reports whether (a,b) is a tested pair (either direction).
func (e *Engine) IsCandidate(a, b int) bool {
	_, ok := e.pairIdx[[2]int{min(a, b), max(a, b)}]
	return ok
}

func (e *Engine) featureList(j int) []feat {
	var fs []feat
	for _, p := range e.parents[j] {
		for l := max(1, p.Lag-1); l <= p.Lag+1; l++ {
			fs = append(fs, feat{kind: featParent, src: p.From, lag: l})
		}
	}
	fs = append(fs, feat{kind: featFactorNow})
	for k := 1; k <= e.cfg.FactorLags; k++ {
		fs = append(fs, feat{kind: featFactorLag, lag: k})
	}
	return append(fs, feat{kind: featDecay}, feat{kind: featConst})
}

// newModel builds j's RLS for its current parents, carrying over weights and
// covariance for features it already had so a graph edit does not reset learning.
func (e *Engine) newModel(j int, old *model) *model {
	fs := e.featureList(j)
	lambda := 1 - stats.Alpha(e.cfg.ModelHalfLife)
	m := &model{rls: stats.NewRLS(len(fs), lambda, 50), feats: fs, x: make([]float64, len(fs))}
	if old != nil {
		pos := map[feat]int{}
		for i, f := range old.feats {
			pos[f] = i
		}
		d, od := len(fs), len(old.feats)
		for a, fa := range fs {
			ia, ok := pos[fa]
			if !ok {
				continue
			}
			m.rls.W[a] = old.rls.W[ia]
			for b, fb := range fs {
				if ib, ok := pos[fb]; ok {
					m.rls.P[a*d+b] = old.rls.P[ia*od+ib]
				}
			}
		}
		m.rls.Resid = old.rls.Resid
		m.rls.N = old.rls.N
	}
	return m
}

// Ingest records one event. Events must be in non-decreasing bar order; within
// a bar any order is fine and the latest value wins.
func (e *Engine) Ingest(ev core.Event) {
	b := ev.TS / e.cfg.BarSeconds
	if !e.started {
		e.started, e.curBar = true, b
	}
	for e.curBar < b {
		e.Close()
	}
	if ev.Value > 0 && int(ev.Entity) < e.n {
		e.pending[ev.Entity] = ev.Value
	}
	e.stats.Events++
}

// BarTime is the start time (unix seconds) of the bar most recently closed.
func (e *Engine) BarTime() int64 { return (e.curBar - 1) * e.cfg.BarSeconds }

// Flush closes the bar in progress (end of stream).
func (e *Engine) Flush() {
	if e.started {
		e.Close()
	}
}

func (e *Engine) at(ring [][]float64, t int) []float64 { return ring[((t%e.hist)+e.hist)%e.hist] }

// R returns the innovation of entity j at bar t (0 if t is out of range).
func (e *Engine) R(j, t int) float64 {
	if t < 0 || t > e.T-1 || e.T-1-t >= e.hist {
		return 0
	}
	return e.at(e.r, t)[j]
}

// Level returns the current log level of j.
func (e *Engine) Level(j int) float64 { return e.x[j] }

// Close ends the current bar and runs one full incremental update.
func (e *Engine) Close() {
	t := e.T
	rt, ut := e.at(e.r, t), e.at(e.u, t)
	for j := 0; j < e.n; j++ {
		v := e.pending[j]
		e.pending[j] = math.NaN()
		rt[j], ut[j] = 0, 0
		if math.IsNaN(v) {
			e.missing[j] = true
			continue
		}
		lx := math.Log(v)
		e.missing[j] = !e.hasX[j]
		if e.hasX[j] {
			rt[j] = lx - e.x[j]
		}
		e.x[j], e.hasX[j] = lx, true
	}
	e.curBar++
	e.T++
	e.stats.Bars++
	if t == 0 {
		for j := 0; j < e.n; j++ {
			e.lvl[j].Add(e.x[j])
		}
		copy(e.levels[0], e.x)
		return
	}
	e.updateFactors(t)
	e.updateModels(t)
	e.updatePairs(t)
	if t >= e.cfg.Warmup && t%e.cfg.DecideEvery == 0 {
		e.editGraph(t)
	}
	e.forecast(t)
	e.detectShocks(t)
	e.trackShocks(t)
	for j := 0; j < e.n; j++ {
		e.lvl[j].Add(e.x[j])
	}
	copy(e.levels[t%histLevels], e.x)
}

func clip(x, lim float64) float64 {
	if x > lim {
		return lim
	}
	if x < -lim {
		return -lim
	}
	return x
}

// updateFactors estimates each cluster's attention factor by GLS across its
// members, f = Σ(λ/σ²)r / Σ(λ²/σ²), plus a leave-one-out version per entity so
// an entity's own move never explains itself. Inputs are winsorised so a single
// shock does not register as a sector-wide move.
func (e *Engine) updateFactors(t int) {
	rt, ft := e.at(e.r, t), e.at(e.f, t)
	for c, mem := range e.members {
		num, den := 0.0, 0.0
		for _, j := range mem {
			if e.missing[j] {
				continue
			}
			lim := 4 * math.Max(e.rStd[j].Std(), 1e-4)
			if e.rStd[j].N < 50 {
				lim = math.Inf(1)
			}
			w := e.lam[j] / e.sig2[j]
			num += w * clip(rt[j], lim)
			den += w * e.lam[j]
		}
		if den > 0 {
			ft[c] = num / den
		} else {
			ft[c] = 0
		}
		e.fStd[c].Add(ft[c])
		for _, j := range mem {
			if e.missing[j] {
				e.fLOO[j] = 0
				continue
			}
			lim := 4 * math.Max(e.rStd[j].Std(), 1e-4)
			if e.rStd[j].N < 50 {
				lim = math.Inf(1)
			}
			w := e.lam[j] / e.sig2[j]
			d := den - w*e.lam[j]
			if d <= 0 {
				e.fLOO[j] = 0
				continue
			}
			e.fLOO[j] = (num - w*clip(rt[j], lim)) / d
		}
	}
	// Loadings by EW regression of r on the leave-one-out factor, then
	// renormalised to mean 1 per cluster (the factor's scale is arbitrary).
	for j := 0; j < e.n; j++ {
		if e.missing[j] {
			continue
		}
		lim := 4 * math.Max(e.rStd[j].Std(), 1e-4)
		if e.rStd[j].N < 50 {
			lim = math.Inf(1)
		}
		rc := clip(rt[j], lim)
		e.rStd[j].Add(rt[j])
		e.covRF[j].Add(rc * e.fLOO[j])
		e.varF[j].Add(e.fLOO[j] * e.fLOO[j])
		if e.varF[j].N > 50 && e.varF[j].Mean > 0 {
			e.lam[j] = math.Max(0.05, e.covRF[j].Mean/e.varF[j].Mean)
		}
		res := rc - e.lam[j]*e.fLOO[j]
		e.sig2[j] = math.Max(1e-8, (1-e.covRF[j].Alpha)*e.sig2[j]+e.covRF[j].Alpha*res*res)
		if e.rStd[j].N < 50 {
			e.sig2[j] = math.Max(1e-8, e.rStd[j].Var)
		}
		e.uStd[j].Add(res)
		ut := e.at(e.u, t)
		ut[j] = clip(res, 5*math.Max(e.uStd[j].Std(), 1e-6))
	}
	for _, mem := range e.members {
		s, k := 0.0, 0
		for _, j := range mem {
			s += e.lam[j]
			k++
		}
		if k > 0 && s > 0 {
			for _, j := range mem {
				e.lam[j] *= float64(k) / s
			}
		}
	}
}

// fill writes j's features for predicting r at bar tgt, reading realised values
// for bars ≤ now and forecast values (rf) beyond it.
func (e *Engine) fill(j int, m *model, tgt, now int, fLOO float64, level, base float64) {
	for i, f := range m.feats {
		switch f.kind {
		case featParent:
			tau := tgt - f.lag
			if tau <= now {
				m.x[i] = e.R(f.src, tau)
			} else {
				m.x[i] = e.rf[tau-now][f.src]
			}
		case featFactorNow:
			m.x[i] = fLOO
		case featFactorLag:
			tau := tgt - f.lag
			if tau <= now && tau >= 0 && now-tau < e.hist {
				m.x[i] = e.at(e.f, tau)[e.clusterOf[j]]
			} else {
				m.x[i] = 0 // future factor innovations are unpredictable
			}
		case featDecay:
			m.x[i] = level - base
		case featConst:
			m.x[i] = 1
		}
	}
}

// updateModels scores each entity's one-step forecast for this bar (a-priori,
// before it learns from the bar) and then updates it.
func (e *Engine) updateModels(t int) {
	rt := e.at(e.r, t)
	for j := 0; j < e.n; j++ {
		m := e.models[j]
		prevLevel := e.x[j] - rt[j]
		e.fill(j, m, t, t-1, e.fLOO[j], prevLevel, e.lvl[j].Mean)
		if e.missing[j] {
			e.v[j] = 0
			continue
		}
		if len(e.parents[j]) > 0 {
			e.edgeHealth(j, m, rt[j])
		}
		e.v[j] = m.rls.Update(m.x, rt[j])
		lim := 5 * math.Max(e.vStd[j].Std(), 1e-6)
		if e.vStd[j].N < 50 {
			lim = math.Inf(1)
		}
		e.v[j] = clip(e.v[j], lim)
		e.vStd[j].Add(e.v[j])
	}
}

// edgeHealth updates every incoming edge's health statistic for one bar,
// using the a-priori forecast (before the RLS learns from this bar).
func (e *Engine) edgeHealth(j int, m *model, y float64) {
	v := y - m.rls.Predict(m.x)
	const a = 1 - 0.9977 // ≈ 300-bar half-life
	for _, p := range e.parents[j] {
		c := 0.0
		for i, f := range m.feats {
			if f.kind == featParent && f.src == p.From {
				c += m.rls.W[i] * m.x[i]
			}
		}
		p.vc += a * (v*c - p.vc)
		p.cc += a * (c*c - p.cc)
		p.healthN++
	}
}

// Health is the edge's recent E[v·c]/E[c²]: near 0 when the edge is earning its
// keep, near -1 when its predicted contribution keeps failing to show up.
func (p *Edge) Health() float64 {
	if p.cc <= 0 {
		return 0
	}
	return p.vc / p.cc
}

// updatePairs updates lagged cross-moments for every candidate pair in both
// directions: leader signal u (factor removed) against the target's model
// residual v. Testing on residuals means a leader is only "new" if the target's
// current parents do not already explain it, so chains a→b→c do not also
// produce a spurious a→c edge.
func (e *Engine) updatePairs(t int) {
	a := e.alphaStat
	L := min(e.cfg.MaxLag, t)
	rows := e.lagRows[:L]
	for l := 1; l <= L; l++ {
		rows[l-1] = e.at(e.u, t-l)
	}
	for k := range e.pairs {
		p := &e.pairs[k]
		va, vb := e.v[p.a], e.v[p.b]
		if e.missing[p.a] {
			va = 0
		}
		if e.missing[p.b] {
			vb = 0
		}
		ab, ba := p.ab[:L], p.ba[:L]
		for l, ul := range rows {
			ab[l] += a * (ul[p.a]*vb - ab[l])
			ba[l] += a * (ul[p.b]*va - ba[l])
		}
	}
}

func (e *Engine) zscore(m, su, sv float64) float64 {
	if su <= 0 || sv <= 0 {
		return 0
	}
	rho := clip(m/(su*sv), 0.999)
	n := stats.EffN(e.alphaStat)
	return math.Atanh(rho) * math.Sqrt(n-3)
}

func (e *Engine) hasEdge(from, to int) bool {
	for _, p := range e.parents[to] {
		if p.From == from {
			return true
		}
	}
	return false
}

// editGraph adds and removes edges.
//
// Add: for each candidate pair, find the strongest directed lag. Promote it if
// its z clears PromoteZ, beats the reverse direction by GapZ, and the target
// has room (or it beats the target's weakest edge).
// Remove: an edge whose summed coefficient has |t| < DropT after MinEdgeAge is
// no longer pulling its weight given the target's other inputs.
func (e *Engine) editGraph(t int) {
	changed := map[int]bool{}
	for j := 0; j < e.n; j++ {
		m := e.models[j]
		kept := e.parents[j][:0]
		for _, p := range e.parents[j] {
			w, se := e.edgeCoef(j, m, p)
			p.Weight, p.T = w, 0
			if se > 0 {
				p.T = w / se
			}
			dead := p.healthN > 300 && p.cc > 0 && p.vc/p.cc < -0.6
			if dead || (t-p.Born >= e.cfg.MinEdgeAge && math.Abs(p.T) < e.cfg.DropT) {
				e.stats.Dropped++
				changed[j] = true
				continue
			}
			kept = append(kept, p)
		}
		e.parents[j] = kept
	}

	type prop struct {
		from, to, lag int
		z             float64
	}
	var props []prop
	for k := range e.pairs {
		p := &e.pairs[k]
		bestAB, lagAB := e.bestLag(p.ab, e.uStd[p.a].Std(), e.vStd[p.b].Std())
		bestBA, lagBA := e.bestLag(p.ba, e.uStd[p.b].Std(), e.vStd[p.a].Std())
		switch {
		case bestAB >= e.cfg.PromoteZ && bestAB-bestBA >= e.cfg.GapZ:
			if !e.hasEdge(p.a, p.b) && !e.hasEdge(p.b, p.a) {
				props = append(props, prop{p.a, p.b, lagAB, bestAB})
			}
		case bestBA >= e.cfg.PromoteZ && bestBA-bestAB >= e.cfg.GapZ:
			if !e.hasEdge(p.b, p.a) && !e.hasEdge(p.a, p.b) {
				props = append(props, prop{p.b, p.a, lagBA, bestBA})
			}
		}
	}
	// Strongest first; at most one new parent per target per decision round so
	// the residual statistics can catch up before the next one is judged.
	sort.Slice(props, func(i, k int) bool { return props[i].z > props[k].z })
	added := map[int]bool{}
	for _, pr := range props {
		if added[pr.to] {
			continue
		}
		ps := e.parents[pr.to]
		if len(ps) >= e.cfg.MaxInDeg {
			weakest := 0
			for i := range ps {
				if math.Abs(ps[i].T) < math.Abs(ps[weakest].T) {
					weakest = i
				}
			}
			if t-ps[weakest].Born < e.cfg.MinEdgeAge || math.Abs(ps[weakest].T) > pr.z/2 {
				continue
			}
			ps = append(ps[:weakest], ps[weakest+1:]...)
			e.stats.Dropped++
		}
		e.parents[pr.to] = append(ps, &Edge{From: pr.from, To: pr.to, Lag: pr.lag, Z: pr.z, Born: t})
		added[pr.to] = true
		changed[pr.to] = true
		e.stats.Promoted++
		// Reset the pair's moments: they described the residual before this
		// edge existed and would otherwise keep re-proposing it.
		pi := e.pairIdx[[2]int{min(pr.from, pr.to), max(pr.from, pr.to)}]
		clear(e.pairs[pi].ab)
		clear(e.pairs[pi].ba)
	}
	for j := range changed {
		e.models[j] = e.newModel(j, e.models[j])
	}
	if len(changed) > 0 {
		for j := range e.children {
			e.children[j] = e.children[j][:0]
		}
		for _, ps := range e.parents {
			for _, p := range ps {
				e.children[p.From] = append(e.children[p.From], p)
			}
		}
	}
}

func (e *Engine) bestLag(mom []float64, su, sv float64) (float64, int) {
	// z is monotone in the moment for fixed su, sv: pick the lag first.
	best := 0
	for l, m := range mom {
		if m > mom[best] {
			best = l
		}
	}
	return e.zscore(mom[best], su, sv), best + 1
}

// edgeCoef returns the summed coefficient of edge p in j's model and its
// standard error, σ²·1ᵀP₍ₛ₎1 over the edge's lag block.
func (e *Engine) edgeCoef(j int, m *model, p *Edge) (w, se float64) {
	var idx []int
	for i, f := range m.feats {
		if f.kind == featParent && f.src == p.From {
			idx = append(idx, i)
			w += m.rls.W[i]
		}
	}
	d := m.rls.D
	v := 0.0
	for _, a := range idx {
		for _, b := range idx {
			v += m.rls.P[a*d+b]
		}
	}
	// With forgetting, P ≈ (Σ λ^k x xᵀ)⁻¹, so var(ŵ) ≈ σ²·P.
	s2 := m.rls.Resid.Var
	if v <= 0 || s2 <= 0 {
		return w, 0
	}
	return w, math.Sqrt(s2 * v)
}

// forecast projects every entity H bars ahead through the graph. Lags are ≥ 1,
// so step h only needs values from steps < h: one pass per step, no solver.
// It also runs a decay-only path; the gap between the two is the dislocation:
// the move the graph and factor say is coming that the level has not made yet.
func (e *Engine) forecast(t int) {
	H := e.cfg.Horizon
	defer func() { e.past[t%(H+1)].valid = true }()
	for j := 0; j < e.n; j++ {
		e.fc[0][j], e.fd[0][j], e.rf[0][j] = e.x[j], e.x[j], 0
	}
	for h := 1; h <= H; h++ {
		tgt := t + h
		for j := 0; j < e.n; j++ {
			m := e.models[j]
			w := m.rls.W
			base := e.lvl[j].Mean
			c := e.clusterOf[j]
			r := 0.0
			for i, f := range m.feats {
				switch f.kind {
				case featParent:
					if tau := tgt - f.lag; tau <= t {
						if tau >= 0 && t-tau < e.hist {
							r += w[i] * e.r[tau%e.hist][f.src]
						}
					} else {
						r += w[i] * e.rf[tau-t][f.src]
					}
				case featFactorLag:
					// Future factor innovations are unpredictable (zero).
					if tau := tgt - f.lag; tau >= 0 && tau <= t && t-tau < e.hist {
						r += w[i] * e.f[tau%e.hist][c]
					}
				case featDecay:
					r += w[i] * (e.fc[h-1][j] - base)
				case featConst:
					r += w[i]
				}
			}
			e.rf[h][j] = r
			e.fc[h][j] = e.fc[h-1][j] + r
			// Decay-only: same fitted decay and intercept, nothing else.
			dk, ck := w[len(m.feats)-2], w[len(m.feats)-1]
			e.fd[h][j] = e.fd[h-1][j] + dk*(e.fd[h-1][j]-base) + ck
		}
	}
	for j := 0; j < e.n; j++ {
		d := e.fc[H][j] - e.fd[H][j]
		e.Disl[j] = d
		m := e.models[j]
		// Excess-move uncertainty: H bars of model residual plus the
		// unpredictable part of the contemporaneous factor.
		lam0 := m.rls.W[len(m.feats)-2-e.cfg.FactorLags-1]
		fs := e.fStd[e.clusterOf[j]].Std()
		s2 := m.rls.Resid.Var + lam0*lam0*fs*fs
		raw := math.Sqrt(float64(H) * math.Max(s2, 1e-10))
		old, cur := &e.past[(t-H+(H+1)*1024)%(H+1)], &e.past[t%(H+1)]
		if t >= H && old.valid && !e.missing[j] && old.sig[j] > 0 {
			exc := e.x[j] - old.fd[j]
			z := (exc - old.disl[j]) / old.sig[j]
			e.calib[j].Add(math.Min(z*z, 25))
			if old.disl[j] != 0 {
				b := &e.pBins[calBin(math.Abs(old.disl[j])/old.sig[j])]
				hit := 0.0
				if exc*old.disl[j] > 0 {
					hit = 1
				}
				const a = 2e-5 // ≈ 35k pooled samples per bin half-life
				b[0] += a * (hit - b[0])
				b[1] += a * (1 - b[1])
			}
		}
		scale := 1.0
		if e.calib[j].N > 200 {
			scale = math.Min(3, math.Max(0.5, math.Sqrt(e.calib[j].Mean)))
		}
		e.Sig[j] = raw * scale
		cur.fd[j], cur.disl[j], cur.sig[j] = e.fd[H][j], d, raw
		e.Catch[j] = H
		if math.Abs(d) > 1e-12 {
			for h := 1; h <= H; h++ {
				if (e.fc[h][j]-e.fd[h][j])/d >= 0.9 {
					e.Catch[j] = h
					break
				}
			}
		}
	}
}

// Prob is the probability that j's excess move over H bars has the sign of its
// dislocation. It starts from the Gaussian Φ(|d|/σ) and, once enough forecasts
// have been scored, switches to the realised hit rate of past forecasts with a
// similar score (pooled across entities, monotone by construction).
func (e *Engine) Prob(j int) float64 {
	if e.Sig[j] <= 0 || e.Disl[j] == 0 {
		return 0.5
	}
	H := e.cfg.Horizon
	raw := e.past[(e.T-1)%(H+1)].sig[j]
	if raw <= 0 {
		return 0.5
	}
	sc := math.Abs(e.Disl[j]) / raw
	return e.calibrated(sc, stats.NormCDF(math.Abs(e.Disl[j])/e.Sig[j]))
}

// calibrated maps a score to a realised hit rate. Bins are merged upward
// (pool-adjacent-violators) so the curve never decreases.
func (e *Engine) calibrated(score, prior float64) float64 {
	var rate [calBins]float64
	var w [calBins]float64
	for i, b := range e.pBins {
		// Shrink thin bins toward the Gaussian prior with 0.002 pseudo-weight.
		const k = 0.002
		rate[i] = (b[0] + k*stats.NormCDF(float64(i)*calWidth+calWidth/2)) / (b[1] + k)
		w[i] = b[1] + k
	}
	if e.pBins[calBin(score)][1] < 0.01 {
		return prior
	}
	// Pool adjacent violators for a non-decreasing fit.
	type blk struct {
		v, w float64
		n    int
	}
	var st []blk
	for i := 0; i < calBins; i++ {
		st = append(st, blk{rate[i], w[i], 1})
		for len(st) > 1 && st[len(st)-2].v > st[len(st)-1].v {
			a, b := st[len(st)-2], st[len(st)-1]
			st = st[:len(st)-2]
			st = append(st, blk{(a.v*a.w + b.v*b.w) / (a.w + b.w), a.w + b.w, a.n + b.n})
		}
	}
	bin := calBin(score)
	for _, b := range st {
		if bin < b.n {
			return math.Max(0.5, b.v)
		}
		bin -= b.n
	}
	return prior
}

// DecayPath returns the decay-only forecast level for j at step h.
func (e *Engine) DecayPath(j, h int) float64 { return e.fd[h][j] }

// FullPath returns the full-model forecast level for j at step h.
func (e *Engine) FullPath(j, h int) float64 { return e.fc[h][j] }

// Edges returns all live edges.
func (e *Engine) Edges() []*Edge {
	var out []*Edge
	for _, ps := range e.parents {
		out = append(out, ps...)
	}
	return out
}

// Parents returns j's live incoming edges.
func (e *Engine) Parents(j int) []*Edge { return e.parents[j] }

// Loading returns j's immediate factor loading and its total (immediate +
// lagged) loading from the fitted model.
func (e *Engine) Loading(j int) (now, total float64) {
	m := e.models[j]
	for i, f := range m.feats {
		switch f.kind {
		case featFactorNow:
			now = m.rls.W[i]
			total += m.rls.W[i]
		case featFactorLag:
			total += m.rls.W[i]
		}
	}
	return
}

// Factor returns cluster c's factor innovation at bar t.
func (e *Engine) Factor(c, t int) float64 {
	if t < 0 || t > e.T-1 || e.T-1-t >= e.hist {
		return 0
	}
	return e.at(e.f, t)[c]
}

// Drivers returns j's largest first-hop forecast contributions over the horizon.
func (e *Engine) Drivers(j int) []Driver {
	m := e.models[j]
	t := e.T - 1
	contrib := map[int]float64{}
	fac := 0.0
	for h := 1; h <= e.cfg.Horizon; h++ {
		for i, f := range m.feats {
			tau := t + h - f.lag
			switch f.kind {
			case featParent:
				if tau <= t {
					contrib[f.src] += m.rls.W[i] * e.R(f.src, tau)
				}
			case featFactorLag:
				if tau <= t {
					fac += m.rls.W[i] * e.Factor(e.clusterOf[j], tau)
				}
			}
		}
	}
	out := make([]Driver, 0, len(contrib)+1)
	for src, c := range contrib {
		out = append(out, Driver{Entity: src, Contrib: c})
	}
	out = append(out, Driver{Entity: -1, Contrib: fac})
	sort.Slice(out, func(a, b int) bool { return math.Abs(out[a].Contrib) > math.Abs(out[b].Contrib) })
	return out
}

// Driver is one source of a forecast move; Entity -1 means the cluster factor.
type Driver struct {
	Entity  int
	Contrib float64
}
