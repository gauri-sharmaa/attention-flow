package live

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
)

// The paper market maker: does the order-book direction signal make money
// for someone who posts orders (and so pays no taker fee and earns Polymarket's
// maker rebate) rather than crossing the spread?
//
// Every few seconds each market's paper orders are replaced. Three
// strategies run side by side on the same markets and moments:
//
//	both    quote the best bid and the best ask (no signal: the baseline)
//	signal  quote only the side the direction model favours (P(up) ≥ 55%:
//	        bid; ≤ 45%: ask), nothing otherwise
//	pull    quote both, except the side the model expects to be run over
//
// Fills can't be observed for paper orders, so two rules bracket the truth:
//
//	strict  filled only if a trade goes through our price (the market blew
//	        past us: the worst fills there are)
//	queue   also filled at our price once the volume traded there exceeds
//	        what was already resting ahead of us when we posted
//
// Each fill is scored by its markout: how far the midpoint moved our way 1,
// 10 and 60 minutes later, per share, plus the maker rebate. A market
// maker's edge is the average markout; "exit as taker" also subtracts half
// the spread and the taker fee for getting out, the worst case.

const mmShares = 10 // paper order size

var mmHorizons = []float64{60, 600, 3600}

type paperOrder struct {
	strat  string
	t      float64
	buy    bool
	price  float64
	ahead  float64 // shares resting at our price when we posted
	traded float64 // shares traded at our price since
}

type paperFill struct {
	strat, rule, cid string
	t, price         float64
	buy              bool
	rebate, takerFee float64 // per share
	marks            []float64
	halfSpread       []float64
	sector           string
}

type mmBook struct {
	orders  map[string][]paperOrder // by market
	pending []paperFill
	done    []paperFill
	f       *os.File
}

func newMM(dir string) *mmBook {
	m := &mmBook{orders: map[string][]paperOrder{}}
	if dir != "" {
		m.f = openAppend(filepath.Join(dir, "paper_mm.csv"),
			"t,market,strategy,fill_rule,side,price,rebate,taker_fee,mark_1m,mark_10m,mark_60m,half_spread_60m,sector")
	}
	return m
}

// requote replaces a market's paper orders (caller holds the engine lock).
func (e *Engine) requote(m *market, now float64) {
	bid, ask, ok := m.bk.top()
	delete(e.mm.orders, m.ConditionID)
	if !ok || ask-bid < 0.0099 { // a crossed or locked book has no room to quote
		return
	}
	pUp := -1.0
	if e.rec.learner.n >= 100 {
		if x, ok := e.bookFeatures(m, now); ok {
			pUp = e.rec.learner.prob(x)
		}
	}
	post := func(strat string, buy bool) {
		price, ahead := bid, m.bk.bids[bid]
		if !buy {
			price, ahead = ask, m.bk.asks[ask]
		}
		e.mm.orders[m.ConditionID] = append(e.mm.orders[m.ConditionID], paperOrder{strat: strat, t: now, buy: buy, price: price, ahead: ahead})
	}
	post("both", true)
	post("both", false)
	if pUp < 0 {
		return // the signal strategies need a trained model
	}
	switch {
	case pUp >= 0.55:
		post("signal", true)
	case pUp <= 0.45:
		post("signal", false)
	}
	if pUp < 0.55 { // not expecting a rise: keep the ask; drop the bid only if a fall is expected
		post("pull", false)
	}
	if pUp > 0.45 {
		post("pull", true)
	}
}

// mmTrade checks a live trade against the market's paper orders (caller
// holds the lock). A YES-equivalent sell hits bids; a buy lifts asks.
func (e *Engine) mmTrade(m *market, t float64, yesPrice float64, dir int, shares float64) {
	orders := e.mm.orders[m.ConditionID]
	kept := orders[:0]
	for _, o := range orders {
		if t <= o.t {
			kept = append(kept, o)
			continue
		}
		hits := (o.buy && dir < 0) || (!o.buy && dir > 0)
		through := hits && ((o.buy && yesPrice < o.price-1e-9) || (!o.buy && yesPrice > o.price+1e-9))
		at := hits && math.Abs(yesPrice-o.price) < 1e-9
		if at {
			o.traded += shares
		}
		queue := at && o.traded >= o.ahead+mmShares
		if through || queue {
			if through {
				e.mm.fill(m, o, "strict", t)
			}
			e.mm.fill(m, o, "queue", t) // anything strict fills, queue fills too
			continue
		}
		kept = append(kept, o)
	}
	e.mm.orders[m.ConditionID] = kept
}

func (mm *mmBook) fill(m *market, o paperOrder, rule string, t float64) {
	p := o.price
	mm.pending = append(mm.pending, paperFill{strat: o.strat, rule: rule, cid: m.ConditionID, t: t, price: p, buy: o.buy,
		rebate: m.RebateRate * m.FeeRate * p * (1 - p), takerFee: m.FeeRate * p * (1 - p), sector: m.Sector})
}

// mmMark scores fills whose horizons have passed against the current
// midpoint (caller holds the lock). Called every few seconds, so a mark is
// at most that late.
func (e *Engine) mmMark(now float64) {
	k := 0
	for i := range e.mm.pending {
		f := &e.mm.pending[i]
		m := e.byID[f.cid]
		for len(f.marks) < len(mmHorizons) && now >= f.t+mmHorizons[len(f.marks)] {
			mid, ok := m.bk.mid()
			bid, ask, _ := m.bk.top()
			if !ok {
				mid, bid, ask = f.price, f.price, f.price
			}
			v := mid - f.price
			if !f.buy {
				v = -v
			}
			f.marks = append(f.marks, v)
			f.halfSpread = append(f.halfSpread, (ask-bid)/2)
		}
		if len(f.marks) == len(mmHorizons) {
			e.mm.done = append(e.mm.done, *f)
			if e.mm.f != nil {
				side := "buy"
				if !f.buy {
					side = "sell"
				}
				fmt.Fprintf(e.mm.f, "%.0f,%s,%s,%s,%s,%.4f,%.5f,%.5f,%.4f,%.4f,%.4f,%.4f,%s\n", f.t, f.cid, f.strat, f.rule, side, f.price,
					f.rebate, f.takerFee, f.marks[0], f.marks[1], f.marks[2], f.halfSpread[2], f.sector)
			}
			continue
		}
		e.mm.pending[k] = *f
		k++
	}
	e.mm.pending = e.mm.pending[:k]
}

// MMRow is one strategy × fill rule on the dashboard: average edge per
// share in cents at each horizon (with its standard error at 10 minutes),
// and after paying to exit as a taker at 60 minutes.
type MMRow struct {
	Strat   string     `json:"strat"`
	Rule    string     `json:"rule"`
	Fills   int        `json:"fills"`
	Edge    [3]float64 `json:"edge"` // ¢/share at 1, 10, 60 min, incl. rebate
	SE10    float64    `json:"se10"`
	Exit    float64    `json:"exit"` // ¢/share at 60 min after exiting as taker
	Rebate  float64    `json:"rebate"`
	Pending int        `json:"pending"`
}

func (mm *mmBook) summary() []MMRow {
	type key struct{ s, r string }
	acc := map[key]*MMRow{}
	sq := map[key]float64{}
	get := func(k key) *MMRow {
		if acc[k] == nil {
			acc[k] = &MMRow{Strat: k.s, Rule: k.r}
		}
		return acc[k]
	}
	for _, f := range mm.done {
		k := key{f.strat, f.rule}
		r := get(k)
		r.Fills++
		for h := range r.Edge {
			r.Edge[h] += 100 * (f.marks[h] + f.rebate)
		}
		v := 100 * (f.marks[1] + f.rebate)
		sq[k] += v * v
		r.Exit += 100 * (f.marks[2] + f.rebate - f.halfSpread[2] - f.takerFee)
		r.Rebate += 100 * f.rebate
	}
	for _, f := range mm.pending {
		get(key{f.strat, f.rule}).Pending++
	}
	var out []MMRow
	for k, r := range acc {
		if n := float64(r.Fills); n > 0 {
			for h := range r.Edge {
				r.Edge[h] = round(r.Edge[h]/n, 3)
			}
			mean := r.Edge[1]
			r.SE10 = round(math.Sqrt(math.Max(sq[k]/n-mean*mean, 0)/n), 3)
			r.Exit, r.Rebate = round(r.Exit/n, 3), round(r.Rebate/n, 4)
		}
		out = append(out, *r)
	}
	order := map[string]int{"both": 0, "signal": 1, "pull": 2}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Strat != out[b].Strat {
			return order[out[a].Strat] < order[out[b].Strat]
		}
		return out[a].Rule > out[b].Rule
	})
	return out
}
