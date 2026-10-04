// Package sim generates synthetic attention streams with a known answer key.
//
// Real attention data has no ground truth: nobody can tell you that "OpenAI
// leads ChatGPT by 3 minutes". So every model in the engine is first checked
// against this simulator, which plants exactly the structure the engine claims
// to find and records it in Truth:
//
//   - a latent attention factor per cluster (AI, crypto, tech), with some
//     entities responding to it immediately and some a few bars late
//   - directed lead-lag edges i → j: a move in i shows up in j l bars later
//   - heavy-tailed attention shocks that propagate along those edges
//   - attention decay back toward each entity's baseline
//   - a regime change halfway: some edges die, new ones appear
//   - missing observations
//
// Edges are planted mostly between semantically related entities (shared tags
// or subtopic), plus a few between unrelated ones, so the semantic candidate
// stage has a measurable recall below 100%.
package sim

import (
	"math"
	"math/rand/v2"
	"strings"

	"github.com/gauri-sharmaa/attention-flow/internal/core"
)

// Config controls the simulation.
type Config struct {
	Bars        int   // number of bars
	BarSeconds  int64 // bar width in seconds
	Start       int64 // unix seconds of bar 0
	Seed        uint64
	MaxLag      int     // planted lags are 1..MaxLag
	IdioStd     float64 // per-bar idiosyncratic noise (log units)
	FactorStd   float64 // per-bar cluster factor innovation
	Decay       float64 // pull toward baseline per bar
	ShockRate   float64 // shocks per entity per bar
	ShockScale  float64 // shock size in units of IdioStd
	MissingRate float64 // fraction of observations dropped
	SlowFrac    float64 // fraction of entities that respond to the factor late
	MaxInDeg    int
	RegimeFlip  float64 // fraction of edges replaced at the midpoint
}

// Default is a two-week, minute-bar run.
func Default() Config {
	return Config{
		Bars: 20000, BarSeconds: 60, Start: 1_780_000_000, Seed: 7,
		MaxLag: 8, IdioStd: 0.02, FactorStd: 0.015, Decay: 0.02,
		ShockRate: 1.0 / 1500, ShockScale: 8, MissingRate: 0.01,
		SlowFrac: 0.3, MaxInDeg: 4, RegimeFlip: 0.2,
	}
}

// TrueEdge is a planted lead-lag link.
type TrueEdge struct {
	From, To int
	Lag      int
	Beta     float64
	// Regime: 0 = active the whole run, 1 = first half only, 2 = second half only.
	Regime int
}

// Truth is the answer key for one run.
type Truth struct {
	Edges     []TrueEdge
	Loading   []float64 // immediate factor loading per entity
	SlowLoad  []float64 // delayed factor loading per entity
	SlowLag   []int     // delay of the slow loading (0 if none)
	Shocks    []Shock
	MidBar    int
	BaseLevel []float64
}

// Shock is a planted attention shock.
type Shock struct {
	Bar, Entity int
	Size        float64
}

// ActiveAt reports whether edge e is live at bar t.
func (e TrueEdge) ActiveAt(t, mid int) bool {
	switch e.Regime {
	case 1:
		return t < mid
	case 2:
		return t >= mid
	}
	return true
}

func related(a, b core.Entity) (sameSub, sharesTag bool) {
	sameSub = a.Subtopic == b.Subtopic
	words := map[string]bool{}
	for _, t := range a.Tags {
		words[t] = true
	}
	for _, w := range strings.Fields(strings.ToLower(a.Name)) {
		words[w] = true
	}
	for _, t := range b.Tags {
		if words[t] {
			return sameSub, true
		}
	}
	for _, w := range strings.Fields(strings.ToLower(b.Name)) {
		if words[w] {
			return sameSub, true
		}
	}
	return sameSub, false
}

// plantEdges draws a DAG over the universe; acyclic by construction because
// edges only go forward in a random permutation.
func plantEdges(u *core.Universe, cfg Config, rng *rand.Rand, regime int, indeg []int, exists map[[2]int]bool, want int) []TrueEdge {
	n := len(u.Entities)
	perm := rng.Perm(n)
	pos := make([]int, n)
	for i, p := range perm {
		pos[p] = i
	}
	var out []TrueEdge
	for tries := 0; tries < 50 && (want < 0 || len(out) < want); tries++ {
		for _, i := range rng.Perm(n) {
			for _, j := range rng.Perm(n) {
				if i == j || pos[i] >= pos[j] || exists[[2]int{i, j}] || exists[[2]int{j, i}] || indeg[j] >= cfg.MaxInDeg {
					continue
				}
				a, b := u.Entities[i], u.Entities[j]
				sameSub, shares := related(a, b)
				p := 0.002
				switch {
				case shares:
					p = 0.25
				case sameSub:
					p = 0.10
				case a.Cluster == b.Cluster:
					p = 0.006
				}
				if want >= 0 {
					p *= 0.2 // replacement edges: draw slowly so they spread out
				}
				if rng.Float64() >= p {
					continue
				}
				out = append(out, TrueEdge{
					From: i, To: j,
					Lag:    1 + rng.IntN(cfg.MaxLag),
					Beta:   0.3 + 0.4*rng.Float64(),
					Regime: regime,
				})
				exists[[2]int{i, j}] = true
				indeg[j]++
				if want >= 0 && len(out) >= want {
					return out
				}
			}
		}
		if want < 0 {
			break
		}
	}
	return out
}

// Run simulates the universe and returns the event stream plus its answer key.
func Run(u *core.Universe, cfg Config) ([]core.Event, *Truth) {
	rng := rand.New(rand.NewPCG(cfg.Seed, cfg.Seed^0x9e3779b97f4a7c15))
	n := len(u.Entities)
	clusterOf := u.ClusterIndex()
	tr := &Truth{
		Loading: make([]float64, n), SlowLoad: make([]float64, n), SlowLag: make([]int, n),
		MidBar: cfg.Bars / 2, BaseLevel: make([]float64, n),
	}

	indeg := make([]int, n)
	exists := map[[2]int]bool{}
	edges := plantEdges(u, cfg, rng, 0, indeg, exists, -1)
	// Regime change: retire a fraction of edges and plant the same number of new ones.
	nFlip := int(cfg.RegimeFlip * float64(len(edges)))
	for _, k := range rng.Perm(len(edges))[:nFlip] {
		edges[k].Regime = 1
		indeg[edges[k].To]-- // frees capacity for second-half edges
	}
	edges = append(edges, plantEdges(u, cfg, rng, 2, indeg, exists, nFlip)...)
	tr.Edges = edges

	for j := 0; j < n; j++ {
		lam := 0.4 + 0.8*rng.Float64()
		if rng.Float64() < cfg.SlowFrac {
			tr.Loading[j] = 0.35 * lam
			tr.SlowLoad[j] = 0.65 * lam
			tr.SlowLag[j] = 2 + rng.IntN(3)
		} else {
			tr.Loading[j] = lam
		}
		tr.BaseLevel[j] = 6 + 3*rng.Float64() // log attention ≈ 400..8000 units
	}

	// incoming[j] lists edges into j.
	incoming := make([][]TrueEdge, n)
	for _, e := range edges {
		incoming[e.To] = append(incoming[e.To], e)
	}

	hist := cfg.MaxLag + 2
	news := make([][]float64, hist) // news[t%hist][j]: innovation excluding decay
	for k := range news {
		news[k] = make([]float64, n)
	}
	nc := len(u.Clusters)
	fac := make([][]float64, 8) // factor innovations ring
	for k := range fac {
		fac[k] = make([]float64, nc)
	}
	x := append([]float64(nil), tr.BaseLevel...)
	evs := make([]core.Event, 0, cfg.Bars*n)

	for t := 0; t < cfg.Bars; t++ {
		cur := news[t%hist]
		f := fac[t%len(fac)]
		for c := range f {
			f[c] = cfg.FactorStd * rng.NormFloat64()
		}
		for j := 0; j < n; j++ {
			c := clusterOf[j]
			v := tr.Loading[j]*f[c] + cfg.IdioStd*rng.NormFloat64()
			if L := tr.SlowLag[j]; L > 0 && t >= L {
				v += tr.SlowLoad[j] * fac[(t-L)%len(fac)][c]
			}
			for _, e := range incoming[j] {
				if t < e.Lag+1 || !e.ActiveAt(t, tr.MidBar) {
					continue
				}
				// Most of the move lands at the planted lag, a little spills one bar later.
				v += e.Beta * (0.8*news[(t-e.Lag)%hist][e.From] + 0.2*news[(t-e.Lag-1)%hist][e.From])
			}
			if rng.Float64() < cfg.ShockRate {
				sz := cfg.IdioStd * cfg.ShockScale * (1 + rng.ExpFloat64())
				if rng.Float64() < 0.15 {
					sz = -sz
				}
				v += sz
				tr.Shocks = append(tr.Shocks, Shock{Bar: t, Entity: j, Size: sz})
			}
			cur[j] = v
		}
		ts := cfg.Start + int64(t)*cfg.BarSeconds
		for j := 0; j < n; j++ {
			x[j] += cur[j] - cfg.Decay*(x[j]-tr.BaseLevel[j])
			if rng.Float64() < cfg.MissingRate {
				continue
			}
			// Spread events across the bar so ingestion sees a realistic interleaving.
			off := int64(rng.IntN(int(cfg.BarSeconds)))
			evs = append(evs, core.Event{TS: ts + off, Entity: int32(j), Value: math.Exp(x[j])})
		}
	}
	sortEvents(evs)
	return evs, tr
}

// sortEvents sorts by timestamp with a radix-friendly stable insertion per bar
// (events are already grouped by bar, so this is near-linear).
func sortEvents(evs []core.Event) {
	for i := 1; i < len(evs); i++ {
		for k := i; k > 0 && evs[k].TS < evs[k-1].TS; k-- {
			evs[k], evs[k-1] = evs[k-1], evs[k]
		}
	}
}
