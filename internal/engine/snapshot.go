package engine

import (
	"math"
	"sort"
)

const histLevels = 96

// Snapshot is a JSON-ready view of the engine for the dashboard.
type Snapshot struct {
	Time     int64        `json:"time"`
	Bar      int          `json:"bar"`
	BarSec   int64        `json:"barSec"`
	Horizon  int          `json:"horizon"`
	Disl     []DislView   `json:"disl"`
	Shocks   []ShockView  `json:"shocks"`
	Edges    []EdgeView   `json:"edges"`
	Factors  []FactorView `json:"factors"`
	Counters Counters     `json:"counters"`
}

// DislView is one entity whose level lags what its inputs imply.
type DislView struct {
	Entity  int          `json:"e"`
	Gap     float64      `json:"gap"`   // % the model expects beyond decay within H
	Prob    float64      `json:"prob"`  // calibrated probability the move happens
	Catch   int          `json:"catch"` // bars to 90% of the move
	Past    []float64    `json:"past"`  // recent levels, % vs now
	Fc      []float64    `json:"fc"`    // forecast path, % vs now
	Fd      []float64    `json:"fd"`    // decay-only path, % vs now
	Drivers []DriverView `json:"drivers"`
}

// DriverView names a source of a forecast move (Entity -1 = cluster factor).
type DriverView struct {
	Entity int     `json:"e"`
	Pct    float64 `json:"pct"`
}

// ShockView is a detected shock and its spread.
type ShockView struct {
	Entity   int         `json:"e"`
	Time     int64       `json:"time"`
	Age      int         `json:"age"`
	Z        float64     `json:"z"`
	Size     float64     `json:"size"` // %
	EchoOf   int         `json:"echoOf"`
	Children []ChildView `json:"children"`
}

// ChildView is one predicted downstream move.
type ChildView struct {
	Entity   int     `json:"e"`
	Depth    int     `json:"depth"`
	Prob     float64 `json:"prob"`
	Pred     float64 `json:"pred"`     // % expected by now
	Final    float64 `json:"final"`    // % expected in total
	Realized float64 `json:"realized"` // % so far
	Peak     int     `json:"peak"`
	Tested   bool    `json:"tested"`
}

// EdgeView is one live lead-lag edge.
type EdgeView struct {
	From   int     `json:"from"`
	To     int     `json:"to"`
	Lag    int     `json:"lag"`
	Weight float64 `json:"w"`
	Hit    float64 `json:"hit"` // -1 if untested
}

// FactorView is a cluster's attention factor and who is off it.
type FactorView struct {
	Cluster int          `json:"c"`
	Move    float64      `json:"move"` // % over the window
	Off     []FactorItem `json:"off"`
}

// FactorItem compares an entity's move with what its factor loading implies.
type FactorItem struct {
	Entity   int     `json:"e"`
	Expected float64 `json:"exp"`
	Actual   float64 `json:"act"`
}

func pct(logMove float64) float64 { return 100 * (math.Exp(logMove) - 1) }

func round(x float64, d int) float64 {
	p := math.Pow(10, float64(d))
	return math.Round(x*p) / p
}

// Snapshot builds the dashboard view. topN limits each list.
func (e *Engine) Snapshot(topN int) *Snapshot {
	t := e.T - 1
	H := e.cfg.Horizon
	s := &Snapshot{Time: e.BarTime(), Bar: e.T, BarSec: e.cfg.BarSeconds, Horizon: H, Counters: e.stats,
		Disl: []DislView{}, Shocks: []ShockView{}, Edges: []EdgeView{}, Factors: []FactorView{}}
	if t < 1 {
		return s
	}

	// Dislocations: rank by calibrated confidence, then size.
	idx := make([]int, 0, e.n)
	for j := 0; j < e.n; j++ {
		if e.Sig[j] > 0 && math.Abs(e.Disl[j]) > 1e-4 {
			idx = append(idx, j)
		}
	}
	sort.Slice(idx, func(a, b int) bool {
		sa, sb := math.Abs(e.Disl[idx[a]])/e.Sig[idx[a]], math.Abs(e.Disl[idx[b]])/e.Sig[idx[b]]
		return sa > sb
	})
	if len(idx) > topN {
		idx = idx[:topN]
	}
	past := min(48, t+1, histLevels)
	for _, j := range idx {
		d := DislView{Entity: j, Gap: round(pct(e.Disl[j]), 2), Prob: round(e.Prob(j), 3), Catch: e.Catch[j], Drivers: []DriverView{}}
		for k := past - 1; k >= 0; k-- {
			d.Past = append(d.Past, round(pct(e.levels[(t-k)%histLevels][j]-e.x[j]), 2))
		}
		for h := 0; h <= H; h++ {
			d.Fc = append(d.Fc, round(pct(e.fc[h][j]-e.x[j]), 2))
			d.Fd = append(d.Fd, round(pct(e.fd[h][j]-e.x[j]), 2))
		}
		for _, dr := range e.Drivers(j) {
			if len(d.Drivers) == 3 || math.Abs(dr.Contrib) < 0.1*math.Abs(e.Disl[j]) {
				break
			}
			d.Drivers = append(d.Drivers, DriverView{Entity: dr.Entity, Pct: round(pct(dr.Contrib), 2)})
		}
		s.Disl = append(s.Disl, d)
	}

	// Shocks, newest first.
	for k := len(e.Shocks) - 1; k >= 0 && len(s.Shocks) < topN; k-- {
		sh := e.Shocks[k]
		age := t - sh.Bar
		if age > 4*H {
			break
		}
		v := ShockView{Entity: sh.Source, Time: sh.Time, Age: age, Z: round(sh.Z, 1), Size: round(pct(sh.Size), 1), EchoOf: sh.EchoOf, Children: []ChildView{}}
		for _, c := range sh.Children {
			if len(v.Children) == 6 {
				break
			}
			a := min(age, H)
			v.Children = append(v.Children, ChildView{
				Entity: c.Entity, Depth: c.Depth, Prob: round(c.Prob, 2), Peak: c.PeakBar,
				Pred: round(pct(c.Pred[a]), 2), Final: round(pct(c.Final()), 2), Realized: round(pct(c.Realized), 2), Tested: c.Tested,
			})
		}
		s.Shocks = append(s.Shocks, v)
	}

	for _, p := range e.Edges() {
		hit := -1.0
		if p.Trials > 0 {
			hit = round(p.HitRate(), 2)
		}
		s.Edges = append(s.Edges, EdgeView{From: p.From, To: p.To, Lag: p.Lag, Weight: round(p.Weight, 3), Hit: hit})
	}

	// Factors over a window: cluster move vs each member's implied and actual move.
	W := min(30, t, e.hist-1)
	for c, mem := range e.members {
		F := 0.0
		for k := 0; k < W; k++ {
			F += e.Factor(c, t-k)
		}
		fv := FactorView{Cluster: c, Move: round(pct(F), 2)}
		var items []FactorItem
		for _, j := range mem {
			_, tot := e.Loading(j)
			act := 0.0
			for k := 0; k < W; k++ {
				act += e.R(j, t-k)
			}
			items = append(items, FactorItem{Entity: j, Expected: tot * F, Actual: act})
		}
		sort.Slice(items, func(a, b int) bool {
			return math.Abs(items[a].Actual-items[a].Expected) > math.Abs(items[b].Actual-items[b].Expected)
		})
		for _, it := range items[:min(4, len(items))] {
			fv.Off = append(fv.Off, FactorItem{Entity: it.Entity, Expected: round(pct(it.Expected), 2), Actual: round(pct(it.Actual), 2)})
		}
		s.Factors = append(s.Factors, fv)
	}
	return s
}
