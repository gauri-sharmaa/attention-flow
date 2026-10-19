package rewards

import (
	"math"
	"testing"
	"time"

	"github.com/gauri-sharmaa/attention-flow/internal/source"
)

func testSim(t *testing.T) *Sim {
	m := SimMarket{LiveMarket: source.LiveMarket{ConditionID: "c", Question: "q"}, Daily: 1440, MaxSpread: 3, MinSize: 10}
	s := NewSim(t.TempDir(), []SimMarket{m}, 5)
	s.onBook(source.BookUpdate{ConditionID: "c", Snapshot: true,
		Bids: []source.Level{{Price: 0.49, Size: 100}}, Asks: []source.Level{{Price: 0.51, Size: 100}}})
	return s
}

func TestSimRewardShare(t *testing.T) {
	s := testSim(t)
	s.sample(time.Unix(1000, 0))
	st := s.state["c"]
	if !st.Bid.Live || st.Bid.Price != 0.49 || !st.Ask.Live || st.Ask.Price != 0.51 {
		t.Fatalf("orders: %+v %+v", st.Bid, st.Ask)
	}
	// We and the book sit at the same prices: 10 shares against 100 on each side.
	if want := 10.0 / 110; math.Abs(st.Rewards-want) > 1e-9 { // $1 a minute pool
		t.Errorf("rewards %v, want %v", st.Rewards, want)
	}
}

func TestSimQueueFill(t *testing.T) {
	s := testSim(t)
	s.sample(time.Unix(1000, 0))
	sell := func(shares float64) {
		s.onTrade(source.Trade{T: 1001, ConditionID: "c", YesPrice: 0.49, Dir: -1, Shares: shares})
	}
	sell(60)
	s.onBook(source.BookUpdate{ConditionID: "c", Bids: []source.Level{{Price: 0.49, Size: 30}}}) // 60 traded, 10 more cancelled
	if st := s.state["c"]; !st.Bid.Live || st.Bid.Ahead != 30 {
		t.Fatalf("after 60 traded and 10 cancelled, 30 should be ahead: %+v", st.Bid)
	}
	sell(35)
	if s.state["c"].Pos != 0 {
		t.Fatal("filled before the queue and our order traded through")
	}
	sell(5)
	st := s.state["c"]
	if st.Bid.Live || st.Pos != 10 || math.Abs(st.Cash+4.9) > 1e-9 {
		t.Fatalf("should have bought 10 at 0.49: %+v", st)
	}
	// A buy at the ask doesn't touch the bid; a trade through the ask fills it at once.
	s.onTrade(source.Trade{T: 1002, ConditionID: "c", YesPrice: 0.52, Dir: 1, Shares: 1})
	if st.Ask.Live || st.Pos != 0 {
		t.Fatalf("trade through the ask should fill it: %+v", st)
	}
}

func TestSimInventoryLimit(t *testing.T) {
	s := testSim(t)
	s.state["c"].Pos = 50 // at the limit (5 × 10 shares)
	s.sample(time.Unix(1000, 0))
	if st := s.state["c"]; st.Bid.Live || !st.Ask.Live {
		t.Fatalf("at the long limit only the ask should be posted: %+v %+v", st.Bid, st.Ask)
	}
}
