package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/gauri-sharmaa/attention-flow/internal/source"
)

// cmdSportsRec records, to the second, the order books of live and upcoming
// matches on Polymarket: the main match market and its side markets
// (handicaps, spreads, game/map/set winners). It places no orders. The
// recording answers the question the minute-level lag study couldn't: when
// the main market moves, can the same side's handicap still be bought at its
// old price a few seconds later, after paying the real bid-ask spread?
//
// Every 20 minutes the list of matches is refreshed (starting between -3h and
// +6h). Output in -dir: groups.json (match → markets), books.csv (every change
// of a market's best bid or ask) and trades.csv.
func cmdSportsRec(args []string) error {
	fs := flag.NewFlagSet("sportsrec", flag.ExitOnError)
	dir := fs.String("dir", "data/sportsrec", "output directory")
	dur := fs.Duration("for", 2*time.Hour, "how long to record")
	fs.Parse(args)
	os.MkdirAll(*dir, 0o755)
	books := openAppendFile(filepath.Join(*dir, "books.csv"), "t,market,bid,ask,bid_size,ask_size")
	trades := openAppendFile(filepath.Join(*dir, "trades.csv"), "t,market,yes_price,dir,shares")
	defer books.Close()
	defer trades.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *dur)
	defer cancel()

	var mu sync.Mutex
	type top struct{ bid, ask, bs, as float64 }
	tops := map[string]top{}
	bk := map[string]*simpleBook{}
	onBook := func(u source.BookUpdate) {
		mu.Lock()
		defer mu.Unlock()
		b := bk[u.ConditionID]
		if b == nil {
			b = &simpleBook{bids: map[float64]float64{}, asks: map[float64]float64{}}
			bk[u.ConditionID] = b
		}
		b.apply(u)
		bid, bs, ask, as := b.top()
		t := top{bid, ask, bs, as}
		if old, ok := tops[u.ConditionID]; ok && old == t {
			return
		}
		tops[u.ConditionID] = t
		fmt.Fprintf(books, "%.3f,%s,%.4f,%.4f,%.2f,%.2f\n", float64(time.Now().UnixMilli())/1000, u.ConditionID, bid, ask, bs, as)
	}
	onTrade := func(tr source.Trade) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(trades, "%.3f,%s,%.4f,%d,%.2f\n", float64(time.Now().UnixMilli())/1000, tr.ConditionID, tr.YesPrice, tr.Dir, tr.Shares)
	}

	groups := map[string]*matchGroup{}
	var streamCancel context.CancelFunc
	refresh := func() {
		found, err := source.PolyMatchGroups(-3*time.Hour, 6*time.Hour)
		if err != nil {
			fmt.Fprintln(os.Stderr, "refresh:", err)
			return
		}
		changed := false
		for _, g := range found {
			var keep []source.LiveMarket
			for _, m := range g.Markets {
				main := m.Question == g.Title || winOnRe.MatchString(m.Question)
				if !skipMarketRe.MatchString(m.Question) && (main || sideMarketRe.MatchString(m.Question)) {
					keep = append(keep, m)
				}
			}
			if len(keep) < 2 {
				continue
			}
			if old, ok := groups[g.Title]; !ok || len(old.Markets) != len(keep) {
				groups[g.Title] = &matchGroup{Title: g.Title, Start: g.Start, Markets: keep}
				changed = true
			}
		}
		if !changed && streamCancel != nil {
			return
		}
		b, _ := json.MarshalIndent(groups, "", " ")
		os.WriteFile(filepath.Join(*dir, "groups.json"), b, 0o644)
		live := map[string]source.LiveMarket{}
		for _, g := range groups {
			for _, m := range g.Markets {
				live[m.ConditionID] = m
			}
		}
		if streamCancel != nil {
			streamCancel()
		}
		var sctx context.Context
		sctx, streamCancel = context.WithCancel(ctx)
		go func() {
			for sctx.Err() == nil {
				source.Stream(sctx, live, onTrade, onBook)
				time.Sleep(2 * time.Second)
			}
		}()
		fmt.Fprintf(os.Stderr, "%s recording %d matches, %d markets\n", time.Now().UTC().Format("15:04"), len(groups), len(live))
	}
	refresh()
	tick := time.NewTicker(20 * time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			if streamCancel != nil {
				streamCancel()
			}
			return nil
		case <-tick.C:
			refresh()
		}
	}
}

type matchGroup struct {
	Title   string
	Start   time.Time
	Markets []source.LiveMarket
}

var (
	sideMarketRe = regexp.MustCompile(`(?i)handicap|spread|(game|map|set) \d winner|winner`)
	winOnRe      = regexp.MustCompile(`(?i)win on \d`)
	skipMarketRe = regexp.MustCompile(`(?i)o/u|over/under|both teams|exact score|toss|total|\b\d[hq]\b|team to score|odd/even|halftime|1h |2h `)
)

type simpleBook struct{ bids, asks map[float64]float64 }

func (b *simpleBook) apply(u source.BookUpdate) {
	if u.Snapshot {
		clear(b.bids)
		clear(b.asks)
	}
	for _, l := range u.Bids {
		if l.Size <= 0 {
			delete(b.bids, l.Price)
		} else {
			b.bids[l.Price] = l.Size
		}
	}
	for _, l := range u.Asks {
		if l.Size <= 0 {
			delete(b.asks, l.Price)
		} else {
			b.asks[l.Price] = l.Size
		}
	}
}

func (b *simpleBook) top() (bid, bidSize, ask, askSize float64) {
	bid, ask = 0, 1
	for p, s := range b.bids {
		if p > bid {
			bid, bidSize = p, s
		}
	}
	for p, s := range b.asks {
		if p < ask {
			ask, askSize = p, s
		}
	}
	return
}

func openAppendFile(path, header string) *os.File {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		panic(err)
	}
	if st, _ := f.Stat(); st != nil && st.Size() == 0 {
		fmt.Fprintln(f, header)
	}
	return f
}
