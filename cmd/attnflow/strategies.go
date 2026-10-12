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
)

// cmdStrategies runs three tests on a year of trades that go beyond
// "does activity predict activity":
//
//  1. Overreaction: when a market's trading spikes and its price jumps at
//     the same time, does the price give some of it back afterwards? Moves
//     of the same size on ordinary volume are the comparison.
//  2. Deadline ladders: "X by Oct 31" can never be likelier than "X by
//     Dec 31". How often, how much and how long do traded prices break
//     that, and what would buying the gap have locked in?
//  3. Calibration: in markets that resolved, how often did a side priced at
//     p a week (or a day, or a month) before the end actually win? Betting
//     markets usually overprice long shots.
func cmdStrategies(args []string) error {
	fs := flag.NewFlagSet("strategies", flag.ExitOnError)
	dir := fs.String("dir", "data/pmt365", "polytrades output (ticks.csv, universe.txt, cache/)")
	fs.Parse(args)
	streams, err := loadStreams(*dir)
	if err != nil {
		return err
	}
	ticks, t0, t1, err := loadSignedTicks(*dir, len(streams))
	if err != nil {
		return err
	}
	overreaction(ticks, t0, t1)
	ladders(streams, ticks)
	return calibration(filepath.Join(*dir, "cache"))
}

// ---- 1. overreaction ----

// overreaction works on 5-minute bins. A jump is a bin whose average trade
// price is ≥ 3¢ from the average over the previous 30 minutes. It is an
// attention spike if the bin has ≥ 10 trades and ≥ 5× the market's median
// trades per active bin over the previous day. Afterwards, the change from
// the jump bin's price to the average price 1, 6 and 24 hours later,
// signed so that positive = continued, negative = gave back.
func overreaction(ticks [][]sTick, t0, t1 float64) {
	const bin = 300.0
	horizons := []float64{3600, 6 * 3600, 24 * 3600}
	type res struct {
		sum, sum2 [3]float64
		n         [3]int
	}
	var spike, normal, busy res       // busy: ≥ 10 trades but no spike (same price noise as spikes)
	month := map[int]*[2][2]float64{} // month → [spike, normal] × [sum 6h, n]
	add := func(r *res, k int, v float64) { r.sum[k] += v; r.sum2[k] += v * v; r.n[k]++ }
	for _, tk := range ticks {
		if len(tk) < 200 {
			continue
		}
		nb := int((t1-t0)/bin) + 1
		cnt := make([]int, nb)
		sum := make([]float64, nb)
		for _, t := range tk {
			b := int((t.t - t0) / bin)
			cnt[b]++
			sum[b] += t.px
		}
		price := func(b int) (float64, bool) {
			if b < 0 || b >= nb || cnt[b] == 0 {
				return 0, false
			}
			return sum[b] / float64(cnt[b]), true
		}
		// Average price over bins [a, b) that had trades.
		avg := func(a, b int) (float64, bool) {
			s, n := 0.0, 0
			for i := max(a, 0); i < min(b, nb); i++ {
				s += sum[i]
				n += cnt[i]
			}
			if n == 0 {
				return 0, false
			}
			return s / float64(n), true
		}
		var active []int // trade counts of active bins in the last day (rolling)
		last := -1000
		for b := 0; b < nb; b++ {
			if b-288 >= 0 && cnt[b-288] > 0 {
				active = active[1:]
			}
			p, ok := price(b)
			if !ok {
				continue
			}
			med := median(active)
			active = append(active, cnt[b])
			before, ok := avg(b-6, b)
			if !ok || b-last < 12 { // one jump per hour per market
				continue
			}
			jump := p - before
			if math.Abs(jump) < 0.03 || p < 0.05 || p > 0.95 {
				continue
			}
			last = b
			isSpike := cnt[b] >= 10 && float64(cnt[b]) >= 5*math.Max(med, 1)
			r := &normal
			if isSpike {
				r = &spike
			}
			if !isSpike && cnt[b] >= 10 {
				for k, h := range horizons {
					hb := b + int(h/bin)
					if after, ok := avg(hb-1, hb+2); ok {
						add(&busy, k, (after-p)*math.Copysign(1, jump))
					}
				}
			}
			for k, h := range horizons {
				hb := b + int(h/bin)
				after, ok := avg(hb-1, hb+2)
				if !ok {
					continue
				}
				v := (after - p) * math.Copysign(1, jump)
				add(r, k, v)
				if k == 1 {
					mo := int((float64(b)*bin)/(30*86400)) + 1
					if month[mo] == nil {
						month[mo] = &[2][2]float64{}
					}
					si := 1
					if isSpike {
						si = 0
					}
					month[mo][si][0] += v
					month[mo][si][1]++
				}
			}
		}
	}
	fmt.Println("1. OVERREACTION: after a ≥3¢ jump, what happens next? (+ = kept going, − = gave back; ¢, average)")
	fmt.Println("   attention spike = jump on ≥10 trades and ≥5× the market's usual trades per 5 min")
	for _, row := range []struct {
		name string
		r    *res
	}{{"on an attention spike", &spike}, {"on normal volume", &normal}, {"  of which ≥10 trades", &busy}} {
		fmt.Printf("   %-22s", row.name)
		for k, h := range []string{"1 h", "6 h", "24 h"} {
			n := float64(row.r.n[k])
			m := row.r.sum[k] / n
			se := math.Sqrt((row.r.sum2[k]/n - m*m) / n)
			fmt.Printf("  %s %+5.2f¢ (±%.2f, n=%d)", h, 100*m, 200*se, row.r.n[k])
		}
		fmt.Println()
	}
	var mos []int
	for mo := range month {
		mos = append(mos, mo)
	}
	sort.Ints(mos)
	fmt.Print("   by month, 6 h, spike vs normal:")
	for _, mo := range mos {
		m := month[mo]
		if m[0][1] < 20 {
			continue
		}
		fmt.Printf("  %d: %+.1f/%+.1f", mo, 100*m[0][0]/m[0][1], 100*m[1][0]/math.Max(m[1][1], 1))
	}
	fmt.Println()
}

func median(xs []int) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]int(nil), xs...)
	sort.Ints(s)
	return float64(s[len(s)/2])
}

// ---- 2. deadline ladders ----

var deadlineRe = regexp.MustCompile(`(?i)\b(by|through|before)\s+(january|february|march|april|may|june|july|august|september|october|november|december)\s+(\d{1,2})(?:,?\s+(\d{4}))?`)

type rung struct {
	market int
	date   time.Time
	by     bool // "by/before" (likelier later) vs "through" (likelier earlier)
}

func ladders(streams []stream, ticks [][]sTick) {
	groups := map[string][]rung{}
	for i, s := range streams {
		m := deadlineRe.FindStringSubmatchIndex(s.name)
		if m == nil {
			continue
		}
		g := deadlineRe.FindStringSubmatch(s.name)
		year := 2026
		if g[4] != "" {
			fmt.Sscan(g[4], &year)
		}
		d, err := time.Parse("January 2 2006", fmt.Sprintf("%s %s %d", strings.Title(strings.ToLower(g[2])), g[3], year))
		if err != nil {
			continue
		}
		stem := strings.ToLower(s.name[:m[0]] + "<date>" + strings.TrimSuffix(strings.TrimSpace(s.name[m[1]:]), "?"))
		key := s.event + "|" + stem
		groups[key] = append(groups[key], rung{i, d, !strings.EqualFold(g[1], "through")})
	}
	type episode struct{ size, dur float64 }
	var eps []episode
	pairs, viol := 0, 0.0
	var totalTime float64
	for _, rs := range groups {
		sort.Slice(rs, func(a, b int) bool { return rs[a].date.Before(rs[b].date) })
		for k := 0; k+1 < len(rs); k++ {
			e, l := rs[k], rs[k+1]
			if e.by != l.by || e.date.Equal(l.date) {
				continue
			}
			// lo must be ≤ hi at all times.
			lo, hi := e.market, l.market
			if !e.by {
				lo, hi = l.market, e.market
			}
			pairs++
			v, tt := ladderGaps(ticks[lo], ticks[hi], func(size, dur float64) { eps = append(eps, episode{size, dur}) })
			viol += v
			totalTime += tt
		}
	}
	fmt.Println("\n2. DEADLINE LADDERS: \"by an earlier date\" priced above \"by a later date\" (impossible at resolution)")
	if pairs == 0 || totalTime == 0 {
		fmt.Println("   no ladder pairs found")
		return
	}
	sort.Slice(eps, func(a, b int) bool { return eps[a].size < eps[b].size })
	big := 0
	var durs []float64
	for _, e := range eps {
		if e.size > 0.02 {
			big++
		}
		durs = append(durs, e.dur)
	}
	sort.Float64s(durs)
	fmt.Printf("   %d adjacent ladder pairs · both traded in the last 10 min for %.0f pair-hours\n", pairs, totalTime/3600)
	fmt.Printf("   gap ≥ 3¢ on last trades %.2f%% of that time · %d separate gaps\n", 100*viol/totalTime, len(eps))
	if len(eps) > 0 {
		fmt.Printf("   median gap %.1f¢ (largest %.0f¢) · median length %s\n", 100*eps[len(eps)/2].size, 100*eps[len(eps)-1].size, lagStr(durs[len(durs)/2]))
		fmt.Printf("   %d of the gaps exceed 2¢ (1¢ a leg), but most last seconds and are measured on last trades, not quotes you could hit\n", big)
	}
	fmt.Println("   reading: ladders are kept in line; the live order book would show whether any gap is actually tradable")
}

// ladderGaps walks two markets' trades together. While both traded within
// the last 10 minutes, it measures time with lo's price ≥ 3¢ above hi's and
// reports each such episode's peak size and length.
func ladderGaps(lo, hi []sTick, ep func(size, dur float64)) (violTime, liveTime float64) {
	i, j := 0, 0
	plo, phi, tlo, thi := 0.0, 0.0, math.Inf(-1), math.Inf(-1)
	inGap, start, peak, prev := false, 0.0, 0.0, 0.0
	for i < len(lo) || j < len(hi) {
		var t float64
		if j >= len(hi) || (i < len(lo) && lo[i].t <= hi[j].t) {
			t, plo, tlo = lo[i].t, lo[i].px, lo[i].t
			i++
		} else {
			t, phi, thi = hi[j].t, hi[j].px, hi[j].t
			j++
		}
		live := t-tlo < 600 && t-thi < 600
		gap := live && plo-phi >= 0.03
		if live && prev > 0 && t-prev < 600 {
			liveTime += t - prev
			if inGap {
				violTime += t - prev
			}
		}
		switch {
		case gap && !inGap:
			inGap, start, peak = true, t, plo-phi
		case gap:
			peak = math.Max(peak, plo-phi)
		case inGap:
			inGap = false
			ep(peak, t-start)
		}
		if live {
			prev = t
		} else {
			prev = 0
		}
	}
	return
}

// ---- 3. calibration ----

func calibration(cacheDir string) error {
	files, err := filepath.Glob(filepath.Join(cacheDir, "*.json"))
	if err != nil {
		return err
	}
	now := float64(time.Now().Unix())
	horizons := []float64{86400, 7 * 86400, 30 * 86400}
	edges := []float64{0, 0.02, 0.05, 0.1, 0.2, 0.3, 0.4, 0.5}
	type cell struct {
		n, won int
		sumP   float64
	}
	cells := make([][]cell, len(horizons))
	for k := range cells {
		cells[k] = make([]cell, len(edges)-1)
	}
	resolved := 0
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var tr []struct {
			Asset     string  `json:"asset"`
			Price     float64 `json:"price"`
			Timestamp int64   `json:"timestamp"`
		}
		if json.Unmarshal(b, &tr) != nil || len(tr) < 50 {
			continue
		}
		sort.Slice(tr, func(a, c int) bool { return tr[a].Timestamp < tr[c].Timestamp })
		end := float64(tr[len(tr)-1].Timestamp)
		if end > now-3*86400 {
			continue // still trading: not resolved
		}
		// Price of asset A implied by any trade (a trade on the other side at p means 1-p).
		a := tr[0].Asset
		implied := func(k int) float64 {
			if tr[k].Asset == a {
				return tr[k].Price
			}
			return 1 - tr[k].Price
		}
		// Winner: A's implied price over the last hour of trading.
		s, n := 0.0, 0
		for k := len(tr) - 1; k >= 0 && float64(tr[k].Timestamp) > end-3600; k-- {
			s += implied(k)
			n++
		}
		final := s / float64(n)
		if final > 0.1 && final < 0.9 {
			continue // no clear winner in the data
		}
		resolved++
		aWon := final >= 0.9
		for h, span := range horizons {
			// Last trade at least `span` before the end.
			k := sort.Search(len(tr), func(k int) bool { return float64(tr[k].Timestamp) > end-span }) - 1
			if k < 0 {
				continue
			}
			p, won := implied(k), aWon
			if p > 0.5 { // look from the cheaper side: the long shot
				p, won = 1-p, !aWon
			}
			for c := 0; c+1 < len(edges); c++ {
				if p >= edges[c] && p < edges[c+1] {
					cells[h][c].n++
					cells[h][c].sumP += p
					if won {
						cells[h][c].won++
					}
					break
				}
			}
		}
	}
	fmt.Printf("\n3. CALIBRATION: %d resolved markets · the cheaper side's price before the end vs how often it won [95%% interval]\n", resolved)
	fmt.Println("   caveat: these are the year's highest-volume closed markets, and a long shot coming in is what drives volume, so surprises are over-represented")
	fmt.Printf("   %-10s", "price")
	for _, h := range []string{"1 day before", "1 week before", "1 month before"} {
		fmt.Printf("  %-34s", h)
	}
	fmt.Println()
	for c := 0; c+1 < len(edges); c++ {
		fmt.Printf("   %3.0f–%-3.0f%% ", 100*edges[c], 100*edges[c+1])
		for h := range horizons {
			x := cells[h][c]
			if x.n == 0 {
				fmt.Printf("  %-34s", "–")
				continue
			}
			lo, hi := wilson(x.won, x.n)
			fmt.Printf("  %-34s", fmt.Sprintf("%4.1f%% → won %4.1f%% [%.0f–%.0f] n=%d", 100*x.sumP/float64(x.n), 100*float64(x.won)/float64(x.n), 100*lo, 100*hi, x.n))
		}
		fmt.Println()
	}
	return nil
}

// wilson is the 95% Wilson score interval for k successes in n trials.
func wilson(k, n int) (float64, float64) {
	if n == 0 {
		return 0, 1
	}
	z, p, nn := 1.96, float64(k)/float64(n), float64(n)
	c := (p + z*z/(2*nn)) / (1 + z*z/nn)
	h := z * math.Sqrt(p*(1-p)/nn+z*z/(4*nn*nn)) / (1 + z*z/nn)
	return math.Max(0, c-h), math.Min(1, c+h)
}
