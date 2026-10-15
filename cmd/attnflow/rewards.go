package main

import (
	"flag"
	"fmt"
	"math"
	"regexp"
	"sort"

	"github.com/gauri-sharmaa/attention-flow/internal/rewards"
	"github.com/gauri-sharmaa/attention-flow/internal/source"
)

// cmdRewards estimates, from current order books, what the paper market
// maker would collect in Polymarket liquidity rewards by resting the minimum
// qualifying size on both sides at the best prices inside the band, and
// what that costs in adverse selection.
//
// Fills: of each day's volume, the share our order would be on the other
// side of (our size over everything resting at our price, plus ours). Loss
// per filled share: what the live paper market maker measured for the same
// kind of market when quoting both sides, either the 10-minute markout or
// the cost of also getting out as a taker an hour later (below).
//
// Only markets where the rewards beat the losses would be quoted, so the
// totals count those; picking them from one snapshot flatters the result.
var mmLoss = map[string][2]float64{ // $ per filled share: 10-minute markout, exit as taker
	"sports": {0.0209, 0.0261}, "econ": {0.0089, 0.0355}, "world": {0.0071, 0.0158}, "other": {0.0052, 0.0275},
	"tech": {0.0016, 0.0217}, "crypto": {0, 0.0214}, "politics": {0, 0.0132}, "culture": {0, 0.0051},
}

var gameRe = regexp.MustCompile(`(?i)\bvs\.?\b|\bgame \d|o/u|handicap|spread:|total kills|map \d`)

func cmdRewards(args []string) error {
	fs := flag.NewFlagSet("rewards", flag.ExitOnError)
	top := fs.Int("top", 150, "markets with the biggest reward pools to check")
	fs.Parse(args)
	rms, err := source.PolyRewardMarkets()
	if err != nil && len(rms) == 0 {
		return err
	}
	pool := 0.0
	for _, r := range rms {
		pool += r.Daily
	}
	nReward := len(rms)
	sort.Slice(rms, func(a, b int) bool { return rms[a].Daily > rms[b].Daily })
	if len(rms) > *top {
		rms = rms[:*top]
	}
	ids := make([]string, len(rms))
	for i, r := range rms {
		ids[i] = r.ConditionID
	}
	meta, err := source.PolyMetaByID(ids)
	if err != nil {
		return err
	}
	type row struct {
		q, sector                          string
		lossMid, lossExit                  float64
		daily, share, earn, fills, capital float64
		mid, v, size                       float64
	}
	var rows []row
	for _, r := range rms {
		m, ok := meta[r.ConditionID]
		if !ok || m.Closed || len(m.Tokens) != 2 {
			continue
		}
		bids, asks, err := source.PolyBookLevels(m.Tokens[0])
		if err != nil || len(bids) == 0 || len(asks) == 0 {
			continue
		}
		bb, ba := bids[0], asks[0]
		for _, l := range bids {
			if l.Price > bb.Price {
				bb = l
			}
		}
		for _, l := range asks {
			if l.Price < ba.Price {
				ba = l
			}
		}
		mid := (bb.Price + ba.Price) / 2
		if mid < 0.02 || mid > 0.98 {
			continue
		}
		v, size := r.MaxSpread, r.MinSize
		// Our quotes: the best prices, pulled inside the band if they're outside it.
		bid := math.Max(bb.Price, mid-(v-0.5)/100)
		ask := math.Min(ba.Price, mid+(v-0.5)/100)
		bid, ask = math.Floor(bid*100+1e-9)/100, math.Ceil(ask*100-1e-9)/100
		ourB := rewards.Score(v, 100*(mid-bid), size)
		ourA := rewards.Score(v, 100*(ask-mid), size)
		ours := rewards.Credit(ourB, ourA, mid)
		ob, oa := rewards.BookScores(bids, asks, mid, v, size)
		theirs := rewards.Credit(ob, oa, mid)
		if ours <= 0 {
			continue
		}
		share := ours / (ours + theirs)
		ahead := func(levels []source.Level, p float64) float64 {
			s := 0.0
			for _, l := range levels {
				if math.Abs(l.Price-p) < 1e-9 {
					s += l.Size
				}
			}
			return s
		}
		shares := m.Volume24h / mid / 2 // a day's shares traded on each side, roughly
		fills := shares*size/(ahead(bids, bid)+size) + shares*size/(ahead(asks, ask)+size)
		sector := source.SectorOf(m.Question + " " + m.Event)
		if gameRe.MatchString(m.Question) {
			sector = "sports"
		}
		loss := mmLoss[sector]
		rows = append(rows, row{m.Question, sector, loss[0], loss[1], r.Daily, share, share * r.Daily, fills, size*bid + size*(1-ask), mid, v, size})
	}
	sort.Slice(rows, func(a, b int) bool {
		return rows[a].earn-rows[a].fills*rows[a].lossMid > rows[b].earn-rows[b].fills*rows[b].lossMid
	})
	fmt.Printf("LIQUIDITY REWARDS · %d markets pay rewards, $%.0f a day in all · checked the %d biggest pools against their current books\n", nReward, pool, len(rows))
	fmt.Println("quote the minimum size on both sides at the best prices inside the band · share = our score over ours + everyone else's (conservative)")
	fmt.Printf("\n%-50s %-8s %6s %5s %5s %7s %8s %8s %7s %9s %9s %7s\n", "market", "type", "pool/d", "band", "size", "share", "rewards", "fills/d", "¢/fill", "net", "net(exit)", "capital")
	var keep, keepX, alone struct {
		n                  int
		earn, net, capital float64
	}
	for i, r := range rows {
		net, netX := r.earn-r.fills*r.lossMid, r.earn-r.fills*r.lossExit
		if net > 0 && r.share > 0.99 {
			alone.n++
			alone.earn += r.earn
			alone.net += net
			alone.capital += r.capital
		}
		if net > 0 {
			keep.n++
			keep.earn += r.earn
			keep.net += net
			keep.capital += r.capital
		}
		if netX > 0 {
			keepX.n++
			keepX.earn += r.earn
			keepX.net += netX
			keepX.capital += r.capital
		}
		perFill := math.Inf(1)
		if r.fills > 0 {
			perFill = 100 * r.earn / r.fills
		}
		if i < 25 || i >= len(rows)-5 {
			if i == len(rows)-5 && i > 25 {
				fmt.Println("   … the worst five:")
			}
			fmt.Printf("%-50.50s %-8s %6.0f %4.1f¢ %5.0f %6.1f%% %8.2f %8.0f %7.2f %+9.2f %+9.2f %7.0f\n", r.q, r.sector, r.daily, r.v, r.size, 100*r.share, r.earn, r.fills, perFill, net, netX, r.capital)
		}
	}
	fmt.Println("¢/fill = rewards per share filled; quoting pays only if it beats the loss per fill for that type of market")
	for _, k := range []struct {
		name string
		v    struct {
			n                  int
			earn, net, capital float64
		}
	}{{"losing the 10-minute markout on each fill", keep}, {"  …of which no one else qualified at that moment", alone}, {"exiting every fill as a taker", keepX}} {
		fmt.Printf("worth quoting if %s: %d markets · rewards $%.0f/day · net $%.0f/day · capital $%.0f", k.name, k.v.n, k.v.earn, k.v.net, k.v.capital)
		if k.v.capital > 0 {
			fmt.Printf(" (net $%.2f per $1 of capital a day)", k.v.net/k.v.capital)
		}
		fmt.Println()
	}
	fmt.Println("caveats: one snapshot, and the markets were picked from it; others re-quote when we join; fills are estimated from volume, not observed;")
	fmt.Println("rewards need the quotes up around the clock; a 100% share means no one else qualified at that moment, which rarely lasts")
	return nil
}
