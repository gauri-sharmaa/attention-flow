package source

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Basket is an event whose markets are mutually exclusive: at most one
// resolves YES.
type Basket struct {
	Venue      string
	Event      string
	Slug       string    // Polymarket event slug or Kalshi event ticker
	End        time.Time // latest scheduled end among its markets
	Exhaustive bool      // one of the listed outcomes must happen
	Legs       []BasketLeg
}

// BasketLeg is one market in a basket with its top-of-book asks. Sizes from
// the event listings are unknown (0) until confirmed against the book.
type BasketLeg struct {
	Name      string
	ID        string // Kalshi ticker, or Polymarket condition ID
	YesToken  string // Polymarket only
	NoToken   string
	Yes, No   Quote
	FeeRate   float64
	Volume24h float64
}

// PolyBaskets lists open Polymarket "neg risk" events: multi-outcome events
// built so exactly one market resolves YES. Markets already closed inside an
// event (decided NO) are left out. In "augmented" events new outcomes can be
// added later, so the listed ones aren't known to be exhaustive.
func PolyBaskets() ([]Basket, error) {
	var out []Basket
	for off := 0; ; off += 100 {
		var page []struct {
			Title     string `json:"title"`
			Slug      string `json:"slug"`
			NegRisk   bool   `json:"negRisk"`
			Augmented bool   `json:"negRiskAugmented"`
			Markets   []struct {
				pmMarket
				Group     string  `json:"groupItemTitle"`
				BestBid   float64 `json:"bestBid"`
				BestAsk   float64 `json:"bestAsk"`
				Closed    bool    `json:"closed"`
				Accepting bool    `json:"acceptingOrders"`
			} `json:"markets"`
		}
		q := fmt.Sprintf("https://gamma-api.polymarket.com/events?closed=false&active=true&limit=100&offset=%d&order=volume24hr&ascending=false&cb=%d", off, time.Now().UnixNano())
		req, _ := http.NewRequest("GET", q, nil)
		req.Header.Set("User-Agent", userAgent)
		var err error
		for attempt := 0; attempt < 5; attempt++ {
			var resp *http.Response
			if resp, err = kalshiClient.Do(req); err == nil {
				err = json.NewDecoder(resp.Body).Decode(&page)
				resp.Body.Close()
				if err == nil {
					break
				}
			}
			time.Sleep(time.Duration(1<<attempt) * time.Second)
		}
		if err != nil {
			return out, err
		}
		for _, e := range page {
			if !e.NegRisk {
				continue
			}
			b := Basket{Venue: "polymarket", Event: e.Title, Slug: e.Slug, Exhaustive: !e.Augmented}
			for _, m := range e.Markets {
				if m.Closed {
					continue
				}
				var toks []string
				json.Unmarshal([]byte(m.ClobTokenIds), &toks)
				if len(toks) != 2 || !m.Accepting {
					b.Legs = nil // an open market we can't trade breaks the set
					break
				}
				end, _ := time.Parse(time.RFC3339, m.EndDate)
				if end.After(b.End) {
					b.End = end
				}
				l := BasketLeg{Name: m.Group, ID: m.ConditionID, YesToken: toks[0], NoToken: toks[1], Volume24h: m.Volume24hr,
					Yes: Quote{Price: m.BestAsk}}
				if m.BestBid > 0 {
					l.No = Quote{Price: 1 - m.BestBid}
				}
				if l.Name == "" {
					l.Name = m.Question
				}
				if m.FeesEnabled {
					l.FeeRate = m.FeeSchedule.Rate
				}
				b.Legs = append(b.Legs, l)
			}
			if len(b.Legs) >= 2 {
				out = append(out, b)
			}
		}
		if len(page) < 100 || off >= 5000 {
			return out, nil
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// PolyBook returns the best ask on both sides of a Polymarket leg.
func PolyBook(l *BasketLeg) error {
	yes, err := PolyAsk(l.YesToken)
	if err != nil {
		yes = Quote{}
	}
	no, err2 := PolyAsk(l.NoToken)
	if err2 != nil {
		no = Quote{}
	}
	l.Yes, l.No = yes, no
	if err != nil && err2 != nil {
		return err
	}
	return nil
}

// KalshiBaskets lists open Kalshi events marked mutually exclusive. Kalshi
// doesn't say whether the listed outcomes cover everything, so Exhaustive is
// false. Parlay series (KXMVE…) are skipped.
func KalshiBaskets() ([]Basket, error) {
	var out []Basket
	cursor := ""
	for {
		var page struct {
			Cursor string `json:"cursor"`
			Events []struct {
				Ticker    string `json:"event_ticker"`
				Title     string `json:"title"`
				Series    string `json:"series_ticker"`
				Exclusive bool   `json:"mutually_exclusive"`
				Markets   []struct {
					Ticker   string `json:"ticker"`
					YesSub   string `json:"yes_sub_title"`
					Status   string `json:"status"`
					Close    string `json:"close_time"`
					Expected string `json:"expected_expiration_time"`
					Vol24    string `json:"volume_24h_fp"`
					YesAsk   string `json:"yes_ask_dollars"`
					NoAsk    string `json:"no_ask_dollars"`
				} `json:"markets"`
			} `json:"events"`
		}
		q := "/events?limit=200&status=open&with_nested_markets=true"
		if cursor != "" {
			q += "&cursor=" + url.QueryEscape(cursor)
		}
		if err := kalshiGet(q, &page); err != nil {
			return out, err
		}
		for _, e := range page.Events {
			if !e.Exclusive || strings.HasPrefix(e.Series, "KXMVE") {
				continue
			}
			b := Basket{Venue: "kalshi", Event: e.Title, Slug: e.Ticker}
			for _, m := range e.Markets {
				if m.Status != "active" {
					continue
				}
				when := m.Expected
				if when == "" {
					when = m.Close
				}
				if t, err := time.Parse(time.RFC3339, when); err == nil && t.After(b.End) {
					b.End = t
				}
				b.Legs = append(b.Legs, BasketLeg{Name: m.YesSub, ID: m.Ticker, FeeRate: 0.07, Volume24h: num(m.Vol24),
					Yes: Quote{Price: num(m.YesAsk)}, No: Quote{Price: num(m.NoAsk)}})
			}
			if len(b.Legs) >= 2 {
				out = append(out, b)
			}
		}
		if page.Cursor == "" {
			return out, nil
		}
		cursor = page.Cursor
		time.Sleep(300 * time.Millisecond)
	}
}
