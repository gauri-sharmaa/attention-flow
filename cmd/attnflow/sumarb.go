package main

import (
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/gauri-sharmaa/attention-flow/internal/source"
	"github.com/gauri-sharmaa/attention-flow/internal/xvenue"
)

// cmdSumArb scans both venues for events whose outcomes can't both happen
// and checks whether buying one contract of every outcome costs less than
// its guaranteed payout (see xvenue.PriceBasket). The event listings' prices
// screen the candidates; anything close is re-priced from the live order
// books, which also give the size available.
func cmdSumArb(args []string) error {
	fs := flag.NewFlagSet("sumarb", flag.ExitOnError)
	every := fs.Duration("every", 0, "rescan this often (0: scan once)")
	dur := fs.Duration("for", time.Hour, "how long to keep rescanning")
	near := fs.Float64("near", 0.01, "re-price from the books when the listing's edge is above −this")
	out := fs.String("out", "data/sumarb", "where to log scans (scans.csv, hits.csv)")
	fs.Parse(args)
	os.MkdirAll(*out, 0o755)
	scans := openCSV(filepath.Join(*out, "scans.csv"), "t,venue,baskets,checked,hits,best_edge_no,best_edge_yes")
	hits := openCSV(filepath.Join(*out, "hits.csv"), "t,venue,event,side,legs,exhaustive,cost,payout,edge,sets,days")
	defer scans.Close()
	defer hits.Close()
	stop := time.Now().Add(*dur)
	for {
		for _, venue := range []string{"polymarket", "kalshi"} {
			if err := scanBaskets(venue, *near, scans, hits); err != nil {
				fmt.Fprintln(os.Stderr, venue+":", err)
			}
		}
		if *every == 0 || time.Now().Add(*every).After(stop) {
			return nil
		}
		time.Sleep(*every)
	}
}

func openCSV(path, header string) *os.File {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil
	}
	if st, _ := f.Stat(); st != nil && st.Size() == 0 {
		fmt.Fprintln(f, header)
	}
	return f
}

func scanBaskets(venue string, near float64, scans, hitLog *os.File) error {
	var baskets []source.Basket
	var err error
	if venue == "polymarket" {
		baskets, err = source.PolyBaskets()
	} else {
		baskets, err = source.KalshiBaskets()
	}
	if err != nil && len(baskets) == 0 {
		return err
	}
	now := time.Now()
	legs := func(b source.Basket) []xvenue.Leg {
		var ls []xvenue.Leg
		for _, l := range b.Legs {
			y, n := l.Yes, l.No
			if y.Size == 0 && y.Price > 0 {
				y.Size = math.Inf(1) // listing prices carry no size
			}
			if n.Size == 0 && n.Price > 0 {
				n.Size = math.Inf(1)
			}
			ls = append(ls, xvenue.Leg{Name: l.Name, Yes: y, No: n, FeeRate: l.FeeRate})
		}
		return ls
	}
	type hit struct {
		b    source.Basket
		q    xvenue.BasketQuote
		days float64
	}
	var found []hit
	var screenNo, screenYes []float64
	checked := 0
	bestNo, bestYes := math.Inf(-1), math.Inf(-1)
	for _, b := range baskets {
		no, yes, nok, yok := xvenue.PriceBasket(legs(b))
		if nok {
			screenNo = append(screenNo, no.Edge)
		}
		if yok && b.Exhaustive {
			screenYes = append(screenYes, yes.Edge)
		}
		if !(nok && no.Edge > -near) && !(yok && b.Exhaustive && yes.Edge > -near) {
			continue
		}
		// Re-price from the books.
		checked++
		ok := true
		for i := range b.Legs {
			l := &b.Legs[i]
			if venue == "polymarket" {
				ok = source.PolyBook(l) == nil && ok
			} else {
				y, n, err := source.KalshiAsks(l.ID)
				ok = err == nil && ok
				l.Yes, l.No = y, n
			}
		}
		if !ok {
			continue
		}
		no, yes, nok, yok = xvenue.PriceBasket(legs(b))
		days := math.Max(b.End.Sub(now).Hours()/24, 1)
		if nok {
			bestNo = math.Max(bestNo, no.Edge)
			if no.Edge > 0 {
				found = append(found, hit{b, no, days})
			}
		}
		if yok && b.Exhaustive {
			bestYes = math.Max(bestYes, yes.Edge)
			if yes.Edge > 0 {
				found = append(found, hit{b, yes, days})
			}
		}
	}
	sort.Slice(found, func(a, c int) bool { return found[a].q.Edge*found[a].q.Sets > found[c].q.Edge*found[c].q.Sets })
	fmt.Printf("\n%s %s · %d exclusive events · %d re-priced from the books · %d with a guaranteed profit after fees\n",
		now.Format("15:04"), venue, len(baskets), checked, len(found))
	fmt.Printf("   listing prices, edge per set: all-NO median %s (best %s) · all-YES (exhaustive events only) median %s (best %s)\n",
		cents(medianF(screenNo)), cents(maxOf(screenNo)), cents(medianF(screenYes)), cents(maxOf(screenYes)))
	for _, h := range found {
		fmt.Printf("   %-48.48s all-%-3s %2d legs  cost %.3f → pays %.0f  edge %5.2f¢/set × %6.0f sets = $%7.2f  %4.0fd  %6.1f%%/yr\n",
			h.b.Event, h.q.Side, len(h.b.Legs), h.q.Cost, h.q.Payout, 100*h.q.Edge, h.q.Sets, h.q.Edge*h.q.Sets, h.days,
			100*h.q.Edge/h.q.Cost*365/h.days)
		if hitLog != nil {
			fmt.Fprintf(hitLog, "%d,%s,%q,%s,%d,%t,%.4f,%.0f,%.4f,%.0f,%.1f\n", now.Unix(), venue, h.b.Event, h.q.Side, len(h.b.Legs),
				h.b.Exhaustive, h.q.Cost, h.q.Payout, h.q.Edge, h.q.Sets, h.days)
		}
	}
	if scans != nil {
		fmt.Fprintf(scans, "%d,%s,%d,%d,%d,%.4f,%.4f\n", now.Unix(), venue, len(baskets), checked, len(found), bestNo, bestYes)
	}
	return nil
}

func medianF(v []float64) float64 {
	if len(v) == 0 {
		return math.NaN()
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return s[len(s)/2]
}

func maxOf(v []float64) float64 {
	m := math.NaN()
	for _, x := range v {
		if math.IsNaN(m) || x > m {
			m = x
		}
	}
	return m
}

func cents(x float64) string {
	if math.IsNaN(x) {
		return "—"
	}
	return fmt.Sprintf("%+.1f¢", 100*x)
}
