package live

import (
	"math"

	"github.com/gauri-sharmaa/attention-flow/internal/source"
)

// book is a market's YES order book: price → resting size on each side.
type book struct {
	bids, asks map[float64]float64
	t          float64 // last update
}

func newBook() *book { return &book{bids: map[float64]float64{}, asks: map[float64]float64{}} }

func (b *book) apply(u source.BookUpdate) {
	if u.Snapshot {
		clear(b.bids)
		clear(b.asks)
	}
	set := func(side map[float64]float64, ls []source.Level) {
		for _, l := range ls {
			if l.Size <= 0 {
				delete(side, l.Price)
			} else {
				side[l.Price] = l.Size
			}
		}
	}
	set(b.bids, u.Bids)
	set(b.asks, u.Asks)
	b.t = u.T
}

// top returns the best bid and ask (ok false if either side is empty or the
// book is crossed, which happens briefly mid-update).
func (b *book) top() (bid, ask float64, ok bool) {
	bid, ask = math.Inf(-1), math.Inf(1)
	for p := range b.bids {
		bid = math.Max(bid, p)
	}
	for p := range b.asks {
		ask = math.Min(ask, p)
	}
	return bid, ask, !math.IsInf(bid, 0) && !math.IsInf(ask, 0) && bid < ask
}

// mid is the midpoint of the best bid and ask: a price without bid-ask
// bounce, unlike the last trade.
func (b *book) mid() (float64, bool) {
	bid, ask, ok := b.top()
	return (bid + ask) / 2, ok
}

// imbalance is (bid depth − ask depth) / total depth within `within` of the
// midpoint, in shares: +1 means only buyers are waiting near the price.
func (b *book) imbalance(within float64) float64 {
	m, ok := b.mid()
	if !ok {
		return 0
	}
	var bd, ad float64
	for p, s := range b.bids {
		if m-p <= within {
			bd += s
		}
	}
	for p, s := range b.asks {
		if p-m <= within {
			ad += s
		}
	}
	if bd+ad == 0 {
		return 0
	}
	return (bd - ad) / (bd + ad)
}
