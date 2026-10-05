package hawkes

import (
	"math"
	"math/rand/v2"
	"sort"
)

// Simulate draws events from the model on [0, T) by Ogata thinning. Used to
// check that Fit recovers known parameters.
func (m *Model) Simulate(T float64, rng *rand.Rand) []Event {
	K := len(m.Betas)
	// children[j] lists (i, p) pairs where j is a parent of i.
	type link struct{ i, p int }
	children := make([][]link, m.D)
	for i := 0; i < m.D; i++ {
		for p, j := range m.Parents[i] {
			children[j] = append(children[j], link{i, p})
		}
	}
	// exc[i][k]: current excitation of stream i at scale k (already multiplied by α and β).
	exc := make([][]float64, m.D)
	for i := range exc {
		exc[i] = make([]float64, K)
	}
	total := func() float64 {
		s := 0.0
		for i := 0; i < m.D; i++ {
			s += m.Mu[i]
			for _, v := range exc[i] {
				s += v
			}
		}
		return s
	}
	decay := func(dt float64) {
		for i := range exc {
			for k, b := range m.Betas {
				exc[i][k] *= math.Exp(-b * dt)
			}
		}
	}
	var out []Event
	t := 0.0
	for {
		bound := total() // intensity only decays between events, so this bounds it
		dt := rng.ExpFloat64() / bound
		t += dt
		if t >= T {
			break
		}
		decay(dt)
		lam := total()
		if rng.Float64()*bound > lam {
			continue
		}
		u := rng.Float64() * lam
		dim := 0
		for i := 0; i < m.D; i++ {
			v := m.Mu[i]
			for _, x := range exc[i] {
				v += x
			}
			if u < v {
				dim = i
				break
			}
			u -= v
		}
		out = append(out, Event{T: t, Dim: dim})
		for _, l := range children[dim] {
			for k, b := range m.Betas {
				exc[l.i][k] += m.Alpha[l.i][l.p][k] * b
			}
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].T < out[b].T })
	return out
}
