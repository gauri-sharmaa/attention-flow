package main

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gauri-sharmaa/attention-flow/internal/leadlag"
	"github.com/gauri-sharmaa/attention-flow/internal/source"
	"github.com/gauri-sharmaa/attention-flow/internal/xvenue"
)

// cmdXVenue compares the same questions on Polymarket and Kalshi.
//
//	attnflow xvenue match              find pairs (writes pairs.json and a readable pairs.txt)
//	attnflow xvenue watch -for 24h     price both sides of every pair once a minute (gaps.csv)
//	attnflow xvenue summary            how often a gap beats both venues' fees, how big, how long
//	attnflow xvenue leadlag -days 30   which venue's price moves first, on long-running pairs
func cmdXVenue(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: attnflow xvenue match|watch|summary|leadlag [flags]")
	}
	fs := flag.NewFlagSet("xvenue", flag.ExitOnError)
	dir := fs.String("dir", "data/xvenue", "where pairs and gaps are kept")
	nPoly := fs.Int("poly", 2000, "busiest Polymarket markets to match")
	minVol := fs.Float64("minvol", 500, "minimum Kalshi contracts traded")
	every := fs.Duration("every", time.Minute, "watch: time between sweeps")
	dur := fs.Duration("for", 24*time.Hour, "watch: how long to run")
	rematch := fs.Duration("rematch", 2*time.Hour, "watch: refresh pairs this often (games end, new ones list)")
	days := fs.Int("days", 30, "leadlag: days of trades from each venue")
	minMoves := fs.Int("min", 20, "leadlag: minimum 2¢ moves on each venue")
	nNull := fs.Int("null", 99, "leadlag: whole-day shuffles for the p-value")
	fs.Parse(args[1:])
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		return err
	}
	switch args[0] {
	case "match":
		_, err := xvMatch(*dir, *nPoly, *minVol)
		return err
	case "watch":
		return xvWatch(*dir, *nPoly, *minVol, *every, *dur, *rematch)
	case "summary":
		return xvSummary(*dir)
	case "leadlag":
		return xvLeadLag(*dir, *days, *minMoves, *nNull)
	}
	return fmt.Errorf("unknown xvenue command %q", args[0])
}

func xvMatch(dir string, nPoly int, minVol float64) ([]xvenue.Pair, error) {
	km, err := source.KalshiOpenMarkets(minVol)
	if err != nil {
		return nil, fmt.Errorf("kalshi: %w", err)
	}
	pm, err := source.PolyMarketsForMatch(nPoly)
	if err != nil {
		return nil, fmt.Errorf("polymarket: %w", err)
	}
	pairs := xvenue.Match(pm, km)
	b, _ := json.MarshalIndent(pairs, "", " ")
	if err := os.WriteFile(filepath.Join(dir, "pairs.json"), b, 0o644); err != nil {
		return nil, err
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "# %d pairs from %d Kalshi and %d Polymarket markets, %s\n# rule | Polymarket question [outcome that equals Kalshi YES] | Kalshi question [YES means]\n",
		len(pairs), len(km), len(pm), time.Now().UTC().Format(time.RFC3339))
	for _, p := range pairs {
		fmt.Fprintf(&sb, "%-22s | %s [%s] | %s [%s]\n", p.Rule, p.Poly.Question, p.Poly.Outcomes[p.Same], p.Kalshi.Title, p.Kalshi.YesSub)
	}
	if err := os.WriteFile(filepath.Join(dir, "pairs.txt"), []byte(sb.String()), 0o644); err != nil {
		return nil, err
	}
	fmt.Fprintf(os.Stderr, "%d pairs from %d Kalshi and %d Polymarket markets (see %s/pairs.txt)\n", len(pairs), len(km), len(pm), dir)
	return pairs, nil
}

var gapHeader = []string{"t", "kalshi", "poly", "rule", "route", "kalshi_price", "poly_price", "gross", "net", "size", "resolves"}

func xvWatch(dir string, nPoly int, minVol float64, every, dur, rematch time.Duration) error {
	path := filepath.Join(dir, "gaps.csv")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	if st, _ := f.Stat(); st.Size() == 0 {
		w.Write(gapHeader)
	}
	var pairs []xvenue.Pair
	var matched time.Time
	stop := time.Now().Add(dur)
	for sweep := 1; time.Now().Before(stop); sweep++ {
		if pairs == nil || time.Since(matched) > rematch {
			if p, err := xvMatch(dir, nPoly, minVol); err == nil {
				pairs, matched = p, time.Now()
			} else if pairs == nil {
				return err
			} else {
				fmt.Fprintln(os.Stderr, "rematch failed, keeping old pairs:", err)
			}
		}
		start := time.Now()
		pos, best := 0, math.Inf(-1)
		for _, p := range pairs {
			resolves := p.Kalshi.Close
			if p.Poly.End.After(resolves) {
				resolves = p.Poly.End // the later of the two: money is tied up until both settle
			}
			if resolves.Before(start) {
				continue
			}
			routes, err := xvenue.Quote(p)
			if err != nil {
				continue
			}
			now := time.Now().UTC()
			for _, r := range routes {
				w.Write([]string{now.Format(time.RFC3339), p.Kalshi.Ticker, p.Poly.ConditionID, p.Rule, r.Name,
					ff(r.Kalshi), ff(r.Poly), ff(r.Gross), ff(r.Net), ff(r.Size), resolves.UTC().Format(time.RFC3339)})
				if r.Net > 0 {
					pos++
				}
				best = math.Max(best, r.Net)
			}
			time.Sleep(100 * time.Millisecond)
		}
		w.Flush()
		fmt.Fprintf(os.Stderr, "sweep %d · %d pairs in %s · %d routes beat both fees · best %+.1f¢\n",
			sweep, len(pairs), time.Since(start).Round(time.Second), pos, 100*best)
		if wait := every - time.Since(start); wait > 0 {
			time.Sleep(wait)
		}
	}
	return w.Error()
}

func ff(x float64) string { return strconv.FormatFloat(x, 'f', 4, 64) }

// xvSummary reads gaps.csv: per pair and route, how often the net gap was
// positive, its size and length, and the yearly return given lock-up.
func xvSummary(dir string) error {
	f, err := os.Open(filepath.Join(dir, "gaps.csv"))
	if err != nil {
		return err
	}
	defer f.Close()
	r := csv.NewReader(bufio.NewReader(f))
	r.FieldsPerRecord = -1 // the watcher may be mid-write: skip a partial last line
	all, err := r.ReadAll()
	if err != nil {
		return err
	}
	var rows [][]string
	for _, row := range all {
		if len(row) == len(gapHeader) {
			rows = append(rows, row)
		}
	}
	if len(rows) == 0 {
		return fmt.Errorf("no complete rows yet")
	}
	type obs struct {
		t, net, gross, size, cost float64
		resolves                  time.Time
	}
	type key struct{ kalshi, poly, rule, route string }
	series := map[key][]obs{}
	sweeps := map[string]bool{}
	for _, row := range rows[1:] {
		t, _ := time.Parse(time.RFC3339, row[0])
		res, _ := time.Parse(time.RFC3339, row[10])
		num := func(i int) float64 { v, _ := strconv.ParseFloat(row[i], 64); return v }
		k := key{row[1], row[2], row[3], row[4]}
		series[k] = append(series[k], obs{float64(t.Unix()), num(8), num(7), num(9), num(5) + num(6), res})
		sweeps[row[0][:16]] = true
	}
	var total, posObs, episodes int
	var durs, sizes, annual []float64
	byRule := map[string][2]int{}
	type top struct {
		k            key
		net, ann, sz float64
	}
	var tops []top
	for k, s := range series {
		sort.Slice(s, func(a, b int) bool { return s[a].t < s[b].t })
		in := false
		var start float64
		for i, o := range s {
			total++
			c := byRule[k.rule]
			c[1]++
			if o.net > 0 {
				posObs++
				c[0]++
				sizes = append(sizes, o.size)
				days := math.Max(1, o.resolves.Sub(time.Unix(int64(o.t), 0)).Hours()/24)
				ann := o.net / o.cost * 365 / days
				annual = append(annual, ann)
				tops = append(tops, top{k, o.net, ann, o.size})
				if !in {
					in, start = true, o.t
					episodes++
				}
			}
			if in && (o.net <= 0 || i == len(s)-1) {
				end := o.t
				if o.net > 0 {
					end += 60
				}
				durs = append(durs, end-start)
				in = false
			}
			byRule[k.rule] = c
		}
	}
	fmt.Printf("cross-venue gaps · %d sweeps · %d pair-routes · %d quotes\n", len(sweeps), len(series), total)
	if total == 0 {
		return nil
	}
	fmt.Printf("  beat both venues' taker fees: %d of %d quotes (%.2f%%) in %d separate stretches\n", posObs, total, 100*float64(posObs)/float64(total), episodes)
	if len(durs) > 0 {
		sort.Float64s(durs)
		sort.Float64s(sizes)
		sort.Float64s(annual)
		fmt.Printf("  median stretch %s (longest %s) · median size %.0f contracts · median yearly return %.1f%% given lock-up until resolution\n",
			lagStr(durs[len(durs)/2]), lagStr(durs[len(durs)-1]), sizes[len(sizes)/2], 100*annual[len(annual)/2])
	}
	fmt.Println("  by pair type (quotes beating fees / quotes):")
	var rules []string
	for r := range byRule {
		rules = append(rules, r)
	}
	sort.Strings(rules)
	for _, r := range rules {
		c := byRule[r]
		fmt.Printf("    %-22s %5d / %-6d (%.2f%%)\n", r, c[0], c[1], 100*float64(c[0])/float64(c[1]))
	}
	sort.Slice(tops, func(a, b int) bool { return tops[a].ann > tops[b].ann })
	fmt.Println("  best by yearly return:")
	seen := map[key]bool{}
	for _, t := range tops {
		if seen[t.k] || len(seen) == 10 {
			continue
		}
		seen[t.k] = true
		fmt.Printf("    %+5.1f¢ net · %6.1f%%/yr · %7.0f contracts · %s · %s\n", 100*t.net, 100*t.ann, t.sz, t.k.kalshi, t.k.route)
	}
	return nil
}

// xvLeadLag asks, for pairs that trade for weeks, which venue's price moves
// first: the same HRY estimator and whole-day shuffle test as the
// within-Polymarket lead-lag, on both venues' trades. Positive lag means
// Polymarket leads.
func xvLeadLag(dir string, days, minMoves, nNull int) error {
	b, err := os.ReadFile(filepath.Join(dir, "pairs.json"))
	if err != nil {
		return fmt.Errorf("run xvenue match first: %w", err)
	}
	var pairs []xvenue.Pair
	if err := json.Unmarshal(b, &pairs); err != nil {
		return err
	}
	to := time.Now().Unix()
	from := to - int64(days)*86400
	rng := rand.New(rand.NewPCG(71, 72))
	type row struct {
		q      string
		r      leadlag.Result
		mp, mk int
	}
	var rows []row
	skipped := 0
	for _, p := range pairs {
		if p.Kalshi.Close.Before(time.Now().Add(7 * 24 * time.Hour)) {
			continue // games and soon-to-close markets: too short for whole-day shuffles
		}
		lm := source.LiveMarket{ConditionID: p.Poly.ConditionID, YesToken: p.Poly.Tokens[p.Same], NoToken: p.Poly.Tokens[1-p.Same]}
		pt, err := source.History(lm, from)
		if err != nil {
			skipped++
			continue
		}
		kt, err := source.KalshiTrades(p.Kalshi.Ticker, from, to)
		if err != nil {
			skipped++
			continue
		}
		var ta, pa, tb, pb []float64
		for _, t := range pt {
			ta, pa = append(ta, float64(t.T)), append(pa, t.YesPrice)
		}
		for _, t := range kt {
			tb, pb = append(tb, t.T), append(pb, t.YesPrice)
		}
		x, y := moveSeries(ta, pa), moveSeries(tb, pb)
		f, e := float64(from), float64(to)
		mp, mk := moves(x, f, e), moves(y, f, e)
		if mp < minMoves || mk < minMoves {
			rows = append(rows, row{q: p.Poly.Question, mp: mp, mk: mk, r: leadlag.Result{P: -1}})
			continue
		}
		rows = append(rows, row{p.Poly.Question, leadlag.Estimate(x, y, f, e, f, e, nNull, rng), mp, mk})
	}
	tested, sig, polyLeads, kalLeads, together := 0, 0, 0, 0, 0
	var lags []float64
	fmt.Printf("which venue moves first · %d days of trades · positive lag = Polymarket leads · %d shuffles\n", days, nNull)
	sort.Slice(rows, func(a, b int) bool { return rows[a].r.P < rows[b].r.P })
	for _, r := range rows {
		if r.r.P < 0 {
			continue
		}
		tested++
		mark := ""
		if r.r.P <= 0.01 {
			sig++
			mark = " *"
			switch {
			case r.r.Lag > 0:
				polyLeads++
				lags = append(lags, r.r.Lag)
			case r.r.Lag < 0:
				kalLeads++
				lags = append(lags, -r.r.Lag)
			default:
				together++
			}
		}
		fmt.Printf("  p=%.2f lag %+7.0fs rho %+.2f · moves P %4d K %4d · %s%s\n", r.r.P, r.r.Lag, r.r.Rho, r.mp, r.mk, clip(r.q, 70), mark)
	}
	sort.Float64s(lags)
	med := 0.0
	if len(lags) > 0 {
		med = lags[len(lags)/2]
	}
	fmt.Printf("\n%d long-running pairs with ≥ %d moves on both venues (%d too quiet, %d failed to load)\n", tested, minMoves, len(rows)-tested, skipped)
	fmt.Printf("significant at 1%%: %d (chance ≈ %.1f) · Polymarket first %d · Kalshi first %d · same time %d · median lead %s\n",
		sig, 0.01*float64(tested), polyLeads, kalLeads, together, lagStr(med))
	return nil
}
