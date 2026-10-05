package source

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gauri-sharmaa/attention-flow/internal/core"
)

const (
	pmSearch  = "https://gamma-api.polymarket.com/public-search?q=%s&limit_per_type=10&events_status=active"
	pmHistory = "https://clob.polymarket.com/prices-history?market=%s&interval=max&fidelity=60"
)

// Market is a prediction market attached to a universe entity.
type Market struct {
	Parent   int     `json:"parent"`
	Question string  `json:"question"`
	Token    string  `json:"token"` // CLOB token id of the YES outcome
	Volume   float64 `json:"volume"`
}

type pmEvent struct {
	Title   string `json:"title"`
	Markets []struct {
		Question     string `json:"question"`
		ClobTokenIds string `json:"clobTokenIds"`
		Volume       string `json:"volume"`
		Closed       bool   `json:"closed"`
		Active       bool   `json:"active"`
	} `json:"markets"`
}

// FindMarkets searches Polymarket for each entity and keeps up to perEntity of
// the highest-volume open markets whose question names the entity.
func FindMarkets(u *core.Universe, perEntity int, minVolume float64) ([]Market, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	seen := map[string]bool{}
	var out []Market
	for _, e := range u.Entities {
		resp, err := client.Get(fmt.Sprintf(pmSearch, url.QueryEscape(e.Name)))
		if err != nil {
			return nil, err
		}
		var body struct {
			Events []pmEvent `json:"events"`
		}
		err = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if err != nil {
			continue
		}
		name := strings.ToLower(e.Name)
		var cand []Market
		for _, ev := range body.Events {
			for _, m := range ev.Markets {
				if m.Closed || !m.Active {
					continue
				}
				q := m.Question
				// The topic must be in the question itself, and head-to-head
				// sports games ("X vs. Y") are never about the topic's attention.
				if !strings.Contains(strings.ToLower(q), name) || strings.Contains(q, " vs. ") {
					continue
				}
				var toks []string
				if json.Unmarshal([]byte(m.ClobTokenIds), &toks) != nil || len(toks) == 0 || seen[toks[0]] {
					continue
				}
				vol, _ := strconv.ParseFloat(m.Volume, 64)
				if vol < minVolume {
					continue
				}
				cand = append(cand, Market{Parent: e.ID, Question: q, Token: toks[0], Volume: vol})
			}
		}
		sort.Slice(cand, func(i, j int) bool { return cand[i].Volume > cand[j].Volume })
		for _, m := range cand[:min(perEntity, len(cand))] {
			seen[m.Token] = true
			out = append(out, m)
		}
		time.Sleep(150 * time.Millisecond)
	}
	return out, nil
}

// MarketHistory returns hourly YES prices for one market as events. The value
// is the odds p/(1-p), so the engine's log transform works in log-odds, the
// natural scale for probabilities.
func MarketHistory(client *http.Client, m Market, entity int, from int64) ([]core.Event, error) {
	resp, err := client.Get(fmt.Sprintf(pmHistory, m.Token))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var body struct {
		History []struct {
			T int64   `json:"t"`
			P float64 `json:"p"`
		} `json:"history"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	var out []core.Event
	for _, h := range body.History {
		if h.T < from {
			continue
		}
		p := math.Min(0.99, math.Max(0.01, h.P))
		out = append(out, core.Event{TS: h.T, Entity: int32(entity), Value: p / (1 - p)})
	}
	return out, nil
}

// FetchPolymarket finds markets for the universe, appends them as entities in
// a "market" cluster, and writes universe.txt (base + markets), markets.json
// and events.csv (market prices only; merge with an attention feed to replay).
func FetchPolymarket(u *core.Universe, uniPath, outDir string, from time.Time, perEntity int, minVolume float64) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	ms, err := FindMarkets(u, perEntity, minVolume)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 30 * time.Second}
	var lines []string
	var all []core.Event
	var kept []Market
	for _, m := range ms {
		id := len(u.Entities) + len(kept)
		evs, err := MarketHistory(client, m, id, from.Unix())
		if err != nil || len(evs) < 48 {
			continue
		}
		parent := u.Entities[m.Parent]
		q := strings.NewReplacer("|", " ", "\n", " ").Replace(m.Question)
		lines = append(lines, fmt.Sprintf("PM: %s | - | market/%s | %s polymarket %s",
			q, parent.Cluster, strings.ToLower(strings.ReplaceAll(parent.Name, " ", "")), strings.Join(parent.Tags, " ")))
		all = append(all, evs...)
		kept = append(kept, m)
		fmt.Fprintf(os.Stderr, "%-24s %5d pts  $%.0f  %s\n", parent.Name, len(evs), m.Volume, q)
		time.Sleep(100 * time.Millisecond)
	}
	base, err := os.ReadFile(uniPath)
	if err != nil {
		return err
	}
	uni := string(base) + "# Polymarket markets (value = YES odds)\n" + strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(outDir, "universe.txt"), []byte(uni), 0o644); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(kept, "", "  ")
	if err := os.WriteFile(filepath.Join(outDir, "markets.json"), b, 0o644); err != nil {
		return err
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].TS < all[j].TS })
	f, err := os.Create(filepath.Join(outDir, "events.csv"))
	if err != nil {
		return err
	}
	defer f.Close()
	fmt.Fprintf(os.Stderr, "%d markets\n", len(kept))
	return core.WriteEvents(f, all)
}
