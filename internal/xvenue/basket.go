package xvenue

import (
	"math"

	"github.com/gauri-sharmaa/attention-flow/internal/source"
)

// A basket is an event whose outcomes can't both happen ("who wins the
// election", "Fed decision"): at most one market in it resolves YES. That
// gives two trades whose payout is known in advance:
//
//	all NO   one NO in every market pays n-1 whichever single outcome
//	         happens (n if none does), so it's free money if it costs less
//	all YES  one YES in every market pays exactly 1 if one outcome must
//	         happen, so it's free money below 1. Only safe when the listed
//	         outcomes cover every possibility.

// Leg is one market in an event: the best prices to buy each side and the
// taker fee rate (fee per contract = rate·p·(1-p) on both venues).
type Leg struct {
	Name    string
	Yes, No source.Quote
	FeeRate float64
}

// BasketQuote is the cost of one full set (one contract in every leg).
type BasketQuote struct {
	Side   string  // "no" or "yes"
	Cost   float64 // per set, fees included
	Payout float64 // guaranteed per set
	Edge   float64 // payout − cost
	Sets   float64 // sets available at these prices (the thinnest leg)
}

// PriceBasket prices both trades. A side is skipped (ok false) when any leg
// has no seller on it.
func PriceBasket(legs []Leg) (no, yes BasketQuote, noOK, yesOK bool) {
	price := func(side string, pick func(Leg) source.Quote, payout float64) (BasketQuote, bool) {
		b := BasketQuote{Side: side, Payout: payout, Sets: math.Inf(1)}
		for _, l := range legs {
			q := pick(l)
			if q.Price <= 0 || q.Price >= 1 || q.Size <= 0 {
				return b, false
			}
			b.Cost += q.Price + l.FeeRate*q.Price*(1-q.Price)
			b.Sets = math.Min(b.Sets, q.Size)
		}
		b.Edge = b.Payout - b.Cost
		return b, len(legs) >= 2
	}
	no, noOK = price("no", func(l Leg) source.Quote { return l.No }, float64(len(legs)-1))
	yes, yesOK = price("yes", func(l Leg) source.Quote { return l.Yes }, 1)
	return
}
