package engine

import (
	"math"
	"sort"
)

// Shock is a detected attention burst and its predicted spread.
type Shock struct {
	Source   int
	Bar      int     // bar index it was detected on
	Time     int64   // unix seconds of that bar
	Z        float64 // robust z-score of the move
	Size     float64 // log move above the source's typical move
	Children []*ShockChild
	Done     bool
}

// ShockChild is one entity the shock is predicted to reach.
type ShockChild struct {
	Entity   int
	Depth    int       // hops from the source
	Prob     float64   // product of edge hit rates along the path (smoothed)
	Pred     []float64 // predicted excess log move at h = 0..H
	base     []float64 // decay-only path at detection time
	Realized float64   // realised excess move so far
	PeakBar  int       // predicted bars to 90% of the final move
	edge     *Edge     // direct edge from the source, if depth 1
}

// Final returns the final predicted excess move.
func (c *ShockChild) Final() float64 { return c.Pred[len(c.Pred)-1] }

// OnShockDone, if set, is called once a shock is H bars old with every child's
// realised move filled in. Replay uses it for scoring.
type ShockHook func(*Shock)

// SetShockHook registers fn.
func (e *Engine) SetShockHook(fn ShockHook) { e.onShock = fn }

func (e *Engine) detectShocks(t int) {
	rt := e.at(e.r, t)
	H := e.cfg.Horizon
	for j := 0; j < e.n; j++ {
		if e.missing[j] {
			continue
		}
		z := e.robust[j].Z(rt[j])
		e.robust[j].Add(rt[j])
		if math.Abs(z) < e.cfg.ShockZ || t < e.cfg.Warmup || t-e.lastShock[j] < H {
			continue
		}
		e.lastShock[j] = t
		e.stats.Shocks++
		s := &Shock{Source: j, Bar: t, Time: e.BarTime(), Z: z, Size: rt[j] - e.robust[j].Med}
		s.Children = e.impulse(j, s.Size)
		e.Shocks = append(e.Shocks, s)
		if len(e.Shocks) > 400 {
			e.Shocks = e.Shocks[len(e.Shocks)-400:]
		}
	}
}

// impulse propagates a unit-time move of size delta at src through the fitted
// graph (parent weights and own decay only) and returns who it reaches.
func (e *Engine) impulse(src int, delta float64) []*ShockChild {
	H := e.cfg.Horizon
	irR, irX := e.irR, e.irX
	for h := 0; h <= H; h++ {
		clear(irR[h])
		clear(irX[h])
	}
	irR[0][src], irX[0][src] = delta, delta
	for h := 1; h <= H; h++ {
		for j := 0; j < e.n; j++ {
			m := e.models[j]
			s := 0.0
			for i, f := range m.feats {
				switch f.kind {
				case featParent:
					if h-f.lag >= 0 {
						s += m.rls.W[i] * irR[h-f.lag][f.src]
					}
				case featDecay:
					s += m.rls.W[i] * irX[h-1][j]
				}
			}
			irR[h][j] = s
			irX[h][j] = irX[h-1][j] + s
		}
	}
	depth, prob := e.bfs(src)
	var out []*ShockChild
	for j := 0; j < e.n; j++ {
		if j == src || depth[j] == 0 || math.Abs(irX[H][j]) < 0.1*math.Abs(delta) {
			continue
		}
		c := &ShockChild{Entity: j, Depth: depth[j], Prob: prob[j], Pred: make([]float64, H+1), base: make([]float64, H+1)}
		for h := 0; h <= H; h++ {
			c.Pred[h] = irX[h][j]
			c.base[h] = e.fd[h][j] // decay-only path from this bar's forecast
		}
		c.PeakBar = H
		for h := 1; h <= H; h++ {
			if c.Pred[h]/c.Pred[H] >= 0.9 {
				c.PeakBar = h
				break
			}
		}
		if depth[j] == 1 {
			for _, p := range e.parents[j] {
				if p.From == src {
					c.edge = p
				}
			}
		}
		out = append(out, c)
	}
	sort.Slice(out, func(a, b int) bool { return math.Abs(out[a].Final()) > math.Abs(out[b].Final()) })
	return out
}

// bfs returns hop depth and path probability from src along live edges, using
// Laplace-smoothed shock hit rates as edge probabilities.
func (e *Engine) bfs(src int) (depth []int, prob []float64) {
	depth, prob = e.bfsDepth, e.bfsProb
	clear(depth)
	clear(prob)
	prob[src] = 1
	frontier := []int{src}
	for d := 1; len(frontier) > 0 && d <= 4; d++ {
		var next []int
		for _, a := range frontier {
			for _, c := range e.children[a] {
				p := prob[a] * (float64(c.Hits) + 1) / (float64(c.Trials) + 2)
				if depth[c.To] == 0 && c.To != src {
					depth[c.To] = d
					next = append(next, c.To)
				}
				if depth[c.To] == d && p > prob[c.To] {
					prob[c.To] = p
				}
			}
		}
		frontier = next
	}
	return depth, prob
}

// trackShocks fills in realised moves for live shocks and closes them after H bars.
func (e *Engine) trackShocks(t int) {
	H := e.cfg.Horizon
	for _, s := range e.Shocks {
		if s.Done {
			continue
		}
		age := t - s.Bar
		if age > H {
			age = H
		}
		for _, c := range s.Children {
			c.Realized = e.x[c.Entity] - c.base[age]
		}
		if t-s.Bar < H {
			continue
		}
		s.Done = true
		for _, c := range s.Children {
			if c.edge == nil {
				continue
			}
			c.edge.Trials++
			if c.Realized*c.Final() > 0 && math.Abs(c.Realized) >= 0.5*math.Abs(c.Final()) {
				c.edge.Hits++
			}
		}
		if e.onShock != nil {
			e.onShock(s)
		}
	}
}
