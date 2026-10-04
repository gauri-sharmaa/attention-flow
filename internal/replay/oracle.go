package replay

import (
	"github.com/gauri-sharmaa/attention-flow/internal/core"
	"github.com/gauri-sharmaa/attention-flow/internal/engine"
	"github.com/gauri-sharmaa/attention-flow/internal/sim"
)

// oracle forecasts with the simulator's true graph, lags, loadings and decay.
// It sees the same observations as the engine, so its score is the ceiling any
// model could reach on this data: the rest is genuinely unpredictable noise.
type oracle struct {
	tr        *sim.Truth
	n, hist   int
	clusterOf []int
	in        [][]sim.TrueEdge
	news      [][]float64 // news[t%hist][j]: innovation excluding decay
	prevX     []float64
	fut       [][]float64 // forecast news, h = 1..H
}

func newOracle(u *core.Universe, tr *sim.Truth, H int) *oracle {
	n := len(u.Entities)
	o := &oracle{tr: tr, n: n, hist: tr.MaxLag + H + 8, clusterOf: u.ClusterIndex(), in: make([][]sim.TrueEdge, n)}
	for _, e := range tr.Edges {
		o.in[e.To] = append(o.in[e.To], e)
	}
	o.news = make([][]float64, o.hist)
	for i := range o.news {
		o.news[i] = make([]float64, n)
	}
	o.fut = make([][]float64, H+1)
	for i := range o.fut {
		o.fut[i] = make([]float64, n)
	}
	return o
}

func (o *oracle) get(j, tau, now int) float64 {
	if tau > now {
		return o.fut[tau-now][j]
	}
	if tau < 0 || now-tau >= o.hist {
		return 0
	}
	return o.news[tau%o.hist][j]
}

// observe records bar t and writes the oracle's H-bar level forecast for every
// entity (true graph, true factor delays, true decay toward the true baseline).
func (o *oracle) observe(e *engine.Engine, t, H int, out []float64) {
	k := o.tr.Decay
	cur := o.news[t%o.hist]
	for j := 0; j < o.n; j++ {
		x := e.Level(j)
		if o.prevX == nil {
			cur[j] = 0
			continue
		}
		cur[j] = (x - o.prevX[j]) + k*(o.prevX[j]-o.tr.BaseLevel[j])
	}
	if o.prevX == nil {
		o.prevX = make([]float64, o.n)
	}
	for j := 0; j < o.n; j++ {
		o.prevX[j] = e.Level(j)
	}
	for h := 1; h <= H; h++ {
		tau := t + h
		for j := 0; j < o.n; j++ {
			v := 0.0
			for _, ed := range o.in[j] {
				if tau < ed.Lag+1 || !ed.ActiveAt(tau, o.tr.MidBar) {
					continue
				}
				v += ed.Beta * (0.8*o.get(ed.From, tau-ed.Lag, t) + 0.2*o.get(ed.From, tau-ed.Lag-1, t))
			}
			if L := o.tr.SlowLag[j]; L > 0 && tau-L <= t && tau-L >= 0 && tau-L < len(o.tr.Factor) {
				v += o.tr.SlowLoad[j] * o.tr.Factor[tau-L][o.clusterOf[j]]
			}
			o.fut[h][j] = v
		}
	}
	for j := 0; j < o.n; j++ {
		x := e.Level(j)
		for h := 1; h <= H; h++ {
			x += o.fut[h][j] - k*(x-o.tr.BaseLevel[j])
		}
		out[j] = x
	}
}
