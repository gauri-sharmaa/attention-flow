package live

import (
	"math"
	"sort"
	"time"
)

// Snapshot is what the dashboard draws.
type Snapshot struct {
	Now     int64     `json:"now"`
	Status  Status    `json:"status"`
	Markets []MarketV `json:"markets"`
	Hot     []HotRow  `json:"hot"`
	Links   []LinkV   `json:"links"`
	Tape    []TapeRow `json:"tape"`
	Horizon float64   `json:"horizon"`
}

// MarketV is the static description plus live state of one market.
type MarketV struct {
	Q      string  `json:"q"`
	Sector string  `json:"s"`
	Event  string  `json:"e"`
	Price  float64 `json:"p"`
	Recent int     `json:"r"` // activity events in the last 5 minutes
}

// HotRow is a market whose trading is running hot right now.
type HotRow struct {
	Market  int     `json:"m"`
	Heat    float64 `json:"heat"`  // current trading rate / normal rate for this hour
	PMove   float64 `json:"pmove"` // probability of a price move within the horizon
	PNormal float64 `json:"pnorm"` // ... at a normal moment
	Drivers []int   `json:"drivers"`
	Recent  int     `json:"recent"`
}

// LinkV is a fitted cross-market link.
type LinkV struct {
	From   int     `json:"f"`
	To     int     `json:"t"`
	Branch float64 `json:"b"`
	Lag    float64 `json:"lag"`
}

// Snapshot computes the current view.
func (e *Engine) Snapshot() *Snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := float64(time.Now().Unix())
	s := &Snapshot{Now: int64(now), Status: e.status, Tape: append([]TapeRow{}, e.tape...), Horizon: e.cfg.Horizon,
		Hot: []HotRow{}, Links: []LinkV{}}
	n := len(e.markets)
	for _, m := range e.markets {
		k := 0
		for k < len(m.recent) && m.recent[k] < now-3600 {
			k++
		}
		m.recent = m.recent[k:]
		r := 0
		for _, t := range m.recent {
			if t >= now-300 {
				r++
			}
		}
		s.Markets = append(s.Markets, MarketV{Q: m.Question, Sector: m.Sector, Event: m.Event, Price: round(m.price, 3), Recent: r})
	}
	if e.cross == nil || e.moves == nil {
		return s
	}
	for i := range e.markets {
		// "Normal" is the market's own average over the window, so a market
		// that rarely trades isn't hot just because one trade came in.
		norm := e.normAct[i]
		if norm <= 0 {
			continue
		}
		heat := e.cross.Rate(i, now) / norm
		exp := e.moves.Expected(n+i, now, e.cfg.Horizon)
		pn := 1 - math.Exp(-e.normMov[i]*e.cfg.Horizon)
		row := HotRow{Market: i, Heat: round(heat, 1), PMove: round(1-math.Exp(-exp), 3),
			PNormal: round(pn, 3), Recent: s.Markets[i].Recent, Drivers: []int{}}
		for _, c := range e.cross.Contributions(i, now) {
			if c.From != i && len(row.Drivers) < 2 && c.Rate > 0.1*norm {
				row.Drivers = append(row.Drivers, c.From)
			}
		}
		if heat > 2 && row.Recent >= 3 && row.PMove > row.PNormal {
			s.Hot = append(s.Hot, row)
		}
	}
	sort.Slice(s.Hot, func(a, b int) bool { return s.Hot[a].PMove-s.Hot[a].PNormal > s.Hot[b].PMove-s.Hot[b].PNormal })
	if len(s.Hot) > 12 {
		s.Hot = s.Hot[:12]
	}
	for _, ed := range e.cross.M.Edges(0.05) {
		if e.markets[ed.From].Event != e.markets[ed.To].Event {
			s.Links = append(s.Links, LinkV{From: ed.From, To: ed.To, Branch: round(ed.Branch, 3), Lag: math.Round(ed.MeanLag)})
		}
	}
	return s
}
