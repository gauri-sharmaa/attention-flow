// Package rewards estimates what a maker earns from Polymarket's liquidity
// rewards: a daily pool per market, shared every minute among resting orders
// near the midpoint in proportion to a score.
//
// The score (from Polymarket's docs) of an order s cents from the midpoint,
// in a market whose qualifying band is v cents, is ((v−s)/v)² × size. Bids
// add up to one side's score, asks to the other's. A maker quoting both
// sides is credited with the smaller; with the midpoint between 10¢ and 90¢
// a one-sided maker still gets a third of its side's score.
package rewards

import (
	"math"

	"github.com/gauri-sharmaa/attention-flow/internal/source"
)

// Score is one order's score: v and s in cents, zero outside the band.
func Score(v, s, size float64) float64 {
	if s >= v || s < 0 {
		return 0
	}
	return (v - s) / v * (v - s) / v * size
}

// Credit combines a maker's two side scores.
func Credit(bidQ, askQ, mid float64) float64 {
	lo := math.Min(bidQ, askQ)
	if mid >= 0.10 && mid <= 0.90 {
		return math.Max(lo, math.Max(bidQ, askQ)/3)
	}
	return lo
}

// Side scores of a whole book, counting only orders of at least minSize.
// Treating everyone else as one maker overstates the competition (the
// smaller side of a sum is at least the sum of the smaller sides), so the
// share computed against it is conservative.
func BookScores(bids, asks []source.Level, mid, v, minSize float64) (bidQ, askQ float64) {
	for _, l := range bids {
		if l.Size >= minSize {
			bidQ += Score(v, 100*(mid-l.Price), l.Size)
		}
	}
	for _, l := range asks {
		if l.Size >= minSize {
			askQ += Score(v, 100*(l.Price-mid), l.Size)
		}
	}
	return
}
