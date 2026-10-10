// Package leadlag estimates lead-lag between asynchronously traded prices
// with the Hoffmann–Rosenbaum–Yoshida (2013) estimator.
//
// Bars force both series onto a common clock and blur any lead shorter than
// the bar (the Epps effect). HRY works on each series' own observation times:
// the Hayashi–Yoshida covariance sums ΔX_i·ΔY_j over every pair of
// observation intervals that overlap, and shifting Y's clock by θ before
// matching gives a covariance curve U(θ). If X leads Y by θ*, |U| peaks at θ*.
//
// Significance comes from a null distribution: the same statistic after
// circularly shifting Y by random whole days (±2 hours), which keeps each
// series' own structure and roughly its time-of-day pattern but destroys real
// timing. P is (hits+1)/(nNull+1), so with 99 shuffles the floor is 0.01.
package leadlag

import (
	"math"
	"math/rand/v2"
	"sort"
)

// Series is one asset's observations: times (seconds, increasing) and the
// increment of the observed value over the interval ending at each time.
type Series struct {
	T  []float64 // observation times
	DX []float64 // increment over (T[i-1], T[i]]; DX[0] is ignored
}

// FromLevels builds a Series from observed levels at increasing times.
func FromLevels(t, x []float64) Series {
	s := Series{T: append([]float64(nil), t...), DX: make([]float64, len(x))}
	for i := 1; i < len(x); i++ {
		s.DX[i] = x[i] - x[i-1]
	}
	return s
}

// hy returns the Hayashi–Yoshida covariance of X with Y shifted by theta
// (Y's interval (b_{j-1}, b_j] is matched as (b_{j-1}-θ, b_j-θ]), restricted
// to intervals inside [from, to). Positive theta tests "X leads Y by theta".
func hy(x, y Series, theta, from, to float64) float64 {
	s := 0.0
	j := 1
	for i := 1; i < len(x.T); i++ {
		a0, a1 := x.T[i-1], x.T[i]
		if a1 <= from || a0 >= to || x.DX[i] == 0 {
			continue
		}
		// Advance j past Y intervals that end before this X interval starts.
		for j < len(y.T) && y.T[j]-theta <= a0 {
			j++
		}
		for k := j; k < len(y.T); k++ {
			b0, b1 := y.T[k-1]-theta, y.T[k]-theta
			if b0 >= a1 {
				break
			}
			if b1 > a0 && y.DX[k] != 0 {
				s += x.DX[i] * y.DX[k]
			}
		}
	}
	return s
}

func sumSq(x Series, from, to float64) float64 {
	s := 0.0
	for i := 1; i < len(x.T); i++ {
		if x.T[i] > from && x.T[i-1] < to {
			s += x.DX[i] * x.DX[i]
		}
	}
	return s
}

// Grid is the set of lags tested, in seconds (positive: X leads).
var Grid = func() []float64 {
	base := []float64{0, 10, 30, 60, 120, 300, 600, 1200, 1800, 3600}
	var g []float64
	for _, v := range base {
		g = append(g, v)
		if v != 0 {
			g = append(g, -v)
		}
	}
	sort.Float64s(g)
	return g
}()

// Result is the estimated lead-lag between two series over a window.
type Result struct {
	Lag   float64 // seconds; positive means X leads Y
	Rho   float64 // normalised covariance at that lag
	Rho0  float64 // ... at lag 0
	P     float64 // share of null shuffles with max |rho| at least as large
	Peaks []float64
}

// Estimate computes the HRY curve on [from, to), its argmax and a p-value
// from nNull whole-day circular shifts of Y within [span0, span1).
func Estimate(x, y Series, from, to, span0, span1 float64, nNull int, rng *rand.Rand) Result {
	// Only observations inside the shuffle span may take part; otherwise a
	// circular shift would wrap data from outside the window into it.
	x, y = window(x, span0, span1), window(y, span0, span1)
	nx, ny := sumSq(x, from, to), sumSq(y, from, to)
	if nx == 0 || ny == 0 {
		return Result{P: 1}
	}
	norm := math.Sqrt(nx * ny)
	curve := func(y Series) (float64, float64, float64) {
		best, lag, r0 := 0.0, 0.0, 0.0
		for _, th := range Grid {
			r := hy(x, y, th, from, to) / norm
			if th == 0 {
				r0 = r
			}
			if math.Abs(r) > math.Abs(best) {
				best, lag = r, th
			}
		}
		return best, lag, r0
	}
	best, lag, r0 := curve(y)
	res := Result{Lag: lag, Rho: best, Rho0: r0}
	if nNull > 0 {
		hits := 0
		span := span1 - span0
		days := int(span / 86400)
		for n := 0; n < nNull; n++ {
			// Whole days plus up to ±2 hours: time of day stays roughly
			// aligned, and a 30-day window offers ~145 distinct shifts, enough
			// to resolve p-values at the 1% level.
			off := 86400*float64(1+rng.IntN(max(1, days-1))) + 3600*float64(rng.IntN(5)-2)
			b, _, _ := curve(shift(y, off, span0, span))
			if math.Abs(b) >= math.Abs(best) {
				hits++
			}
		}
		res.P = (float64(hits) + 1) / float64(nNull+1)
	}
	return res
}

// window keeps the observations with times in [a, b).
func window(s Series, a, b float64) Series {
	lo := sort.SearchFloat64s(s.T, a)
	hi := sort.SearchFloat64s(s.T, b)
	if lo >= hi {
		return Series{}
	}
	w := Series{T: s.T[lo:hi], DX: append([]float64(nil), s.DX[lo:hi]...)}
	w.DX[0] = 0 // its interval starts before the window
	return w
}

// shift circularly moves Y's observation times by off within [span0, span0+span).
func shift(y Series, off, span0, span float64) Series {
	type obs struct{ t, dx float64 }
	o := make([]obs, len(y.T))
	for i := range y.T {
		t := span0 + math.Mod(y.T[i]-span0+off, span)
		o[i] = obs{t, y.DX[i]}
	}
	sort.Slice(o, func(a, b int) bool { return o[a].t < o[b].t })
	s := Series{T: make([]float64, len(o)), DX: make([]float64, len(o))}
	for i, v := range o {
		s.T[i], s.DX[i] = v.t, v.dx
	}
	if len(s.DX) > 0 {
		s.DX[0] = 0 // the wrapped first interval has no defined increment
	}
	return s
}
