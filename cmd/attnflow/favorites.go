package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/gauri-sharmaa/attention-flow/internal/source"
)

// cmdFavorites tests the "bond" strategy: shortly before a market's
// scheduled end, buy whichever side is priced at 95¢ or more and hold it to
// the payout. It wins small almost every time; the question is whether the
// rare loss (−95¢) eats all the 5¢ wins.
//
// Done without looking ahead:
//   - entry is a fixed time before the market's scheduled end date, which is
//     published when the market opens (not "a day before the last trade");
//   - the market has to still be open then, with a trade in the day before
//     (the price a trader would have seen);
//   - the winner is Polymarket's official settlement, not the last price;
//   - the buy costs the last price plus half a cent, plus Polymarket's taker
//     fee where the market charges one.
func cmdFavorites(args []string) error {
	fs := flag.NewFlagSet("favorites", flag.ExitOnError)
	dir := fs.String("dir", "data/pmt365", "polytrades output (cache/ of per-market trades)")
	slip := fs.Float64("slip", 0.005, "paid above the last trade price when buying")
	stake := fs.Float64("stake", 100, "dollars per trade")
	fs.Parse(args)

	files, _ := filepath.Glob(filepath.Join(*dir, "cache", "*.json"))
	sort.Strings(files)
	byID := map[string]string{} // newest cache file per market
	for _, f := range files {
		base := filepath.Base(f)
		if i := strings.LastIndex(base, "-"); i > 0 {
			byID[base[:i]] = f
		}
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	meta, err := loadMeta(filepath.Join(*dir, "meta.json"), ids)
	if err != nil {
		return err
	}

	type trade struct {
		Asset     string  `json:"asset"`
		Price     float64 `json:"price"`
		Timestamp int64   `json:"timestamp"`
	}
	type mkt struct {
		m      source.PolyMeta
		t      []int64
		p0     []float64 // price of outcome 0 implied by each trade
		won0   bool
		close  int64
		sector string
	}
	var mkts []mkt
	skipped := map[string]int{}
	for _, id := range ids {
		m, ok := meta[id]
		switch {
		case !ok:
			skipped["not found"]++
			continue
		case !m.Closed || len(m.Final) != 2 || len(m.Tokens) != 2 || m.End.IsZero():
			skipped["open or not yes/no"]++
			continue
		case m.Final[0] != 1 && m.Final[1] != 1:
			skipped["no clear winner (split or void)"]++
			continue
		}
		b, err := os.ReadFile(byID[id])
		if err != nil {
			continue
		}
		var tr []trade
		if json.Unmarshal(b, &tr) != nil || len(tr) < 20 {
			skipped["under 20 trades"]++
			continue
		}
		sort.Slice(tr, func(a, c int) bool { return tr[a].Timestamp < tr[c].Timestamp })
		x := mkt{m: m, won0: m.Final[0] == 1, close: tr[len(tr)-1].Timestamp}
		if !m.ClosedAt.IsZero() {
			x.close = m.ClosedAt.Unix()
		}
		for _, t := range tr {
			p := t.Price
			if t.Asset != m.Tokens[0] {
				p = 1 - p
			}
			x.t = append(x.t, t.Timestamp)
			x.p0 = append(x.p0, p)
		}
		x.sector = source.SectorOf(m.Question + " " + m.Event)
		if vsRe.MatchString(m.Question) {
			x.sector = "sports"
		}
		mkts = append(mkts, x)
	}
	fmt.Printf("FAVORITES (\"bond\") · %d resolved yes/no markets from the year's busiest %d", len(mkts), len(ids))
	for k, n := range skipped {
		fmt.Printf(" · skipped %d %s", n, k)
	}
	fmt.Println()
	fmt.Printf("buy the side priced ≥ threshold at a fixed time before the scheduled end; cost = last price + %.1f¢ + taker fee; $%.0f a trade\n", 100**slip, *stake)
	fmt.Println("caveat: the busiest markets of the year over-represent upsets (an upset is what drives volume), which works against favorites")

	type pos struct {
		x       *mkt
		cost    float64
		won     bool
		entry   int64
		holdDay float64
		price   float64
	}
	horizons := []struct {
		name string
		d    time.Duration
	}{{"6 hours", 6 * time.Hour}, {"1 day", 24 * time.Hour}, {"3 days", 72 * time.Hour}, {"7 days", 168 * time.Hour}}
	thresholds := []float64{0.90, 0.95, 0.97}
	positions := func(th float64, h time.Duration) []pos {
		var out []pos
		for i := range mkts {
			x := &mkts[i]
			entry := x.m.End.Add(-h).Unix()
			if entry >= x.close {
				continue // already over by then
			}
			k := sort.Search(len(x.t), func(k int) bool { return x.t[k] > entry }) - 1
			if k < 0 || entry-x.t[k] > 86400 {
				continue // no price in the day before
			}
			p, won := x.p0[k], x.won0
			if p < 0.5 {
				p, won = 1-p, !won
			}
			if p < th || p >= 0.995 {
				continue // not a favorite, or nothing left to earn
			}
			cost := p + *slip + x.m.FeeRate*p*(1-p)
			if cost >= 1 {
				continue
			}
			out = append(out, pos{x: x, cost: cost, won: won, entry: entry, holdDay: math.Max(float64(x.close-entry), 3600) / 86400, price: p})
		}
		return out
	}
	pnl := func(q pos) float64 {
		if q.won {
			return *stake * (1/q.cost - 1)
		}
		return -*stake
	}

	fmt.Printf("\n%-8s %-8s %5s %8s %15s %9s %9s %10s %9s %10s\n", "price≥", "before", "n", "won", "[95%]", "needed", "avg/trade", "total P&L", "hold", "per year")
	for _, th := range thresholds {
		for _, h := range horizons {
			ps := positions(th, h.d)
			if len(ps) == 0 {
				continue
			}
			won, cost, tot, dollarDays := 0, 0.0, 0.0, 0.0
			for _, q := range ps {
				if q.won {
					won++
				}
				cost += q.cost
				tot += pnl(q)
				dollarDays += *stake * q.holdDay
			}
			n := float64(len(ps))
			lo, hi := wilson(won, len(ps))
			fmt.Printf("%-8.2f %-8s %5d %7.1f%% %6.1f–%5.1f%% %8.1f%% %8.2f%% %10.0f %7.1fd %9.0f%%\n", th, h.name, len(ps), 100*float64(won)/n,
				100*lo, 100*hi, 100*cost/n, 100*tot/(n**stake), tot, dollarDays/n / *stake, 100*tot/dollarDays*365)
		}
	}
	fmt.Println("needed = average cost, the win rate that breaks even · per year = profit per dollar-day of capital locked until resolution, ×365 (not compounded)")

	// The main configuration in detail.
	ps := positions(0.95, 24*time.Hour)
	fmt.Printf("\nDETAIL: price ≥ 0.95, one day before the scheduled end (%d trades)\n", len(ps))
	type agg struct {
		n, won int
		pnl    float64
	}
	bySector, byMonth := map[string]*agg{}, map[string]*agg{}
	add := func(m map[string]*agg, k string, q pos) {
		if m[k] == nil {
			m[k] = &agg{}
		}
		m[k].n++
		m[k].pnl += pnl(q)
		if q.won {
			m[k].won++
		}
	}
	for _, q := range ps {
		add(bySector, q.x.sector, q)
		add(byMonth, time.Unix(q.entry, 0).UTC().Format("2006-01"), q)
	}
	printAgg := func(title string, m map[string]*agg) {
		fmt.Println(title)
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			a := m[k]
			fmt.Printf("   %-10s %4d trades  %5.1f%% won  %+8.0f\n", k, a.n, 100*float64(a.won)/float64(a.n), a.pnl)
		}
	}
	printAgg("by month of entry:", byMonth)
	printAgg("by market type:", bySector)
	fmt.Println("the losses (question · favorite's price · what it cost):")
	sort.Slice(ps, func(a, b int) bool { return ps[a].entry < ps[b].entry })
	for _, q := range ps {
		if !q.won {
			fmt.Printf("   %s  %-70.70s %.3f  −$%.0f\n", time.Unix(q.entry, 0).UTC().Format("2006-01-02"), q.x.m.Question, q.price, *stake)
		}
	}
	return nil
}

var vsRe = regexp.MustCompile(`(?i)\bvs\.?\b`)

// loadMeta reads market metadata from a cache file, fetching what's missing.
func loadMeta(path string, ids []string) (map[string]source.PolyMeta, error) {
	meta := map[string]source.PolyMeta{}
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, &meta)
	}
	var missing []string
	for _, id := range ids {
		if _, ok := meta[id]; !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "fetching metadata for %d markets…\n", len(missing))
		got, err := source.PolyMetaByID(missing)
		if err != nil {
			return nil, err
		}
		for k, v := range got {
			meta[k] = v
		}
		if b, err := json.Marshal(meta); err == nil {
			os.WriteFile(path, b, 0o644)
		}
	}
	return meta, nil
}
