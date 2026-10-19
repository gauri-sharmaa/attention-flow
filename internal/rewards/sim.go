package rewards

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gauri-sharmaa/attention-flow/internal/source"
)

// A live paper test of collecting liquidity rewards. Every minute (the rate
// Polymarket samples at) each market gets fresh paper orders of the minimum
// qualifying size at the best prices inside the reward band, and is credited
// its share of that minute's pool: our score over ours plus everyone
// else's, scored on the real order book. Nothing is sent to the exchange.
//
// Fills come from real trades: a paper order waits behind what was resting
// at its price when it was posted (moving up as those orders are cancelled)
// and fills once that much has traded there, or at once if a trade goes
// through its price. Each fill changes the paper position; the test reports
// rewards, the position's value at the midpoint, and markouts.

// SimMarket is one market in the test.
type SimMarket struct {
	source.LiveMarket
	Daily     float64 // $ a day in the pool
	MaxSpread float64 // band, cents from the midpoint
	MinSize   float64 // shares
	End       time.Time
}

type simOrder struct {
	Live  bool
	Price float64
	Ahead float64 // shares still queued ahead of ours at our price
}

type simFill struct {
	T, Price, Mid float64
	Buy           bool
	Marks         []float64
}

type simState struct {
	Rewards     float64 // $
	Pos, Cash   float64 // YES shares held (negative: short YES, i.e. long NO); cash flow
	Minutes     int     // minutes sampled with a valid book
	Quoted      int     // minutes with at least one order in the band
	ShareSum    float64
	Fills       []simFill
	Bid, Ask    simOrder
	MaxCapital  float64
	LastMid     float64
	Spread      float64
	FeeRateSeen float64
}

// Sim runs the test.
type Sim struct {
	mu      sync.Mutex
	markets map[string]*SimMarket
	state   map[string]*simState
	books   map[string]*simBook
	dir     string
	maxInv  float64 // most shares held either way, as a multiple of the order size
	start   time.Time
	minutes *os.File
	fills   *os.File
}

type simBook struct{ bids, asks map[float64]float64 }

func (b *simBook) top() (bid, ask float64, ok bool) {
	bid, ask = math.Inf(-1), math.Inf(1)
	for p := range b.bids {
		bid = math.Max(bid, p)
	}
	for p := range b.asks {
		ask = math.Min(ask, p)
	}
	return bid, ask, !math.IsInf(bid, 0) && !math.IsInf(ask, 0) && bid < ask
}

func (b *simBook) levels() (bids, asks []source.Level) {
	for p, s := range b.bids {
		bids = append(bids, source.Level{Price: p, Size: s})
	}
	for p, s := range b.asks {
		asks = append(asks, source.Level{Price: p, Size: s})
	}
	return
}

// NewSim prepares a test, resuming from dir/state.json if a run is there.
func NewSim(dir string, markets []SimMarket, maxInv float64) *Sim {
	s := &Sim{markets: map[string]*SimMarket{}, state: map[string]*simState{}, books: map[string]*simBook{}, dir: dir, maxInv: maxInv, start: time.Now()}
	os.MkdirAll(dir, 0o755)
	for i := range markets {
		m := &markets[i]
		s.markets[m.ConditionID] = m
		s.state[m.ConditionID] = &simState{}
		s.books[m.ConditionID] = &simBook{map[float64]float64{}, map[float64]float64{}}
	}
	var saved struct {
		Start time.Time
		State map[string]*simState
	}
	if b, err := os.ReadFile(filepath.Join(dir, "state.json")); err == nil && json.Unmarshal(b, &saved) == nil {
		s.start = saved.Start
		for id, st := range saved.State {
			if _, ok := s.state[id]; ok {
				st.Bid, st.Ask = simOrder{}, simOrder{} // orders don't survive a restart
				s.state[id] = st
			}
		}
	}
	s.minutes = open(filepath.Join(dir, "minutes.csv"), "t,market,mid,bid,ask,our_score,their_score,share,reward,pos")
	s.fills = open(filepath.Join(dir, "fills.csv"), "t,market,side,price,mid,mark_1m,mark_10m,mark_60m,pos_after")
	return s
}

// Started is when the test began (earlier than now after a resume).
func (s *Sim) Started() time.Time { return s.start }

func open(path, header string) *os.File {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil
	}
	if st, _ := f.Stat(); st != nil && st.Size() == 0 {
		fmt.Fprintln(f, header)
	}
	return f
}

var simHorizons = []float64{60, 600, 3600}

// Run follows the markets until ctx ends.
func (s *Sim) Run(ctx context.Context) {
	live := map[string]source.LiveMarket{}
	for id, m := range s.markets {
		live[id] = m.LiveMarket
	}
	s.resnapshot()
	go func() {
		for ctx.Err() == nil {
			source.Stream(ctx, live, s.onTrade, s.onBook)
			time.Sleep(3 * time.Second)
		}
	}()
	minute := time.NewTicker(time.Minute)
	snap := time.NewTicker(10 * time.Minute)
	defer minute.Stop()
	defer snap.Stop()
	for {
		select {
		case <-ctx.Done():
			s.save()
			return
		case now := <-minute.C:
			s.sample(now)
		case <-snap.C:
			s.resnapshot() // websocket deltas can drift; start each market over from the full book
			s.save()
		}
	}
}

func (s *Sim) resnapshot() {
	for id, m := range s.markets {
		bids, asks, err := source.PolyBookLevels(m.YesToken)
		if err != nil {
			continue
		}
		s.onBook(source.BookUpdate{ConditionID: id, Snapshot: true, Bids: bids, Asks: asks})
		time.Sleep(100 * time.Millisecond)
	}
}

func (s *Sim) onBook(u source.BookUpdate) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.books[u.ConditionID]
	if b == nil {
		return
	}
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
	// Orders ahead of ours that get cancelled move us up the queue.
	st := s.state[u.ConditionID]
	// (A level smaller than the queue we think is ahead means some of it left.)
	if st.Bid.Live {
		st.Bid.Ahead = math.Min(st.Bid.Ahead, b.bids[st.Bid.Price])
	}
	if st.Ask.Live {
		st.Ask.Ahead = math.Min(st.Ask.Ahead, b.asks[st.Ask.Price])
	}
}

func (s *Sim) onTrade(t source.Trade) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, m := s.state[t.ConditionID], s.markets[t.ConditionID]
	if st == nil {
		return
	}
	size := m.MinSize
	try := func(o *simOrder, buy bool) {
		if !o.Live {
			return
		}
		hits := (buy && t.Dir < 0) || (!buy && t.Dir > 0)
		if !hits {
			return
		}
		through := (buy && t.YesPrice < o.Price-1e-9) || (!buy && t.YesPrice > o.Price+1e-9)
		if math.Abs(t.YesPrice-o.Price) < 1e-9 {
			o.Ahead -= t.Shares // the queue ahead of us trades first
		}
		if !through && o.Ahead > -size {
			return // not yet traded through all of the queue and our order
		}
		o.Live = false
		if buy {
			st.Pos += size
			st.Cash -= size * o.Price
		} else {
			st.Pos -= size
			st.Cash += size * o.Price
		}
		mid := st.LastMid
		st.Fills = append(st.Fills, simFill{T: float64(t.T), Price: o.Price, Mid: mid, Buy: buy})
	}
	try(&st.Bid, true)
	try(&st.Ask, false)
}

// sample re-posts every market's orders and credits this minute's rewards.
func (s *Sim) sample(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ts := float64(now.Unix())
	for id, m := range s.markets {
		st, b := s.state[id], s.books[id]
		bb, ba, ok := b.top()
		if !ok {
			st.Bid.Live, st.Ask.Live = false, false
			continue
		}
		v, size := m.MaxSpread, m.MinSize
		// Polymarket measures from a midpoint that ignores orders smaller
		// than the minimum size, so a few tiny orders can't move it.
		mid := adjustedMid(b, size, bb, ba)
		st.LastMid, st.Spread = (bb+ba)/2, ba-bb
		st.Minutes++
		// Best prices, pulled inside the band when the best is outside it.
		bid := math.Max(bb, math.Ceil((mid-v/100)*100+1e-6)/100)
		ask := math.Min(ba, math.Floor((mid+v/100)*100-1e-6)/100)
		wantBid := bid > 0 && bid < ba && 100*(mid-bid) < v && st.Pos < s.maxInv*size
		wantAsk := ask < 1 && ask > bb && 100*(ask-mid) < v && st.Pos > -s.maxInv*size
		post := func(o *simOrder, want bool, price float64, side map[float64]float64) {
			if !want {
				o.Live = false
				return
			}
			if o.Live && math.Abs(o.Price-price) < 1e-9 {
				return // same price: keep our place in the queue
			}
			*o = simOrder{Live: true, Price: price, Ahead: side[price]}
		}
		post(&st.Bid, wantBid, bid, b.bids)
		post(&st.Ask, wantAsk, ask, b.asks)
		ourB, ourA := 0.0, 0.0
		if st.Bid.Live {
			ourB = Score(v, 100*(mid-st.Bid.Price), size)
		}
		if st.Ask.Live {
			ourA = Score(v, 100*(st.Ask.Price-mid), size)
		}
		ours := Credit(ourB, ourA, mid)
		bids, asks := b.levels()
		tb, ta := BookScores(bids, asks, mid, v, size)
		theirs := Credit(tb, ta, mid)
		share, reward := 0.0, 0.0
		if ours > 0 {
			share = ours / (ours + theirs)
			reward = share * m.Daily / 1440
			st.Quoted++
		}
		st.Rewards += reward
		st.ShareSum += share
		capital := 0.0
		if st.Bid.Live {
			capital += size * st.Bid.Price
		}
		if st.Ask.Live {
			capital += size * (1 - st.Ask.Price)
		}
		if st.Pos > 0 {
			capital += st.Pos * mid
		} else {
			capital += -st.Pos * (1 - mid)
		}
		st.MaxCapital = math.Max(st.MaxCapital, capital)
		if s.minutes != nil {
			fmt.Fprintf(s.minutes, "%.0f,%s,%.4f,%.3f,%.3f,%.2f,%.2f,%.4f,%.5f,%.0f\n", ts, id, mid, liveP(st.Bid), liveP(st.Ask), ours, theirs, share, reward, st.Pos)
		}
		// Markouts for fills whose horizons have passed.
		for i := range st.Fills {
			f := &st.Fills[i]
			for len(f.Marks) < len(simHorizons) && ts >= f.T+simHorizons[len(f.Marks)] {
				d := mid - f.Price
				if !f.Buy {
					d = -d
				}
				f.Marks = append(f.Marks, d)
				if len(f.Marks) == len(simHorizons) && s.fills != nil {
					side := "buy"
					if !f.Buy {
						side = "sell"
					}
					fmt.Fprintf(s.fills, "%.0f,%s,%s,%.3f,%.4f,%.4f,%.4f,%.4f,%.0f\n", f.T, id, side, f.Price, f.Mid, f.Marks[0], f.Marks[1], f.Marks[2], st.Pos)
				}
			}
		}
	}
}

func adjustedMid(b *simBook, minSize, bb, ba float64) float64 {
	bid, ask := math.Inf(-1), math.Inf(1)
	for p, sz := range b.bids {
		if sz >= minSize {
			bid = math.Max(bid, p)
		}
	}
	for p, sz := range b.asks {
		if sz >= minSize {
			ask = math.Min(ask, p)
		}
	}
	if math.IsInf(bid, 0) {
		bid = bb
	}
	if math.IsInf(ask, 0) {
		ask = ba
	}
	return (bid + ask) / 2
}

func liveP(o simOrder) float64 {
	if !o.Live {
		return math.NaN()
	}
	return o.Price
}

func (s *Sim) save() {
	s.mu.Lock()
	b, err := json.Marshal(struct {
		Start time.Time
		State map[string]*simState
	}{s.start, s.state})
	s.mu.Unlock()
	if err == nil {
		tmp := filepath.Join(s.dir, "state.json.tmp")
		if os.WriteFile(tmp, b, 0o644) == nil {
			os.Rename(tmp, filepath.Join(s.dir, "state.json"))
		}
	}
}

// Report summarizes the test so far.
func (s *Sim) Report() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	type row struct {
		q                                              string
		rewards, pnl, exit, capital, share, quotedFrac float64
		fills                                          int
		mark10                                         float64
		marked                                         int
	}
	var rows []row
	var tot row
	for id, m := range s.markets {
		st := s.state[id]
		r := row{q: m.Question, rewards: st.Rewards, capital: st.MaxCapital, fills: len(st.Fills)}
		r.pnl = st.Cash + st.Pos*st.LastMid // the position at the midpoint
		r.exit = r.pnl - math.Abs(st.Pos)*st.Spread/2
		if st.Minutes > 0 {
			r.share = st.ShareSum / float64(st.Minutes)
			r.quotedFrac = float64(st.Quoted) / float64(st.Minutes)
		}
		for _, f := range st.Fills {
			if len(f.Marks) >= 2 {
				r.mark10 += f.Marks[1]
				r.marked++
			}
		}
		rows = append(rows, r)
		tot.rewards += r.rewards
		tot.pnl += r.pnl
		tot.exit += r.exit
		tot.capital += r.capital
		tot.fills += r.fills
		tot.mark10 += r.mark10
		tot.marked += r.marked
	}
	sort.Slice(rows, func(a, b int) bool { return rows[a].rewards+rows[a].exit > rows[b].rewards+rows[b].exit })
	// Rates count the minutes actually sampled, not time the process was down.
	sampled := 0
	for _, st := range s.state {
		sampled = max(sampled, st.Minutes)
	}
	hours := float64(sampled) / 60
	var sb strings.Builder
	fmt.Fprintf(&sb, "LIQUIDITY-REWARDS PAPER TEST · %d markets · %.1f hours sampled since %s UTC (%.1f hours ago)\n", len(rows), hours,
		s.start.UTC().Format("Jan 2 15:04"), time.Since(s.start).Hours())
	fmt.Fprintln(&sb, "minimum-size paper orders on both sides at the best prices inside each market's reward band, re-posted every minute and")
	fmt.Fprintln(&sb, "scored against the real order book; fills from real trades after the queue ahead of us. Nothing is sent to the exchange.")
	fmt.Fprintf(&sb, "\n%-58s %8s %6s %7s %6s %9s %9s %9s %8s\n", "market", "rewards", "share", "quoted", "fills", "position", "after exit", "net", "capital")
	for _, r := range rows {
		fmt.Fprintf(&sb, "%-58.58s %8.2f %5.1f%% %6.0f%% %6d %+9.2f %+9.2f %+9.2f %8.0f\n", r.q, r.rewards, 100*r.share, 100*r.quotedFrac, r.fills, r.pnl, r.exit, r.rewards+r.exit, r.capital)
	}
	fmt.Fprintf(&sb, "\nTOTAL rewards $%.2f · position at midpoint $%+.2f · after paying half the spread to exit $%+.2f · NET $%+.2f\n", tot.rewards, tot.pnl, tot.exit, tot.rewards+tot.exit)
	if hours > 0 {
		fmt.Fprintf(&sb, "per day at this rate: rewards $%.0f · net $%+.0f · on the most capital tied up at once in each market (sum $%.0f)\n", tot.rewards*24/hours, (tot.rewards+tot.exit)*24/hours, tot.capital)
	}
	if tot.marked > 0 {
		fmt.Fprintf(&sb, "fills: %d · the price 10 minutes after a fill moved %+.2f¢ per share our way on average (negative: we were picked off)\n", tot.fills, 100*tot.mark10/float64(tot.marked))
	}
	fmt.Fprintln(&sb, "caveats: our orders aren't really in the book, so no one reacts to them; queue position is estimated; rewards use Polymarket's")
	fmt.Fprintln(&sb, "published formula, sampled each minute, not its actual payout; markets were chosen at the start from an estimate.")
	return sb.String()
}
