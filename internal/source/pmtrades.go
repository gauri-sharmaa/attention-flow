package source

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gauri-sharmaa/attention-flow/internal/core"
)

const pmTrades = "https://data-api.polymarket.com/trades?market=%s&limit=500&offset=%d"

type pmTrade struct {
	Side      string  `json:"side"` // BUY or SELL of Asset
	Asset     string  `json:"asset"`
	Price     float64 `json:"price"`
	Size      float64 `json:"size"`
	Timestamp int64   `json:"timestamp"`
}

// marketTrades fetches a market's trades from `from` to now. The API caps
// offset paging at 10,000 rows per query, so it asks for the whole window at
// once and, only if that overflows, splits the window in half and recurses.
// Quiet markets take a handful of requests; busy ones split as deep as they
// need. Only timestamp, side, price, size and outcome token are kept; trader
// wallets in the response are dropped.
func marketTrades(client *http.Client, conditionID string, from int64) ([]pmTrade, error) {
	return tradesIn(client, conditionID, from, time.Now().Unix())
}

func tradesIn(client *http.Client, conditionID string, a, b int64) ([]pmTrade, error) {
	var out []pmTrade
	for off := 0; off <= 10000; off += 500 {
		u := fmt.Sprintf(pmTrades, conditionID, off) + fmt.Sprintf("&start=%d&end=%d", a, b-1)
		var page []pmTrade
		var err error
		// Rate limiting shows up as a non-JSON error page; back off hard.
		for attempt := 0; attempt < 8; attempt++ {
			var resp *http.Response
			if resp, err = client.Get(u); err == nil {
				err = json.NewDecoder(resp.Body).Decode(&page)
				resp.Body.Close()
			}
			if err == nil {
				break
			}
			time.Sleep(time.Duration(min(60, 2<<attempt)) * time.Second)
		}
		if err != nil {
			return nil, err // never keep a partial history
		}
		out = append(out, page...)
		if len(page) < 500 {
			return out, nil
		}
	}
	// More than the API will page through: split the window.
	if b-a <= 600 {
		return out, nil // ten minutes with 10k+ trades: keep what we have
	}
	mid := a + (b-a)/2
	left, err := tradesIn(client, conditionID, a, mid)
	if err != nil {
		return nil, err
	}
	right, err := tradesIn(client, conditionID, mid, b)
	if err != nil {
		return nil, err
	}
	return append(left, right...), nil
}

// cachedTrades reads a market's trades from the cache if a complete fetch for
// the same window start is there, else fetches and caches them, so a long
// download can be restarted without starting over.
func cachedTrades(dir string, client *http.Client, conditionID string, from int64) ([]pmTrade, error) {
	path := filepath.Join(dir, fmt.Sprintf("%s-%d.json", conditionID, from/86400))
	if b, err := os.ReadFile(path); err == nil {
		var tr []pmTrade
		if json.Unmarshal(b, &tr) == nil {
			return tr, nil
		}
	}
	tr, err := marketTrades(client, conditionID, from)
	if err != nil {
		return tr, err
	}
	if err := os.MkdirAll(dir, 0o755); err == nil {
		if b, err := json.Marshal(tr); err == nil {
			os.WriteFile(path, b, 0o644)
		}
	}
	return tr, nil
}

// FetchMarketTrades builds two series per market from its trade log, on
// barSeconds bars over the last `days` days:
//
//   - activity: number of trades in the bar (+1), with empty bars written
//     explicitly so "nobody traded" reads as zero, not as "no data";
//   - price: last traded YES price in the bar, as odds p/(1-p).
//
// Activity is attention to the market itself; price is what it believes.
func FetchMarketTrades(outDir string, n, nClosed, days int, barSeconds int64, workers int) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	ms, err := ListActiveMarkets(n)
	if err != nil {
		return err
	}
	if nClosed > 0 {
		// Markets that resolved inside the window traded during it too;
		// leaving them out would bias every test toward survivors.
		closed, err := ListClosedMarkets(nClosed, time.Now().AddDate(0, 0, -days))
		if err != nil {
			return err
		}
		have := map[string]bool{}
		for _, m := range ms {
			have[m.ConditionID] = true
		}
		for _, m := range closed {
			if !have[m.ConditionID] {
				ms = append(ms, m)
			}
		}
	}
	fmt.Fprintf(os.Stderr, "%d markets\n", len(ms))

	now := time.Now().Unix() / barSeconds * barSeconds
	from := now - int64(days)*86400
	client := &http.Client{Timeout: 60 * time.Second}
	trades := make([][]pmTrade, len(ms))
	jobs := make(chan int)
	var wg sync.WaitGroup
	var mu sync.Mutex
	done := 0
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				tr, err := cachedTrades(filepath.Join(outDir, "cache"), client, ms[i].ConditionID, from)
				if err != nil {
					fmt.Fprintf(os.Stderr, "dropped %s: %v\n", ms[i].Question, err)
					tr = nil
				}
				mu.Lock()
				trades[i] = tr
				done++
				if done%50 == 0 {
					fmt.Fprintf(os.Stderr, "%d/%d markets\n", done, len(ms))
				}
				mu.Unlock()
			}
		}()
	}
	for i := range ms {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	nBars := int((now - from) / barSeconds)
	ticks, err := os.Create(filepath.Join(outDir, "ticks.csv"))
	if err != nil {
		return err
	}
	defer ticks.Close()
	fmt.Fprintln(ticks, "ts,market,yes_price,usd,dir")
	var names []string
	var lines []string
	var all []core.Event
	clean := strings.NewReplacer("|", " ", "\n", " ")
	total := 0
	for i, m := range ms {
		var toks []string
		json.Unmarshal([]byte(m.ClobTokenIds), &toks)
		if len(toks) < 2 {
			continue
		}
		// The last day before a market resolves is a scramble toward 0 or 1
		// that says nothing about attention; drop it.
		stop := now
		if end, err := time.Parse(time.RFC3339, m.EndDate); err == nil && end.Unix()-86400 < stop {
			stop = end.Unix() - 86400
		}
		kept := trades[i][:0]
		for _, t := range trades[i] {
			if t.Timestamp < stop {
				kept = append(kept, t)
			}
		}
		trades[i] = kept
		count := make([]int, nBars)
		last := make([]float64, nBars)
		for b := range last {
			last[b] = math.NaN()
		}
		// Oldest first, so each bar ends up with its last trade's price.
		sort.Slice(trades[i], func(a, c int) bool { return trades[i][a].Timestamp < trades[i][c].Timestamp })
		inWindow := 0
		for _, t := range trades[i] {
			if t.Timestamp < from || t.Timestamp >= now {
				continue
			}
			b := (t.Timestamp - from) / barSeconds
			count[b]++
			inWindow++
			p := t.Price
			if t.Asset == toks[1] {
				p = 1 - p // a NO trade at p is a YES price of 1-p
			}
			last[b] = p
		}
		if inWindow < 300 {
			continue // too quiet to learn anything at this resolution
		}
		total += inWindow
		event, eslug := m.Question, m.Slug
		if len(m.Events) > 0 {
			event, eslug = m.Events[0].Title, m.Events[0].Slug
		}
		sec := sectorOf(m.Question + " " + event)
		q := clean.Replace(m.Question)
		tags := strings.ReplaceAll(eslug, "-", " ")
		// Raw ticks for event-time models (Hawkes, Hayashi-Yoshida): one row
		// per trade, YES price, dollar size. Market index matches markets.txt.
		mi := len(names)
		names = append(names, q)
		for _, t := range trades[i] {
			if t.Timestamp < from || t.Timestamp >= now {
				continue
			}
			p := t.Price
			if t.Asset == toks[1] {
				p = 1 - p
			}
			// dir: +1 if the trade pushes the YES price up (buy YES or sell
			// NO), -1 if down. Signed order flow is what can predict direction.
			dir := 1
			if (t.Asset == toks[1]) == (t.Side == "BUY") {
				dir = -1
			}
			fmt.Fprintf(ticks, "%d,%d,%.4f,%.2f,%d\n", t.Timestamp, mi, p, t.Size*t.Price, dir)
		}
		actID, pxID := len(lines), len(lines)+1
		lines = append(lines,
			fmt.Sprintf("Activity: %s | - | %s-activity/%s | %s", q, sec, eslug, tags),
			fmt.Sprintf("Price: %s | - | %s-price/%s | %s", q, sec, eslug, tags))
		for b := 0; b < nBars; b++ {
			ts := from + int64(b)*barSeconds
			all = append(all, core.Event{TS: ts, Entity: int32(actID), Value: float64(count[b] + 1)})
			if !math.IsNaN(last[b]) {
				p := math.Min(0.99, math.Max(0.01, last[b]))
				all = append(all, core.Event{TS: ts, Entity: int32(pxID), Value: p / (1 - p)})
			}
		}
	}
	if err := os.WriteFile(filepath.Join(outDir, "markets.txt"), []byte(strings.Join(names, "\n")+"\n"), 0o644); err != nil {
		return err
	}
	uni := "# Polymarket: per market, trading activity and price from its trade log\n" + strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(outDir, "universe.txt"), []byte(uni), 0o644); err != nil {
		return err
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].TS < all[j].TS })
	f, err := os.Create(filepath.Join(outDir, "events.csv"))
	if err != nil {
		return err
	}
	defer f.Close()
	fmt.Fprintf(os.Stderr, "%d markets kept, %d trades, %d bars\n", len(lines)/2, total, nBars)
	return core.WriteEvents(f, all)
}
