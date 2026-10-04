// Package stats holds the small online estimators the engine updates once per bar.
// Everything here is O(1) or O(d^2) per update and allocation-free after construction.
package stats

import "math"

// Alpha converts a half-life in bars to an exponential-weighting factor.
func Alpha(halfLife float64) float64 { return 1 - math.Exp(-math.Ln2/halfLife) }

// EffN is the effective sample size of an exponential window with factor alpha.
func EffN(alpha float64) float64 { return (2 - alpha) / alpha }

// EW tracks an exponentially weighted mean and variance.
type EW struct {
	Alpha float64
	Mean  float64
	Var   float64
	N     int
}

// Add folds x into the estimate.
func (e *EW) Add(x float64) {
	e.N++
	if e.N == 1 {
		e.Mean = x
		return
	}
	d := x - e.Mean
	e.Mean += e.Alpha * d
	e.Var = (1 - e.Alpha) * (e.Var + e.Alpha*d*d)
}

// Std is the current standard deviation.
func (e *EW) Std() float64 { return math.Sqrt(e.Var) }

// Robust tracks a running median and median absolute deviation with
// stochastic-approximation updates, so a burst of spikes cannot drag the
// baseline the way it drags a mean/variance. Used for shock detection.
type Robust struct {
	Med, MAD float64
	step     float64
	n        int
}

// NewRobust returns a tracker that adapts at roughly rate alpha.
func NewRobust(alpha float64) *Robust { return &Robust{step: alpha} }

// Z returns the robust z-score of x against the current baseline.
func (r *Robust) Z(x float64) float64 {
	if r.n < 30 || r.MAD <= 0 {
		return 0
	}
	return (x - r.Med) / (1.4826 * r.MAD)
}

// Add folds x into the baseline.
func (r *Robust) Add(x float64) {
	r.n++
	if r.n == 1 {
		r.Med, r.MAD = x, 0
		return
	}
	scale := r.MAD
	if scale <= 0 {
		scale = math.Abs(x-r.Med) + 1e-9
	}
	// Median: move toward x by a fixed fraction of the scale (sign-based update).
	r.Med += r.step * scale * sign(x-r.Med)
	// MAD: the median of |x - med| obeys the same update.
	dev := math.Abs(x - r.Med)
	if r.MAD <= 0 {
		r.MAD = dev
		return
	}
	r.MAD += r.step * r.MAD * sign(dev-r.MAD)
}

func sign(x float64) float64 {
	switch {
	case x > 0:
		return 1
	case x < 0:
		return -1
	}
	return 0
}

// RLS is recursive least squares with exponential forgetting.
// It fits y ≈ w·x online and tracks residual variance for prediction intervals.
type RLS struct {
	D      int
	W      []float64
	P      []float64 // D×D inverse-covariance, row-major
	Lambda float64   // forgetting factor (1 = no forgetting)
	Resid  EW        // one-step residuals
	N      int
	px     []float64 // scratch: P·x
}

// NewRLS returns an RLS model with d features, forgetting factor lambda and
// initial P = delta·I (larger delta = weaker prior toward zero weights).
func NewRLS(d int, lambda, delta float64) *RLS {
	r := &RLS{D: d, W: make([]float64, d), P: make([]float64, d*d), Lambda: lambda, px: make([]float64, d)}
	r.Resid.Alpha = 1 - lambda
	for i := 0; i < d; i++ {
		r.P[i*d+i] = delta
	}
	return r
}

// Predict returns w·x.
func (r *RLS) Predict(x []float64) float64 {
	s := 0.0
	for i, v := range x {
		s += r.W[i] * v
	}
	return s
}

// Leverage returns x'Px, the parameter-uncertainty part of the prediction variance.
func (r *RLS) Leverage(x []float64) float64 {
	d := r.D
	s := 0.0
	for i := 0; i < d; i++ {
		row := r.P[i*d : i*d+d]
		t := 0.0
		for j, v := range x {
			t += row[j] * v
		}
		s += x[i] * t
	}
	return s
}

// Update folds in one observation (x, y) and returns the a-priori residual.
func (r *RLS) Update(x []float64, y float64) float64 {
	d := r.D
	px := r.px
	for i := 0; i < d; i++ {
		row := r.P[i*d : i*d+d]
		t := 0.0
		for j, v := range x {
			t += row[j] * v
		}
		px[i] = t
	}
	den := r.Lambda
	for i, v := range x {
		den += v * px[i]
	}
	err := y - r.Predict(x)
	for i := 0; i < d; i++ {
		r.W[i] += px[i] / den * err
	}
	inv := 1 / r.Lambda
	for i := 0; i < d; i++ {
		ki := px[i] / den
		row := r.P[i*d : i*d+d]
		for j := 0; j < d; j++ {
			row[j] = (row[j] - ki*px[j]) * inv
		}
	}
	// Keep P symmetric and bounded so long runs with idle features cannot blow
	// up. Rounding drift is slow, so every 32 updates is plenty.
	r.N++
	r.Resid.Add(err)
	if r.N%32 != 0 {
		return err
	}
	for i := 0; i < d; i++ {
		for j := i + 1; j < d; j++ {
			m := 0.5 * (r.P[i*d+j] + r.P[j*d+i])
			r.P[i*d+j], r.P[j*d+i] = m, m
		}
		if r.P[i*d+i] > 1e6 {
			r.P[i*d+i] = 1e6
		}
	}
	return err
}

// NormCDF is the standard normal CDF.
func NormCDF(z float64) float64 { return 0.5 * math.Erfc(-z/math.Sqrt2) }
