package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/gauri-sharmaa/attention-flow/internal/source"
)

// cmdWeather tests whether Kalshi's daily high-temperature markets can be
// beaten with the National Weather Service's own forecast.
//
// For each city and day, at two decision times (the afternoon before, and
// the morning of), it takes the latest National Blend of Models forecast
// issued at least two hours earlier, corrects it by the forecast's average
// miss over the previous weeks and turns it into a probability for every
// bracket (normal errors, spread from the same past misses). Both are
// walk-forward: only days before the one being traded are used. The market's
// probability is the bracket's last trade in the six hours before.
//
// Scored two ways: which probability is closer to what happened (Brier
// score), and a paper strategy that buys whichever side the forecast says
// is at least 5¢ cheap after paying a cent of spread and Kalshi's fee.
// Settlement is the official NWS Daily Climate Report Kalshi uses.
func cmdWeather(args []string) error {
	fs := flag.NewFlagSet("weather", flag.ExitOnError)
	days := fs.Int("days", 150, "days of history")
	dir := fs.String("dir", "data/weather", "cache")
	minEdge := fs.Float64("edge", 0.05, "buy when the forecast says a side is this cheap after costs")
	fs.Parse(args)
	os.MkdirAll(*dir, 0o755)
	cities := []struct{ name, series, station string }{
		{"New York", "KXHIGHNY", "KNYC"}, {"Chicago", "KXHIGHCHI", "KMDW"}, {"Miami", "KXHIGHMIA", "KMIA"},
		{"Austin", "KXHIGHAUS", "KAUS"}, {"Los Angeles", "KXHIGHLAX", "KLAX"}, {"Denver", "KXHIGHDEN", "KDEN"},
		{"Philadelphia", "KXHIGHPHIL", "KPHL"},
	}
	horizons := []struct {
		name   string
		offset time.Duration // decision time relative to 00Z on the day
	}{{"day before, 4pm ET", -4 * time.Hour}, {"morning of, 11am ET", 15 * time.Hour}}
	since := time.Now().UTC().AddDate(0, 0, -*days).Truncate(24 * time.Hour)

	type obs struct {
		city, date, hz string
		p, mkt         float64 // forecast and market probability of YES
		yes            bool
		mu, sigma      float64
	}
	var all []obs
	for _, c := range cities {
		cache := filepath.Join(*dir, c.station+".json")
		var data struct {
			Highs    map[string]float64
			NBM      []source.NBMHigh
			Brackets []source.KalshiBracket
			Prices   map[string]float64 // ticker|unix decision time → last YES price (−1: no trade)
		}
		if b, err := os.ReadFile(cache); err == nil {
			json.Unmarshal(b, &data)
		}
		if data.Prices == nil {
			data.Prices = map[string]float64{}
		}
		if len(data.Highs) == 0 {
			data.Highs = map[string]float64{}
			for y := since.Year(); y <= time.Now().Year(); y++ {
				h, err := source.ClimateHighs(c.station, y)
				if err != nil {
					return err
				}
				for k, v := range h {
					data.Highs[k] = v
				}
			}
			fmt.Fprintf(os.Stderr, "%s: %d climate reports\n", c.name, len(data.Highs))
		}
		if len(data.NBM) == 0 {
			for t := since.AddDate(0, 0, -50); t.Before(time.Now()); t = t.AddDate(0, 0, 10) {
				f, err := source.NBMHighs(c.station, t, t.AddDate(0, 0, 10))
				if err != nil {
					return err
				}
				data.NBM = append(data.NBM, f...)
			}
			fmt.Fprintf(os.Stderr, "%s: %d forecasts\n", c.name, len(data.NBM))
		}
		if len(data.Brackets) == 0 {
			br, err := source.KalshiSettledBrackets(c.series, since)
			if err != nil {
				return err // a partial list would silently shorten the test
			}
			data.Brackets = br
			fmt.Fprintf(os.Stderr, "%s: %d settled brackets\n", c.name, len(br))
		}
		save := func() {
			if b, err := json.Marshal(data); err == nil {
				os.WriteFile(cache, b, 0o644)
			}
		}
		save()

		// Forecast for a day as known at time t: the latest run ≥ 2h before t.
		byValid := map[int64][]source.NBMHigh{}
		for _, f := range data.NBM {
			byValid[f.Valid.Unix()] = append(byValid[f.Valid.Unix()], f)
		}
		forecast := func(date string, t time.Time) (float64, bool) {
			d, _ := time.Parse("2006-01-02", date)
			valid := d.AddDate(0, 0, 1)
			best, ok := NBMRun{}, false
			for _, f := range byValid[valid.Unix()] {
				if !f.Run.After(t.Add(-2*time.Hour)) && (!ok || f.Run.After(best.Run)) {
					best, ok = NBMRun{f.Run, f.High}, true
				}
			}
			return best.High, ok
		}
		byDate := map[string][]source.KalshiBracket{}
		for _, b := range data.Brackets {
			byDate[b.Date] = append(byDate[b.Date], b)
		}
		dates := make([]string, 0, len(byDate))
		for d := range byDate {
			dates = append(dates, d)
		}
		sort.Strings(dates)
		for _, hz := range horizons {
			// Walk-forward forecast misses for this horizon, from all days (not only traded ones).
			miss := func(before string) (mean, sd float64, n int) {
				var xs []float64
				b, _ := time.Parse("2006-01-02", before)
				first := 1
				if hz.offset < 0 {
					first = 2 // the day before, yesterday's high isn't reported yet
				}
				for k := first; k <= 45; k++ {
					d := b.AddDate(0, 0, -k)
					ds := d.Format("2006-01-02")
					h, ok := data.Highs[ds]
					if !ok {
						continue
					}
					f, ok := forecast(ds, d.Add(hz.offset))
					if !ok {
						continue
					}
					xs = append(xs, h-f)
				}
				if len(xs) < 15 {
					return 0, 0, len(xs)
				}
				for _, x := range xs {
					mean += x
				}
				mean /= float64(len(xs))
				for _, x := range xs {
					sd += (x - mean) * (x - mean)
				}
				return mean, math.Max(math.Sqrt(sd/float64(len(xs)-1)), 1), len(xs)
			}
			for _, date := range dates {
				d, _ := time.Parse("2006-01-02", date)
				t := d.Add(hz.offset)
				f, ok := forecast(date, t)
				if !ok {
					continue
				}
				bias, sigma, n := miss(date)
				if n < 15 {
					continue
				}
				mu := f + bias
				for _, b := range byDate[date] {
					key := fmt.Sprintf("%s|%d", b.Ticker, t.Unix())
					price, ok := data.Prices[key]
					if !ok {
						time.Sleep(120 * time.Millisecond) // stay under Kalshi's rate limit
						tr, err := source.KalshiTrades(b.Ticker, t.Add(-6*time.Hour).Unix(), t.Unix())
						if err != nil {
							continue
						}
						price = -1
						if len(tr) > 0 {
							price = tr[len(tr)-1].YesPrice
						}
						data.Prices[key] = price
					}
					if price <= 0 || price >= 1 {
						continue
					}
					p := normCDF((b.Hi+0.5-mu)/sigma) - normCDF((b.Lo-0.5-mu)/sigma)
					all = append(all, obs{c.name, date, hz.name, p, price, b.Yes, mu, sigma})
				}
			}
			save()
		}
	}

	fmt.Printf("WEATHER · Kalshi daily-high markets, 7 cities, last %d days · forecast = NWS National Blend of Models, bias-corrected walk-forward\n", *days)
	fmt.Println("Brier score (lower is better) per bracket, and a paper strategy buying the side the forecast says is ≥ 5¢ cheap (cost: last trade + 1¢ + Kalshi fee)")
	type agg struct {
		n, bets, won         int
		bModel, bMkt, bBlend float64
		pnl, staked          float64
		days                 map[string]bool
	}
	report := func(title string, keep func(obs) bool) {
		a := agg{days: map[string]bool{}}
		for _, o := range all {
			if !keep(o) {
				continue
			}
			y := 0.0
			if o.yes {
				y = 1
			}
			a.n++
			a.days[o.city+o.date] = true
			a.bModel += (o.p - y) * (o.p - y)
			a.bMkt += (o.mkt - y) * (o.mkt - y)
			bl := (o.p + o.mkt) / 2
			a.bBlend += (bl - y) * (bl - y)
			yesCost := math.Min(o.mkt+0.01, 0.99)
			yesCost += 0.07 * yesCost * (1 - yesCost)
			noCost := math.Min(1-o.mkt+0.01, 0.99)
			noCost += 0.07 * noCost * (1 - noCost)
			switch {
			case o.p-yesCost >= *minEdge:
				a.bets++
				a.staked += yesCost
				a.pnl += y - yesCost
				if o.yes {
					a.won++
				}
			case (1-o.p)-noCost >= *minEdge:
				a.bets++
				a.staked += noCost
				a.pnl += (1 - y) - noCost
				if !o.yes {
					a.won++
				}
			}
		}
		if a.n == 0 {
			return
		}
		n := float64(a.n)
		fmt.Printf("   %-22s %4d days %5d brackets  Brier forecast %.4f  market %.4f  average of both %.4f   bets %4d  won %5.1f%%  P&L %+7.2f$ per contract  (%+.1f%% of cost)\n",
			title, len(a.days), a.n, a.bModel/n, a.bMkt/n, a.bBlend/n, a.bets, 100*float64(a.won)/math.Max(float64(a.bets), 1), a.pnl, 100*a.pnl/math.Max(a.staked, 1e-9))
	}
	for _, hz := range horizons {
		fmt.Printf("\n%s\n", hz.name)
		report("all cities", func(o obs) bool { return o.hz == hz.name })
		for _, c := range cities {
			report(c.name, func(o obs) bool { return o.hz == hz.name && o.city == c.name })
		}
		months := map[string]bool{}
		for _, o := range all {
			months[o.date[:7]] = true
		}
		var ms []string
		for m := range months {
			ms = append(ms, m)
		}
		sort.Strings(ms)
		for _, m := range ms {
			report(m, func(o obs) bool { return o.hz == hz.name && o.date[:7] == m })
		}
	}
	return nil
}

// NBMRun is a forecast value and when it was issued.
type NBMRun struct {
	Run  time.Time
	High float64
}

func normCDF(z float64) float64 { return 0.5 * math.Erfc(-z/math.Sqrt2) }
