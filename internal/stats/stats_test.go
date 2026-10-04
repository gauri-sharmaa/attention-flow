package stats

import (
	"math"
	"math/rand/v2"
	"testing"
)

func TestRLSRecoversWeights(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	r := NewRLS(3, 0.999, 100)
	want := []float64{0.5, -1.2, 2}
	x := make([]float64, 3)
	for i := 0; i < 5000; i++ {
		y := 0.0
		for j := range x {
			x[j] = rng.NormFloat64()
			y += want[j] * x[j]
		}
		r.Update(x, y+0.1*rng.NormFloat64())
	}
	for j, w := range want {
		if math.Abs(r.W[j]-w) > 0.02 {
			t.Errorf("w[%d] = %.3f, want %.3f", j, r.W[j], w)
		}
	}
	if s := r.Resid.Std(); s < 0.08 || s > 0.12 {
		t.Errorf("residual std = %.3f, want ~0.1", s)
	}
}

func TestRobustIgnoresSpikes(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	r := NewRobust(0.01)
	for i := 0; i < 20000; i++ {
		x := rng.NormFloat64()
		if i%50 == 0 {
			x = 40 // 2% gross outliers
		}
		r.Add(x)
	}
	if math.Abs(r.Med) > 0.15 {
		t.Errorf("median = %.3f, want ~0", r.Med)
	}
	// MAD of N(0,1) is 0.674.
	if r.MAD < 0.55 || r.MAD > 0.8 {
		t.Errorf("MAD = %.3f, want ~0.67", r.MAD)
	}
	if z := r.Z(6); z < 4.5 || z > 7.5 {
		t.Errorf("z(6) = %.2f, want ~6", z)
	}
}

func TestEW(t *testing.T) {
	e := EW{Alpha: Alpha(500)}
	rng := rand.New(rand.NewPCG(5, 6))
	for i := 0; i < 20000; i++ {
		e.Add(3 + 2*rng.NormFloat64())
	}
	if math.Abs(e.Mean-3) > 0.3 || math.Abs(e.Std()-2) > 0.2 {
		t.Errorf("mean %.2f std %.2f, want 3, 2", e.Mean, e.Std())
	}
}
