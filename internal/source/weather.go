package source

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Weather markets settle on the National Weather Service's Daily Climate
// Report for one station. The Iowa Environmental Mesonet (IEM) archives
// both those reports and the NWS's own forecasts as they were issued, so a
// backtest can use exactly what a trader could have seen at the time.

const iem = "https://mesonet.agron.iastate.edu"

func iemGet(u string) (*http.Response, error) {
	var resp *http.Response
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		req, _ := http.NewRequest("GET", u, nil)
		req.Header.Set("User-Agent", userAgent)
		if resp, err = kalshiClient.Do(req); err == nil && resp.StatusCode == 200 {
			return resp, nil
		}
		if err == nil {
			resp.Body.Close()
			err = fmt.Errorf("%s: %s", u, resp.Status)
		}
		time.Sleep(time.Duration(2<<attempt) * time.Second)
	}
	return nil, err
}

// ClimateHighs returns a station's official daily high (°F) by local date
// ("2006-01-02") for one year, from the NWS Daily Climate Reports.
func ClimateHighs(station string, year int) (map[string]float64, error) {
	resp, err := iemGet(fmt.Sprintf("%s/json/cli.py?station=%s&year=%d", iem, url.QueryEscape(station), year))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var d struct {
		Results []struct {
			Valid string `json:"valid"`
			High  any    `json:"high"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return nil, err
	}
	out := map[string]float64{}
	for _, r := range d.Results {
		if h, ok := r.High.(float64); ok {
			out[r.Valid] = h
		}
	}
	return out, nil
}

// NBMHigh is one National Blend of Models forecast of a day's high.
type NBMHigh struct {
	Run   time.Time // model run (UTC); out about an hour later
	Valid time.Time // end of the daytime max period (00Z after the local day)
	High  float64   // °F
}

// NBMHighs returns the daytime-high forecasts from every NBM run between
// from and to for a station.
func NBMHighs(station string, from, to time.Time) ([]NBMHigh, error) {
	u := fmt.Sprintf("%s/cgi-bin/request/mos.py?station=%s&model=NBS&sts=%s&ets=%s&format=csv", iem, url.QueryEscape(station),
		from.UTC().Format("2006-01-02T15:04Z"), to.UTC().Format("2006-01-02T15:04Z"))
	resp, err := iemGet(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	rows, err := csv.NewReader(resp.Body).ReadAll()
	if err != nil || len(rows) < 2 {
		return nil, err
	}
	col := map[string]int{}
	for i, h := range rows[0] {
		col[h] = i
	}
	var out []NBMHigh
	for _, r := range rows[1:] {
		v := r[col["ftime"]]
		valid, err1 := time.Parse("2006-01-02 15:04:05", v)
		run, err2 := time.Parse("2006-01-02 15:04:05", r[col["runtime"]])
		x, err3 := strconv.ParseFloat(r[col["txn"]], 64)
		if err1 != nil || err2 != nil || err3 != nil || valid.Hour() != 0 {
			continue // the 12Z value is the overnight low
		}
		out = append(out, NBMHigh{Run: run, Valid: valid, High: x})
	}
	return out, nil
}

// KalshiBracket is one settled temperature market.
type KalshiBracket struct {
	Ticker string
	Event  string // e.g. KXHIGHNY-26OCT14
	Date   string // the local day it's about, "2006-01-02"
	Lo, Hi float64
	Yes    bool // resolved YES
	Sub    string
}

// KalshiSettledBrackets returns a temperature series' settled markets. Each
// bracket covers whole degrees Lo..Hi inclusive (±Inf for the open ends).
func KalshiSettledBrackets(series string, since time.Time) ([]KalshiBracket, error) {
	var out []KalshiBracket
	cursor := ""
	for {
		var page struct {
			Cursor string `json:"cursor"`
			Events []struct {
				Ticker     string `json:"event_ticker"`
				StrikeDate string `json:"strike_date"`
				Markets    []struct {
					Ticker string   `json:"ticker"`
					Type   string   `json:"strike_type"`
					Floor  *float64 `json:"floor_strike"`
					Cap    *float64 `json:"cap_strike"`
					Result string   `json:"result"`
					Sub    string   `json:"yes_sub_title"`
					Close  string   `json:"close_time"`
				} `json:"markets"`
			} `json:"events"`
		}
		q := "/events?limit=100&status=settled&with_nested_markets=true&series_ticker=" + url.QueryEscape(series)
		if cursor != "" {
			q += "&cursor=" + url.QueryEscape(cursor)
		}
		if err := kalshiGet(q, &page); err != nil {
			return out, err
		}
		old := false
		for _, e := range page.Events {
			date, err := eventDate(e.Ticker)
			if err != nil {
				continue
			}
			if date.Before(since) {
				old = true
				continue
			}
			for _, m := range e.Markets {
				b := KalshiBracket{Ticker: m.Ticker, Event: e.Ticker, Date: date.Format("2006-01-02"), Yes: m.Result == "yes", Sub: m.Sub}
				switch {
				case m.Type == "between" && m.Floor != nil && m.Cap != nil:
					b.Lo, b.Hi = *m.Floor, *m.Cap
				case m.Type == "greater" && m.Floor != nil:
					b.Lo, b.Hi = *m.Floor+1, inf
				case m.Type == "less" && m.Cap != nil:
					b.Lo, b.Hi = -inf, *m.Cap-1
				default:
					continue
				}
				if m.Result != "yes" && m.Result != "no" {
					continue
				}
				out = append(out, b)
			}
		}
		if page.Cursor == "" || old {
			return out, nil
		}
		cursor = page.Cursor
		time.Sleep(300 * time.Millisecond)
	}
}

var inf = 1e9

// eventDate reads the day from an event ticker like KXHIGHNY-26OCT14.
func eventDate(ticker string) (time.Time, error) {
	for i := len(ticker) - 1; i >= 0; i-- {
		if ticker[i] == '-' {
			return time.Parse("06Jan02", ticker[i+1:i+4]+ticker[i+4:i+5]+lower(ticker[i+5:i+6])+ticker[i+6:])
		}
	}
	return time.Time{}, fmt.Errorf("no date in %s", ticker)
}

func lower(s string) string {
	if s == "" {
		return s
	}
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}
