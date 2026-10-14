// Package live runs the Polymarket view in real time: it follows the busiest
// markets' trades, keeps two Hawkes models fitted on a rolling window, and
// turns them into a snapshot for the dashboard.
//
//   - Cross-market model: does trading in one market set off related ones?
//   - Price-move model: does trading activity predict 2¢ price moves? This is
//     the finding that held out of sample, so it drives the "heating up" list.
//
// Everything uses public, keyless endpoints, so anyone can run it.
package live

import (
	"context"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gauri-sharmaa/attention-flow/internal/core"
	"github.com/gauri-sharmaa/attention-flow/internal/hawkes"
	"github.com/gauri-sharmaa/attention-flow/internal/semantic"
	"github.com/gauri-sharmaa/attention-flow/internal/source"
)

// Config controls the live view.
type Config struct {
	Markets  int           // how many of the busiest markets to follow
	History  time.Duration // rolling window the models are fitted on
	Refit    time.Duration // how often to refit
	MoveSize float64       // price move that counts, in YES-price units
	Horizon  float64       // seconds ahead for "price move likely"
	Record   string        // directory for the track record ("" keeps it in memory only)
	Every    time.Duration // how often predictions are written down
}

// DefaultConfig is sized for a laptop: ~1-2 minutes of warm-up.
func DefaultConfig() Config {
	return Config{Markets: 200, History: 48 * time.Hour, Refit: 10 * time.Minute,
		MoveSize: 0.02, Horizon: 600, Record: "data/live", Every: time.Minute}
}

var betas = []float64{1.0 / 10, 1.0 / 120, 1.0 / 1200}

type market struct {
	source.LiveMarket
	idx     int
	price   float64 // last YES price
	ref     float64 // price at the last counted move (2¢ hysteresis)
	lastAct float64 // last activity event time (one per second)
	recent  []float64
	moves   []float64    // times of 2¢ trade-price moves (for scoring predictions)
	flow    [][2]float64 // live trades: time, signed dollars (+ pushes YES up)
	bk      *book
	midRef  float64 // midpoint at the last 2¢ midpoint move
	lastMid float64 // direction of that move (+1, -1, 0 none yet)
	open    *dirCall
}

// Engine owns all state; Snapshot and the feed loop lock mu.
type Engine struct {
	cfg     Config
	mu      sync.Mutex
	markets []*market
	byID    map[string]*market
	cand    [][]int // cross-market candidate parents
	moveC   [][]int // parents for the 2N-stream price-move model
	events  []hawkes.Event
	seen    map[string]bool
	cross   *hawkes.Live
	moves   *hawkes.Live
	tape    []TapeRow
	status  Status
	normAct []float64 // each market's average trading rate over the window (per second)
	normMov []float64 // ... and price-move rate
	rec     *recorder
	mm      *mmBook
	down    float64 // when the feed dropped (0 = up)
}

// Status describes progress for the UI.
type Status struct {
	Phase      string  `json:"phase"` // "loading", "fitting", "live"
	Progress   float64 `json:"progress"`
	Markets    int     `json:"markets"`
	Trades     int     `json:"trades"`
	LiveTrades int     `json:"liveTrades"`
	LiveMoves  int     `json:"liveMoves"`
	FittedAt   int64   `json:"fittedAt"`
	FitSeconds float64 `json:"fitSeconds"`
	Window     string  `json:"window"`
	Error      string  `json:"error,omitempty"`
}

// TapeRow is one recent trade.
type TapeRow struct {
	T      int64   `json:"t"`
	Market int     `json:"m"`
	Price  float64 `json:"p"` // YES price
	USD    float64 `json:"usd"`
	Dir    int     `json:"dir"`
}

// New prepares an engine; Run does the work.
func New(cfg Config) *Engine {
	return &Engine{cfg: cfg, byID: map[string]*market{}, seen: map[string]bool{}, status: Status{Phase: "loading"},
		rec: newRecorder(cfg.Record, cfg.Horizon), mm: newMM(cfg.Record)}
}

// Run loads markets and history, fits, then follows the live feed forever.
func (e *Engine) Run() error {
	ms, err := source.LiveMarkets(e.cfg.Markets)
	if err != nil {
		return fmt.Errorf("listing markets: %w", err)
	}
	e.mu.Lock()
	for i, m := range ms {
		mk := &market{LiveMarket: m, idx: i, ref: -1, bk: newBook(), midRef: -1}
		e.markets = append(e.markets, mk)
		e.byID[m.ConditionID] = mk
	}
	e.status.Markets = len(e.markets)
	e.buildCandidates()
	e.mu.Unlock()
	log.Printf("following %d markets; loading %s of trades", len(ms), e.cfg.History)

	// Warm-up history, in parallel.
	from := time.Now().Add(-e.cfg.History).Unix()
	hist := make([][]source.Trade, len(ms))
	var wg sync.WaitGroup
	jobs := make(chan int)
	var done int
	var dmu sync.Mutex
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				tr, err := source.History(ms[i], from)
				if err != nil {
					log.Printf("history %s: %v", ms[i].Question, err)
				}
				hist[i] = tr
				dmu.Lock()
				done++
				e.mu.Lock()
				e.status.Progress = float64(done) / float64(len(ms))
				e.mu.Unlock()
				dmu.Unlock()
			}
		}()
	}
	for i := range ms {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	var all []source.Trade
	for _, h := range hist {
		all = append(all, h...)
	}
	sort.Slice(all, func(a, b int) bool { return all[a].T < all[b].T })
	e.mu.Lock()
	for _, t := range all {
		e.ingest(t, false)
	}
	e.status.Trades = len(all)
	e.mu.Unlock()

	e.refit()
	go func() {
		for range time.Tick(e.cfg.Refit) {
			e.refit()
		}
	}()
	go func() {
		for range time.Tick(e.cfg.Every) {
			e.predictTick(float64(time.Now().Unix()))
		}
	}()
	go func() { // paper market maker: requote and score every 10 seconds
		for range time.Tick(10 * time.Second) {
			e.mmTick(float64(time.Now().Unix()))
		}
	}()
	return e.follow()
}

// buildCandidates sets who may excite whom: sibling outcomes of one event and
// each market's most similar markets by wording.
func (e *Engine) buildCandidates() {
	u := &core.Universe{}
	for i, m := range e.markets {
		u.Entities = append(u.Entities, core.Entity{ID: i, Name: m.Question, Cluster: m.Sector,
			Subtopic: m.Sector + "/" + m.Event, Tags: strings.Split(m.Event, "-")})
	}
	n := len(e.markets)
	e.cand = make([][]int, n)
	add := func(c [][]int, a, b int) {
		for _, x := range c[a] {
			if x == b {
				return
			}
		}
		c[a] = append(c[a], b)
	}
	for _, p := range semantic.Candidates(u, 8, 0.05) {
		add(e.cand, p.A, p.B)
		add(e.cand, p.B, p.A)
	}
	for i := range e.markets {
		for j := range e.markets {
			if i != j && e.markets[i].Event == e.markets[j].Event {
				add(e.cand, i, j)
			}
		}
	}
	// Price-move model: streams 0..n-1 are activity, n..2n-1 are moves. A
	// market's moves can be set off by its own and its candidates' activity
	// and by its candidates' moves.
	e.moveC = make([][]int, 2*n)
	for i := 0; i < n; i++ {
		e.moveC[i] = append([]int(nil), e.cand[i]...)
		e.moveC[n+i] = append(e.moveC[n+i], i)
		for _, j := range e.cand[i] {
			e.moveC[n+i] = append(e.moveC[n+i], j, n+j)
		}
	}
}

// ingest records one trade (caller holds mu). Activity is one event per
// market per second (a split order is one decision); a move event fires when
// the YES price is MoveSize away from the price at the previous move.
func (e *Engine) ingest(t source.Trade, live bool) {
	m := e.byID[t.ConditionID]
	if m == nil {
		return
	}
	ts := float64(t.T)
	n := len(e.markets)
	var add []hawkes.Event
	if ts > m.lastAct {
		add = append(add, hawkes.Event{T: ts, Dim: m.idx})
		m.lastAct = ts
		m.recent = append(m.recent, ts)
	}
	m.price = t.YesPrice
	if m.ref < 0 {
		m.ref = t.YesPrice
	} else if math.Abs(t.YesPrice-m.ref) >= e.cfg.MoveSize-1e-9 {
		m.ref = t.YesPrice
		add = append(add, hawkes.Event{T: ts, Dim: n + m.idx})
		m.moves = append(m.moves, ts)
		if live {
			e.status.LiveMoves++
		}
	}
	for _, ev := range add {
		e.events = append(e.events, ev)
		if e.cross != nil && ev.Dim < n {
			e.cross.Add(ev)
		}
		if e.moves != nil {
			e.moves.Add(ev)
		}
	}
	if live {
		e.mmTrade(m, ts, t.YesPrice, t.Dir, t.Shares)
		m.flow = append(m.flow, [2]float64{ts, float64(t.Dir) * t.USD})
		e.status.LiveTrades++
		e.tape = last(append(e.tape, TapeRow{T: t.T, Market: m.idx, Price: round(t.YesPrice, 3), USD: round(t.USD, 0), Dir: t.Dir}), 60)
	}
}

// refit fits both models on the rolling window, outside the lock, then
// swaps in live trackers replayed over the window.
func (e *Engine) refit() {
	e.mu.Lock()
	now := float64(time.Now().Unix())
	from := now - e.cfg.History.Seconds()
	cut := sort.Search(len(e.events), func(i int) bool { return e.events[i].T >= from })
	e.events = append([]hawkes.Event(nil), e.events[cut:]...)
	evs := append([]hawkes.Event(nil), e.events...)
	n := len(e.markets)
	cand, moveC := e.cand, e.moveC
	e.status.Phase = map[bool]string{true: "fitting", false: e.status.Phase}[e.cross == nil]
	e.mu.Unlock()
	if len(evs) < 100 {
		return
	}
	start := time.Now()
	sort.SliceStable(evs, func(a, b int) bool { return evs[a].T < evs[b].T })
	var act []hawkes.Event
	for _, ev := range evs {
		if ev.Dim < n {
			act = append(act, ev)
		}
	}
	t1 := now + 1
	cross := hawkes.New(n, betas, cand)
	cross.Fit(act, hawkes.Options{Iters: 150, Tol: 1e-7, L1: 5, Window: [2]float64{from, t1}})
	mv := hawkes.New(2*n, betas, moveC)
	mv.Fit(evs, hawkes.Options{Iters: 150, Tol: 1e-7, L1: 5, Window: [2]float64{from, t1}})
	normAct, normMov := make([]float64, n), make([]float64, n)
	for _, ev := range evs {
		if ev.Dim < n {
			normAct[ev.Dim]++
		} else {
			normMov[ev.Dim-n]++
		}
	}
	for i := range normAct {
		normAct[i] /= t1 - from
		normMov[i] /= t1 - from
	}
	cl, ml := cross.NewLive(), mv.NewLive()
	for _, ev := range act {
		cl.Add(ev)
	}
	for _, ev := range evs {
		ml.Add(ev)
	}
	e.mu.Lock()
	// Events that arrived during the fit join the new trackers too.
	for _, ev := range e.events[len(evs):] {
		if ev.Dim < n {
			cl.Add(ev)
		}
		ml.Add(ev)
	}
	e.cross, e.moves = cl, ml
	e.normAct, e.normMov = normAct, normMov
	e.status.Phase = "live"
	e.status.FittedAt = time.Now().Unix()
	e.status.FitSeconds = round(time.Since(start).Seconds(), 1)
	e.status.Window = e.cfg.History.String()
	e.mu.Unlock()
	log.Printf("refit on %d events in %.1fs", len(evs), time.Since(start).Seconds())
}

// follow streams live trades and order books forever, reconnecting when the
// socket drops. Time without a feed is an outage for the track record.
func (e *Engine) follow() error {
	ids := map[string]source.LiveMarket{}
	for _, m := range e.markets {
		ids[m.ConditionID] = m.LiveMarket
	}
	back := func() { // caller holds mu
		if e.down > 0 {
			e.rec.outage(e.down, float64(time.Now().Unix())+1)
			e.down = 0
		}
		e.status.Error = ""
	}
	wait := time.Second
	for {
		start := time.Now()
		err := source.Stream(context.Background(), ids, func(t source.Trade) {
			e.mu.Lock()
			defer e.mu.Unlock()
			back()
			if e.seen[t.Key] {
				return
			}
			e.seen[t.Key] = true
			if len(e.seen) > 200000 {
				e.seen = map[string]bool{}
			}
			if len(e.events) > 0 && float64(t.T) < e.events[len(e.events)-1].T {
				t.T = int64(e.events[len(e.events)-1].T) // keep time order
			}
			e.ingest(t, true)
		}, func(u source.BookUpdate) {
			e.mu.Lock()
			defer e.mu.Unlock()
			back()
			e.onBook(u)
		})
		if time.Since(start) > time.Minute {
			wait = time.Second
		}
		e.mu.Lock()
		e.status.Error = fmt.Sprint(err)
		if e.down == 0 {
			e.down = float64(time.Now().Unix())
		}
		e.mu.Unlock()
		log.Printf("trade stream: %v; reconnecting in %s", err, wait)
		time.Sleep(wait)
		wait = min(2*wait, time.Minute)
	}
}

// onBook applies an order-book change and settles the market's open
// direction call when the midpoint moves 2¢ (caller holds mu).
func (e *Engine) onBook(u source.BookUpdate) {
	m := e.byID[u.ConditionID]
	if m == nil {
		return
	}
	m.bk.apply(u)
	mid, ok := m.bk.mid()
	if !ok {
		return
	}
	if m.midRef < 0 {
		m.midRef = mid
		return
	}
	if math.Abs(mid-m.midRef) < e.cfg.MoveSize-1e-9 {
		return
	}
	up := mid > m.midRef
	if m.open != nil {
		e.rec.settleDir(*m.open, up)
		m.open = nil
	}
	m.lastMid = map[bool]float64{true: 1, false: -1}[up]
	m.midRef = mid
}

// predictTick writes down this minute's predictions and settles the ones
// whose horizon has passed.
func (e *Engine) predictTick(now float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cross == nil || e.moves == nil {
		return
	}
	keep := now - 2*e.cfg.Horizon
	for i, m := range e.markets {
		k := 0
		for k < len(m.moves) && m.moves[k] < keep {
			k++
		}
		m.moves = m.moves[k:]
		if e.down == 0 {
			_, pm, pn, ok := e.odds(i, now)
			if ok {
				e.rec.predict(prediction{now, m.ConditionID, pm, pn})
			}
		}
		if m.open != nil && now-m.open.t > 6*3600 {
			m.open = nil // no 2¢ midpoint move in six hours: no outcome
		}
		if m.open == nil && e.down == 0 {
			if x, ok := e.bookFeatures(m, now); ok {
				m.open = &dirCall{now, m.ConditionID, x, e.rec.learner.prob(x), e.rec.pLast(m.lastMid)}
			}
		}
	}
	e.rec.resolve(now, func(cid string, from, to float64) bool {
		m := e.byID[cid]
		if m == nil {
			return false
		}
		for _, t := range m.moves {
			if t > from && t <= to {
				return true
			}
		}
		return false
	})
}

// mmTick scores paper fills and replaces every market's paper orders.
func (e *Engine) mmTick(now float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.mmMark(now)
	if e.down > 0 || e.cross == nil {
		for k := range e.mm.orders {
			delete(e.mm.orders, k) // no quotes while the feed is down
		}
		return
	}
	for _, m := range e.markets {
		e.requote(m, now)
	}
}

// odds returns a market's trading heat and its chance of a 2¢ move within
// the horizon, now and at a normal moment (caller holds mu).
func (e *Engine) odds(i int, now float64) (heat, pmove, pnorm float64, ok bool) {
	norm := e.normAct[i]
	if norm <= 0 {
		return 0, 0, 0, false
	}
	n := len(e.markets)
	heat = e.cross.Rate(i, now) / norm
	pmove = 1 - math.Exp(-e.moves.Expected(n+i, now, e.cfg.Horizon))
	pnorm = 1 - math.Exp(-e.normMov[i]*e.cfg.Horizon)
	return heat, pmove, pnorm, true
}

// bookFeatures describes a market's order book and recent flow for the
// direction learner; ok is false without a two-sided book.
func (e *Engine) bookFeatures(m *market, now float64) ([]float64, bool) {
	bid, ask, ok := m.bk.top()
	if !ok {
		return nil, false
	}
	mid := (bid + ask) / 2
	k := 0
	for k < len(m.flow) && m.flow[k][0] < now-300 {
		k++
	}
	m.flow = m.flow[k:]
	var net, gross float64
	for _, f := range m.flow {
		net += f[1]
		gross += math.Abs(f[1])
	}
	flow := 0.0
	if gross > 0 {
		flow = net / gross
	}
	return []float64{m.bk.imbalance(0.01), m.bk.imbalance(0.03), math.Min((ask-bid)*100, 20) / 5,
		flow, m.lastMid, mid - 0.5, math.Abs(mid - 0.5)}, true
}

func last(r []TapeRow, n int) []TapeRow {
	if len(r) <= n {
		return r
	}
	return append([]TapeRow(nil), r[len(r)-n:]...)
}

func round(x float64, d int) float64 {
	p := math.Pow(10, float64(d))
	return math.Round(x*p) / p
}
