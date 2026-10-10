package leadlag

import (
	"math"
	"math/rand/v2"
	"testing"
)

// X is a random walk observed at random times; Y follows X 120 s later plus
// noise, observed at its own random times. HRY must find +120 s.
func TestFindsPlantedLead(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	T := 5 * 86400.0
	// Latent X on a 1-second grid.
	n := int(T)
	lx := make([]float64, n)
	for i := 1; i < n; i++ {
		lx[i] = lx[i-1] + rng.NormFloat64()*0.01
	}
	obs := func(rate float64, f func(int) float64) Series {
		var ts, xs []float64
		for tt := 0.0; tt < T-1; tt += rng.ExpFloat64() / rate {
			ts = append(ts, tt)
			xs = append(xs, f(int(tt)))
		}
		return FromLevels(ts, xs)
	}
	x := obs(1.0/30, func(i int) float64 { return lx[i] })
	y := obs(1.0/45, func(i int) float64 {
		if i < 120 {
			return 0
		}
		return lx[i-120] + rng.NormFloat64()*0.005
	})
	r := Estimate(x, y, 0, T, 0, T, 20, rng)
	if r.Lag != 120 {
		t.Errorf("lag = %v, want 120 (rho %.3f, rho0 %.3f)", r.Lag, r.Rho, r.Rho0)
	}
	if r.P > 0.05 {
		t.Errorf("p = %.3f, want significant", r.P)
	}
	if math.Abs(r.Rho) < 3*math.Abs(r.Rho0) {
		t.Errorf("peak %.3f not clearly above lag-0 %.3f", r.Rho, r.Rho0)
	}
}

// Under independence, p-values must be roughly uniform: about 5% of pairs
// below 0.05, not many more.
func TestNullFalseAlarmRate(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 8))
	// Whole-day shifts need many distinct days for a usable null.
	T := 20 * 86400.0
	walk := func(rate float64) Series {
		var ts, xs []float64
		x := 0.0
		for tt := 0.0; tt < T; tt += rng.ExpFloat64() / rate {
			x += rng.NormFloat64() * 0.01
			ts = append(ts, tt)
			xs = append(xs, x)
		}
		return FromLevels(ts, xs)
	}
	low := 0
	const pairs = 40
	for k := 0; k < pairs; k++ {
		if r := Estimate(walk(1.0/300), walk(1.0/300), 0, T, 0, T, 39, rng); r.P < 0.05 {
			low++
		}
	}
	if low > 6 { // 5% of 40 is 2; 6+ would be p < 0.02 under a fair null
		t.Errorf("%d of %d independent pairs significant at 5%%", low, pairs)
	}
}
