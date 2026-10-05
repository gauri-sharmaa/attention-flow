package hawkes

import (
	"math"
	"math/rand/v2"
	"testing"
)

// Three streams: 0 excites 1 at the 2-minute scale, 1 excites 2 at the
// 10-second scale, every stream self-excites; 2 does not excite 0.
func truth() *Model {
	betas := []float64{1.0 / 10, 1.0 / 120, 1.0 / 1200}
	cand := [][]int{{1, 2}, {0, 2}, {0, 1}}
	m := New(3, betas, cand)
	m.Mu = []float64{0.01, 0.005, 0.005}
	set := func(i, j, k int, v float64) {
		for p, q := range m.Parents[i] {
			if q == j {
				m.Alpha[i][p][k] = v
			}
		}
	}
	set(0, 0, 2, 0.3)
	set(1, 1, 1, 0.2)
	set(2, 2, 0, 0.2)
	set(1, 0, 1, 0.4)
	set(2, 1, 0, 0.5)
	return m
}

func branch(m *Model, i, j int) float64 {
	for p, q := range m.Parents[i] {
		if q == j {
			s := 0.0
			for _, a := range m.Alpha[i][p] {
				s += a
			}
			return s
		}
	}
	return 0
}

func TestFitRecoversLinks(t *testing.T) {
	if testing.Short() {
		t.Skip("slow: EM needs many iterations on the 20-minute scale")
	}
	tr := truth()
	T := 30 * 86400.0
	evs := tr.Simulate(T, rand.New(rand.NewPCG(1, 2)))
	if len(evs) < 20000 {
		t.Fatalf("only %d events", len(evs))
	}
	m := New(3, tr.Betas, [][]int{{1, 2}, {0, 2}, {0, 1}})
	m.Fit(evs, Options{Iters: 2000, Tol: 1e-9, Window: [2]float64{0, T}})
	for _, c := range []struct {
		i, j int
		want float64
	}{{1, 0, 0.4}, {2, 1, 0.5}, {0, 0, 0.3}, {0, 2, 0}, {2, 0, 0}} {
		if got := branch(m, c.i, c.j); math.Abs(got-c.want) > 0.07 {
			t.Errorf("branch %d→%d = %.3f, want %.2f", c.j, c.i, got, c.want)
		}
	}
	// The 1→2 link acts at 10 s, the 0→1 link at 2 min.
	for _, e := range m.Edges(0.1) {
		if e.From == 1 && e.To == 2 && (e.MeanLag < 5 || e.MeanLag > 40) {
			t.Errorf("1→2 mean lag %.0fs, want ~10s", e.MeanLag)
		}
		if e.From == 0 && e.To == 1 && (e.MeanLag < 60 || e.MeanLag > 300) {
			t.Errorf("0→1 mean lag %.0fs, want ~120s", e.MeanLag)
		}
	}
}

func TestCrossLinksWinOutOfSample(t *testing.T) {
	tr := truth()
	T := 30 * 86400.0
	evs := tr.Simulate(T, rand.New(rand.NewPCG(3, 4)))
	split := 0.7 * T
	self := New(3, tr.Betas, nil)
	self.Fit(evs, Options{Iters: 200, Tol: 1e-7, Window: [2]float64{0, split}})
	full := New(3, tr.Betas, [][]int{{1, 2}, {0, 2}, {0, 1}})
	full.Fit(evs, Options{Iters: 200, Tol: 1e-7, Window: [2]float64{0, split}})
	ls, n := self.LogLik(evs, split, T)
	lf, _ := full.LogLik(evs, split, T)
	if gain := (lf - ls) / float64(n); gain < 0.05 {
		t.Errorf("held-out gain from cross links = %.4f nats/event, want clearly positive", gain)
	}
}

// A weighted event must be exactly equivalent to that many copies.
func TestWeightsEqualCopies(t *testing.T) {
	tr := truth()
	evs := tr.Simulate(2*86400, rand.New(rand.NewPCG(5, 6)))
	var dup, wt []Event
	for i, e := range evs {
		if i%7 == 0 {
			dup = append(dup, e, e, e)
			wt = append(wt, Event{T: e.T, Dim: e.Dim, W: 3})
		} else {
			dup = append(dup, e)
			wt = append(wt, e)
		}
	}
	a, na := tr.LogLik(dup, 3600, 2*86400)
	b, nb := tr.LogLik(wt, 3600, 2*86400)
	if na != nb || math.Abs(a-b) > 1e-6*math.Abs(a) {
		t.Errorf("copies: %.6f over %d events, weights: %.6f over %d", a, na, b, nb)
	}
}
