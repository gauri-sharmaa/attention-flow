package source

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Kalshi's public market data needs no account. Its trade records carry no
// trader identity.
const kalshiAPI = "https://api.elections.kalshi.com/trade-api/v2"

// userAgent identifies these research requests by the project, not a person.
const userAgent = "attention-flow/0.1 (research; https://github.com/gauri-sharmaa/attention-flow)"

// KalshiMarket is one open Kalshi market.
type KalshiMarket struct {
	Ticker    string
	Title     string // the question
	YesSub    string // what YES means, e.g. a team or candidate name
	Event     string // the parent event's title
	Category  string
	Close     time.Time // expected resolution (or close) time
	Volume    float64   // contracts traded, all time
	Volume24h float64
	YesBid    float64
	YesAsk    float64
}

var kalshiClient = &http.Client{Timeout: 60 * time.Second}

// kalshiGet fetches and decodes one API path, backing off on errors and on
// rate limits (honouring Retry-After) for up to a few minutes.
func kalshiGet(path string, v any) error {
	var err error
	for attempt := 0; attempt < 8; attempt++ {
		wait := time.Duration(min(60, 1<<attempt)) * time.Second
		var req *http.Request
		req, err = http.NewRequest("GET", kalshiAPI+path, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", userAgent)
		var resp *http.Response
		if resp, err = kalshiClient.Do(req); err == nil {
			if resp.StatusCode == http.StatusOK {
				err = json.NewDecoder(resp.Body).Decode(v)
			} else {
				err = fmt.Errorf("%s: %s", path, resp.Status)
				if s, e := strconv.Atoi(resp.Header.Get("Retry-After")); e == nil && s > 0 {
					wait = time.Duration(min(s, 120)) * time.Second
				}
			}
			resp.Body.Close()
			if err == nil {
				return nil
			}
		}
		time.Sleep(wait)
	}
	return err
}

func num(s string) float64 { v, _ := strconv.ParseFloat(s, 64); return v }

// KalshiOpenMarkets lists every open market with at least minVolume
// contracts traded. Auto-generated parlay series (KXMVE…) are skipped.
func KalshiOpenMarkets(minVolume float64) ([]KalshiMarket, error) {
	var out []KalshiMarket
	cursor := ""
	for {
		var page struct {
			Cursor string `json:"cursor"`
			Events []struct {
				Title    string `json:"title"`
				Category string `json:"category"`
				Series   string `json:"series_ticker"`
				Markets  []struct {
					Ticker   string `json:"ticker"`
					Title    string `json:"title"`
					YesSub   string `json:"yes_sub_title"`
					Expected string `json:"expected_expiration_time"`
					Close    string `json:"close_time"`
					Volume   string `json:"volume_fp"`
					Vol24    string `json:"volume_24h_fp"`
					YesBid   string `json:"yes_bid_dollars"`
					YesAsk   string `json:"yes_ask_dollars"`
				} `json:"markets"`
			} `json:"events"`
		}
		q := "/events?limit=200&status=open&with_nested_markets=true"
		if cursor != "" {
			q += "&cursor=" + url.QueryEscape(cursor)
		}
		if err := kalshiGet(q, &page); err != nil {
			return nil, err
		}
		for _, e := range page.Events {
			if strings.HasPrefix(e.Series, "KXMVE") {
				continue
			}
			for _, m := range e.Markets {
				if num(m.Volume) < minVolume {
					continue
				}
				when := m.Expected
				if when == "" {
					when = m.Close
				}
				t, _ := time.Parse(time.RFC3339, when)
				out = append(out, KalshiMarket{Ticker: m.Ticker, Title: m.Title, YesSub: m.YesSub, Event: e.Title,
					Category: e.Category, Close: t, Volume: num(m.Volume), Volume24h: num(m.Vol24), YesBid: num(m.YesBid), YesAsk: num(m.YesAsk)})
			}
		}
		if page.Cursor == "" {
			return out, nil
		}
		cursor = page.Cursor
		time.Sleep(300 * time.Millisecond)
	}
}

// Quote is the best price to buy at, and the size available there.
type Quote struct{ Price, Size float64 }

// KalshiAsks returns the best prices to buy YES and NO. Kalshi books hold
// only bids: buying YES at p means matching a NO bid at 1-p, and the reverse.
func KalshiAsks(ticker string) (yes, no Quote, err error) {
	var d struct {
		Book struct {
			Yes [][]string `json:"yes_dollars"`
			No  [][]string `json:"no_dollars"`
		} `json:"orderbook_fp"`
	}
	if err = kalshiGet("/markets/"+url.PathEscape(ticker)+"/orderbook", &d); err != nil {
		return
	}
	best := func(levels [][]string) (Quote, bool) {
		var q Quote
		ok := false
		for _, l := range levels {
			if len(l) == 2 && num(l[0]) > q.Price {
				q, ok = Quote{num(l[0]), num(l[1])}, true
			}
		}
		return q, ok
	}
	if nb, ok := best(d.Book.No); ok {
		yes = Quote{round4(1 - nb.Price), nb.Size}
	}
	if yb, ok := best(d.Book.Yes); ok {
		no = Quote{round4(1 - yb.Price), yb.Size}
	}
	return
}

func round4(x float64) float64 { return float64(int64(x*10000+0.5)) / 10000 }

// KalshiTrade is one Kalshi trade.
type KalshiTrade struct {
	T        float64 // unix seconds
	YesPrice float64
	Count    float64 // contracts
	TakerYes bool    // the taker bought YES
}

// KalshiTrades returns a market's trades in [from, to), oldest first.
func KalshiTrades(ticker string, from, to int64) ([]KalshiTrade, error) {
	out, err := kalshiTrades("/markets/trades", ticker, from, to)
	if err == nil && len(out) == 0 {
		// Older markets are archived: their trades are only under /historical.
		return kalshiTrades("/historical/trades", ticker, from, to)
	}
	return out, err
}

func kalshiTrades(endpoint, ticker string, from, to int64) ([]KalshiTrade, error) {
	var out []KalshiTrade
	cursor := ""
	for {
		var page struct {
			Cursor string `json:"cursor"`
			Trades []struct {
				Created string `json:"created_time"`
				Yes     string `json:"yes_price_dollars"`
				Count   string `json:"count_fp"`
				Taker   string `json:"taker_side"`
			} `json:"trades"`
		}
		q := fmt.Sprintf("%s?ticker=%s&limit=1000&min_ts=%d&max_ts=%d", endpoint, url.QueryEscape(ticker), from, to-1)
		if cursor != "" {
			q += "&cursor=" + url.QueryEscape(cursor)
		}
		if err := kalshiGet(q, &page); err != nil {
			return nil, err
		}
		for _, t := range page.Trades {
			ts, err := time.Parse(time.RFC3339Nano, t.Created)
			if err != nil {
				continue
			}
			out = append(out, KalshiTrade{T: float64(ts.UnixNano()) / 1e9, YesPrice: num(t.Yes), Count: num(t.Count), TakerYes: t.Taker == "yes"})
		}
		if page.Cursor == "" || len(page.Trades) == 0 {
			break
		}
		cursor = page.Cursor
		time.Sleep(150 * time.Millisecond)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 { // API is newest first
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// PolyAsk returns the best ask for a Polymarket outcome token.
func PolyAsk(token string) (Quote, error) {
	req, _ := http.NewRequest("GET", "https://clob.polymarket.com/book?token_id="+url.QueryEscape(token), nil)
	req.Header.Set("User-Agent", userAgent)
	resp, err := kalshiClient.Do(req)
	if err != nil {
		return Quote{}, err
	}
	defer resp.Body.Close()
	var d struct {
		Asks []struct{ Price, Size string } `json:"asks"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return Quote{}, err
	}
	q := Quote{Price: 2}
	for _, a := range d.Asks {
		if p := num(a.Price); p < q.Price {
			q = Quote{p, num(a.Size)}
		}
	}
	if q.Price > 1 {
		return Quote{}, fmt.Errorf("no asks")
	}
	return q, nil
}

// PolyMatchMarket is a Polymarket market with what cross-venue matching
// needs: its outcome names, their tokens and its taker fee rate.
type PolyMatchMarket struct {
	ConditionID string
	Question    string
	Event       string
	Outcomes    []string // e.g. ["Yes","No"] or two team names
	Tokens      []string // token for each outcome, same order
	End         time.Time
	GameStart   time.Time // for a single game: when it starts (zero otherwise)
	FeeRate     float64   // taker fee rate (0 if the market charges none)
	Volume24h   float64
}

// PolyMarketsForMatch returns the n busiest active Polymarket markets.
func PolyMarketsForMatch(n int) ([]PolyMatchMarket, error) {
	var out []PolyMatchMarket
	seen := map[string]bool{}
	for off := 0; off < n; off += 100 {
		var page []struct {
			pmMarket
			Outcomes  string `json:"outcomes"`
			GameStart string `json:"gameStartTime"`
		}
		u := fmt.Sprintf("https://gamma-api.polymarket.com/markets?active=true&closed=false&order=volume24hr&ascending=false&limit=100&offset=%d", off)
		req, _ := http.NewRequest("GET", u, nil)
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
			return nil, err
		}
		for _, m := range page {
			if seen[m.ConditionID] {
				continue
			}
			seen[m.ConditionID] = true
			var outs, toks []string
			json.Unmarshal([]byte(m.Outcomes), &outs)
			json.Unmarshal([]byte(m.ClobTokenIds), &toks)
			if len(outs) != 2 || len(toks) != 2 {
				continue
			}
			end, _ := time.Parse(time.RFC3339, m.EndDate)
			game, _ := time.Parse("2006-01-02 15:04:05-07", m.GameStart)
			ev := ""
			if len(m.Events) > 0 {
				ev = m.Events[0].Title
			}
			fee := 0.0
			if m.FeesEnabled {
				fee = m.FeeSchedule.Rate
			}
			out = append(out, PolyMatchMarket{ConditionID: m.ConditionID, Question: m.Question, Event: ev, Outcomes: outs, Tokens: toks,
				End: end, GameStart: game, FeeRate: fee, Volume24h: m.Volume24hr})
		}
		if len(page) < 100 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	return out, nil
}

// PolyMeta is the settled state of a Polymarket market.
type PolyMeta struct {
	ConditionID string
	Question    string
	Event       string
	End         time.Time // scheduled end date (known in advance)
	Closed      bool
	ClosedAt    time.Time // when trading stopped (zero if open or unknown)
	Outcomes    []string
	Tokens      []string
	Final       []float64 // settlement prices per outcome (1/0 once resolved)
	FeeRate     float64
	Volume24h   float64   // dollars traded in the last day
	Volume      float64   // dollars traded, all time (PolyClosedMarkets only)
	Start       time.Time // when it opened (PolyClosedMarkets only)
}

// PolyMetaByID looks up markets by condition ID, open or closed.
func PolyMetaByID(ids []string) (map[string]PolyMeta, error) {
	out := map[string]PolyMeta{}
	for i := 0; i < len(ids); i += 20 {
		batch := ids[i:min(i+20, len(ids))]
		q := url.Values{}
		for _, id := range batch {
			q.Add("condition_ids", id)
		}
		for _, closed := range []string{"false", "true"} {
			q.Set("closed", closed)
			var page []struct {
				pmMarket
				Outcomes string `json:"outcomes"`
				Closed   bool   `json:"closed"`
				ClosedAt string `json:"closedTime"`
			}
			req, _ := http.NewRequest("GET", "https://gamma-api.polymarket.com/markets?"+q.Encode(), nil)
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
				return nil, err
			}
			for _, m := range page {
				var outs, toks, prices []string
				json.Unmarshal([]byte(m.Outcomes), &outs)
				json.Unmarshal([]byte(m.ClobTokenIds), &toks)
				json.Unmarshal([]byte(m.OutcomePrices), &prices)
				end, _ := time.Parse(time.RFC3339, m.EndDate)
				pm := PolyMeta{ConditionID: m.ConditionID, Question: m.Question, End: end, Closed: m.Closed, Outcomes: outs, Tokens: toks, Volume24h: m.Volume24hr}
				for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05-07", "2006-01-02 15:04:05Z07:00"} {
					if t, err := time.Parse(layout, m.ClosedAt); err == nil {
						pm.ClosedAt = t
						break
					}
				}
				for _, p := range prices {
					pm.Final = append(pm.Final, num(p))
				}
				if len(m.Events) > 0 {
					pm.Event = m.Events[0].Title
				}
				if m.FeesEnabled {
					pm.FeeRate = m.FeeSchedule.Rate
				}
				out[m.ConditionID] = pm
			}
			time.Sleep(150 * time.Millisecond)
		}
	}
	return out, nil
}

// PolyClosedMarkets returns the closed markets whose scheduled end falls in
// [from, to), with how long each was open and its lifetime volume.
func PolyClosedMarkets(from, to time.Time) ([]PolyMeta, error) {
	var out []PolyMeta
	cursor := ""
	for {
		var resp struct {
			Next    string `json:"next_cursor"`
			Markets []struct {
				pmMarket
				Outcomes string `json:"outcomes"`
				ClosedAt string `json:"closedTime"`
				Start    string `json:"startDate"`
			} `json:"markets"`
		}
		// The keyset endpoint pages past the plain endpoint's offset limit.
		u := fmt.Sprintf("https://gamma-api.polymarket.com/markets/keyset?closed=true&limit=100&end_date_min=%s&end_date_max=%s",
			from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339))
		if cursor != "" {
			u += "&after_cursor=" + url.QueryEscape(cursor)
		}
		if err := getJSON(u, &resp); err != nil {
			return out, err
		}
		for _, m := range resp.Markets {
			var outs, toks, prices []string
			json.Unmarshal([]byte(m.Outcomes), &outs)
			json.Unmarshal([]byte(m.ClobTokenIds), &toks)
			json.Unmarshal([]byte(m.OutcomePrices), &prices)
			end, _ := time.Parse(time.RFC3339, m.EndDate)
			start, _ := time.Parse(time.RFC3339, m.Start)
			pm := PolyMeta{ConditionID: m.ConditionID, Question: m.Question, End: end, Start: start, Closed: true, Outcomes: outs, Tokens: toks, Volume: m.VolumeNum}
			for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05-07", "2006-01-02 15:04:05Z07:00"} {
				if t, err := time.Parse(layout, m.ClosedAt); err == nil {
					pm.ClosedAt = t
					break
				}
			}
			for _, p := range prices {
				pm.Final = append(pm.Final, num(p))
			}
			if len(m.Events) > 0 {
				pm.Event = m.Events[0].Title
			}
			if m.FeesEnabled {
				pm.FeeRate = m.FeeSchedule.Rate
			}
			out = append(out, pm)
		}
		if resp.Next == "" || len(resp.Markets) == 0 {
			return out, nil
		}
		cursor = resp.Next
		time.Sleep(100 * time.Millisecond)
	}
}

// PricePoint is one sample of a token's price history.
type PricePoint struct {
	T int64   `json:"t"`
	P float64 `json:"p"`
}

// PolyPriceHistory returns a token's hourly price over [from, to]. The API
// refuses windows much longer than two weeks at this resolution.
func PolyPriceHistory(token string, from, to time.Time) ([]PricePoint, error) {
	var d struct {
		History []PricePoint `json:"history"`
	}
	err := getJSON(fmt.Sprintf("https://clob.polymarket.com/prices-history?market=%s&startTs=%d&endTs=%d&fidelity=60",
		url.QueryEscape(token), from.Unix(), to.Unix()), &d)
	return d.History, err
}
