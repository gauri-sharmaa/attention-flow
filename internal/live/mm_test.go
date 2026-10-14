package live

import (
	"math"
	"testing"

	"github.com/gauri-sharmaa/attention-flow/internal/source"
)

func mmEngine() (*Engine, *market) {
	e := New(Config{MoveSize: 0.02, Horizon: 600})
	m := &market{LiveMarket: source.LiveMarket{ConditionID: "c", FeeRate: 0.04, RebateRate: 0.25}, ref: -1, bk: newBook(), midRef: -1}
	e.markets, e.byID = []*market{m}, map[string]*market{"c": m}
	m.bk.apply(source.BookUpdate{Snapshot: true,
		Bids: []source.Level{{Price: 0.40, Size: 100}}, Asks: []source.Level{{Price: 0.42, Size: 50}}})
	return e, m
}

func TestPaperFills(t *testing.T) {
	e, m := mmEngine()
	e.requote(m, 0) // untrained model: only the baseline quotes both sides
	if n := len(e.mm.orders["c"]); n != 2 {
		t.Fatalf("%d orders, want 2", n)
	}
	e.mmTrade(m, 1, 0.40, -1, 60) // 60 of the 100 ahead of our bid
	if len(e.mm.pending) != 0 {
		t.Fatal("filled before the queue ahead was cleared")
	}
	e.mmTrade(m, 2, 0.40, -1, 60) // 120 ≥ 100 ahead + our 10: queue fill only
	if len(e.mm.pending) != 1 || e.mm.pending[0].rule != "queue" || !e.mm.pending[0].buy {
		t.Fatalf("pending %+v, want one queue buy", e.mm.pending)
	}
	e.mmTrade(m, 3, 0.43, +1, 5) // a buy through our 0.42 ask: strict and queue
	if len(e.mm.pending) != 3 {
		t.Fatalf("%d pending, want 3", len(e.mm.pending))
	}
	if r := e.mm.pending[1]; r.buy || r.rule != "strict" || math.Abs(r.rebate-0.25*0.04*0.42*0.58) > 1e-12 {
		t.Fatalf("sell fill %+v", r)
	}
}

// A buy at 0.40 marked against a later midpoint of 0.45 earns 5¢ (plus
// rebate); a sell at 0.42 against the same midpoint loses 3¢.
func TestMarkoutSign(t *testing.T) {
	e, m := mmEngine()
	e.requote(m, 0)
	e.mmTrade(m, 1, 0.39, -1, 1)
	e.mmTrade(m, 1, 0.43, +1, 1)
	m.bk.apply(source.BookUpdate{Snapshot: true,
		Bids: []source.Level{{Price: 0.44, Size: 10}}, Asks: []source.Level{{Price: 0.46, Size: 10}}})
	e.mmMark(1 + 3600)
	if len(e.mm.done) != 4 {
		t.Fatalf("%d scored fills, want 4", len(e.mm.done))
	}
	for _, f := range e.mm.done {
		want := 0.05
		if !f.buy {
			want = -0.03
		}
		if math.Abs(f.marks[2]-want) > 1e-9 {
			t.Fatalf("%v fill marked %v, want %v", f.buy, f.marks[2], want)
		}
	}
}
