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
}

// DefaultConfig is sized for a laptop: ~1-2 minutes of warm-up.
func DefaultConfig() Config {
	return Config{Markets: 200, History: 48 * time.Hour, Refit: 10 * time.Minute,
		MoveSize: 0.02, Horizon: 600}
}

var betas = []float64{1.0 / 10, 1.0 / 120, 1.0 / 1200}

type market struct {
	source.LiveMarket
	idx     int
	price   float64 // last YES price
	ref     float64 // price at the last counted move (2¢ hysteresis)
	lastAct float64 // last activity event time (one per second)
	recent  []float64
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
	normAct []float64 // each market's average trading rate over the window (per second)
	normMov []float64 // ... and price-move rate
	tape    []TapeRow
	status  Status
}

// Status describes progress for the UI.
type Status struct {
	Phase      string  `json:"phase"` // "loading", "fitting", "live"
	Progress   float64 `json:"progress"`
	Markets    int     `json:"markets"`
	Trades     int     `json:"trades"`
	LiveTrades int     `json:"liveTrades"`
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
	return &Engine{cfg: cfg, byID: map[string]*market{}, seen: map[string]bool{}, status: Status{Phase: "loading"}}
}

// Run loads markets and history, fits, then follows the live feed forever.
func (e *Engine) Run() error {
	ms, err := source.LiveMarkets(e.cfg.Markets)
	if err != nil {
		return fmt.Errorf("listing markets: %w", err)
	}
	e.mu.Lock()
	for i, m := range ms {
		mk := &market{LiveMarket: m, idx: i, ref: -1}
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

// follow streams live trades forever, reconnecting when the socket drops.
func (e *Engine) follow() error {
	ids := map[string]source.LiveMarket{}
	for _, m := range e.markets {
		ids[m.ConditionID] = m.LiveMarket
	}
	wait := time.Second
	for {
		start := time.Now()
		err := source.Stream(context.Background(), ids, func(t source.Trade) {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.status.Error = ""
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
		})
		if time.Since(start) > time.Minute {
			wait = time.Second
		}
		e.mu.Lock()
		e.status.Error = fmt.Sprint(err)
		e.mu.Unlock()
		log.Printf("trade stream: %v; reconnecting in %s", err, wait)
		time.Sleep(wait)
		wait = min(2*wait, time.Minute)
	}
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
