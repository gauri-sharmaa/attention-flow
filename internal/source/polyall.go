package source

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gauri-sharmaa/attention-flow/internal/core"
)

const pmMarkets = "https://gamma-api.polymarket.com/markets?active=true&closed=false&limit=100&offset=%d&order=volume24hr&ascending=false"

type pmMarket struct {
	Question      string  `json:"question"`
	ConditionID   string  `json:"conditionId"`
	Slug          string  `json:"slug"`
	EndDate       string  `json:"endDate"`
	OutcomePrices string  `json:"outcomePrices"`
	ClobTokenIds  string  `json:"clobTokenIds"`
	Volume24hr    float64 `json:"volume24hr"`
	VolumeNum     float64 `json:"volumeNum"`
	Events        []struct {
		Title string `json:"title"`
		Slug  string `json:"slug"`
	} `json:"events"`
}

// sectors assigns a market to a broad sector from its wording, so the engine's
// sector factors group things that move together (all Fed markets react to the
// same CPI print). First match wins; order matters.
var sectors = []struct {
	name string
	re   *regexp.Regexp
}{
	{"crypto", regexp.MustCompile(`(?i)\b(bitcoin|btc|ethereum|eth|solana|xrp|crypto|stablecoin|token|airdrop|memecoin|coinbase|binance|hyperliquid|doge)`)},
	{"econ", regexp.MustCompile(`(?i)\b(fed|interest rate|bps|inflation|cpi|recession|gdp|unemployment|tariff|s&p|nasdaq|gold|oil|treasury|powell|rate cut)`)},
	{"tech", regexp.MustCompile(`(?i)\b(openai|anthropic|ai\b|gpt|gemini|apple|google|tesla|spacex|nvidia|microsoft|meta\b|ipo|starship|iphone|model)`)},
	{"world", regexp.MustCompile(`(?i)\b(iran|israel|ukraine|russia|china|taiwan|gaza|hamas|prime minister|ceasefire|strait|nato|putin|zelensky|netanyahu|france|french|uk\b|germany|venezuela|ethiopia|war\b|invade|military)`)},
	{"politics", regexp.MustCompile(`(?i)\b(trump|senate|house|election|democrat|republican|president|governor|congress|midterm|vance|newsom|mayor|nominee|impeach|supreme court|act\b)`)},
	{"sports", regexp.MustCompile(`(?i)\b(nfl|nba|mlb|nhl|super bowl|champion|championship|world cup|premier league|f1|grand prix|ufc|mvp|playoffs|finals|win the 20)`)},
	{"culture", regexp.MustCompile(`(?i)\b(movie|film|album|song|oscar|grammy|emmy|box office|taylor swift|gta|netflix|spotify|youtube|tiktok|celebrity|aliens|jesus)`)},
}

func sectorOf(text string) string {
	for _, s := range sectors {
		if s.re.MatchString(text) {
			return s.name
		}
	}
	return "other"
}

// ListActiveMarkets returns tradable markets, busiest first: priced between 3%
// and 97% (long shots at 1% barely move), resolving at least two weeks out,
// and not single head-to-head games.
func ListActiveMarkets(n int) ([]pmMarket, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	var out []pmMarket
	cutoff := time.Now().Add(14 * 24 * time.Hour)
	for off := 0; off <= 2000 && len(out) < n; off += 100 {
		resp, err := client.Get(fmt.Sprintf(pmMarkets, off))
		if err != nil {
			return nil, err
		}
		var page []pmMarket
		err = json.NewDecoder(resp.Body).Decode(&page)
		resp.Body.Close()
		if err != nil || len(page) == 0 {
			break
		}
		for _, m := range page {
			var prices []string
			if json.Unmarshal([]byte(m.OutcomePrices), &prices) != nil || len(prices) == 0 {
				continue
			}
			p, _ := strconv.ParseFloat(prices[0], 64)
			end, err := time.Parse(time.RFC3339, m.EndDate)
			if err != nil || p <= 0.03 || p >= 0.97 || end.Before(cutoff) || strings.Contains(m.Question, " vs. ") || m.Volume24hr <= 0 {
				continue
			}
			out = append(out, m)
			if len(out) == n {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return out, nil
}

// FetchAllMarkets writes a universe of the n busiest tradable Polymarket
// markets and their price history at barSeconds resolution over the last
// `days` days. Values are YES odds (log-odds after the engine's log).
func FetchAllMarkets(outDir string, n, days int, barSeconds int64, workers int) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	ms, err := ListActiveMarkets(n)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "%d tradable markets\n", len(ms))
	fidelity := max(1, barSeconds/60)
	page := int64(14 * 86400)
	if fidelity == 1 {
		page = 7 * 86400
	}
	now := time.Now().Unix()
	from := now - int64(days)*86400

	client := &http.Client{Timeout: 60 * time.Second}
	type result struct {
		idx int
		evs []core.Event
	}
	jobs := make(chan int)
	results := make(chan result)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				var toks []string
				json.Unmarshal([]byte(ms[i].ClobTokenIds), &toks)
				if len(toks) == 0 {
					results <- result{i, nil}
					continue
				}
				var evs []core.Event
				for s := from; s < now; s += page {
					u := fmt.Sprintf("https://clob.polymarket.com/prices-history?market=%s&startTs=%d&endTs=%d&fidelity=%d", toks[0], s, min(s+page, now), fidelity)
					var body struct {
						History []struct {
							T int64   `json:"t"`
							P float64 `json:"p"`
						} `json:"history"`
					}
					for attempt := 0; attempt < 3; attempt++ {
						resp, err := client.Get(u)
						if err == nil {
							err = json.NewDecoder(resp.Body).Decode(&body)
							resp.Body.Close()
						}
						if err == nil {
							break
						}
						time.Sleep(time.Duration(1+attempt) * time.Second)
					}
					for _, h := range body.History {
						p := math.Min(0.99, math.Max(0.01, h.P))
						evs = append(evs, core.Event{TS: h.T, Value: p / (1 - p)})
					}
				}
				results <- result{i, evs}
			}
		}()
	}
	go func() {
		for i := range ms {
			jobs <- i
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()
	hist := make([][]core.Event, len(ms))
	done := 0
	for r := range results {
		hist[r.idx] = r.evs
		done++
		if done%50 == 0 {
			fmt.Fprintf(os.Stderr, "%d/%d markets\n", done, len(ms))
		}
	}

	var lines []string
	var all []core.Event
	sec := map[string]int{}
	clean := strings.NewReplacer("|", " ", "\n", " ")
	for i, m := range ms {
		if len(hist[i]) < 200 {
			continue // too little history to learn anything
		}
		id := len(lines)
		event, eslug := m.Question, m.Slug
		if len(m.Events) > 0 {
			event, eslug = m.Events[0].Title, m.Events[0].Slug
		}
		s := sectorOf(m.Question + " " + event)
		sec[s]++
		// Subtopic = parent event, so sibling outcomes ("25 bps cut" / "no
		// change") are always tested against each other.
		lines = append(lines, fmt.Sprintf("%s | - | %s/%s | %s",
			clean.Replace(m.Question), s, eslug, strings.ReplaceAll(eslug, "-", " ")))
		for _, ev := range hist[i] {
			ev.Entity = int32(id)
			all = append(all, ev)
		}
	}
	uni := "# Busiest tradable Polymarket markets (value = YES odds)\n" + strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(outDir, "universe.txt"), []byte(uni), 0o644); err != nil {
		return err
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].TS < all[j].TS })
	f, err := os.Create(filepath.Join(outDir, "events.csv"))
	if err != nil {
		return err
	}
	defer f.Close()
	fmt.Fprintf(os.Stderr, "%d markets with history, %d points, sectors %v\n", len(lines), len(all), sec)
	return core.WriteEvents(f, all)
}
