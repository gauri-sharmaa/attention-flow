package source

import (
	"encoding/json"
	"net/http"
	"net/url"
	"time"
)

// RewardMarket is a Polymarket market that pays liquidity rewards: a daily
// pool shared among makers whose resting orders sit within MaxSpread (¢) of
// the midpoint and are at least MinSize shares.
type RewardMarket struct {
	ConditionID string
	Daily       float64 // $ a day
	MaxSpread   float64 // cents from the midpoint
	MinSize     float64 // shares
}

// PolyRewardMarkets lists the markets currently paying liquidity rewards.
func PolyRewardMarkets() ([]RewardMarket, error) {
	var out []RewardMarket
	cursor := ""
	for {
		var page struct {
			Next string `json:"next_cursor"`
			Data []struct {
				ConditionID string  `json:"condition_id"`
				MaxSpread   float64 `json:"rewards_max_spread"`
				MinSize     float64 `json:"rewards_min_size"`
				Daily       float64 `json:"total_daily_rate"`
			} `json:"data"`
		}
		u := "https://clob.polymarket.com/rewards/markets/current"
		if cursor != "" {
			u += "?next_cursor=" + url.QueryEscape(cursor)
		}
		if err := getJSON(u, &page); err != nil {
			return out, err
		}
		for _, d := range page.Data {
			out = append(out, RewardMarket{d.ConditionID, d.Daily, d.MaxSpread, d.MinSize})
		}
		if page.Next == "" || page.Next == "LTE=" || len(page.Data) == 0 {
			return out, nil
		}
		cursor = page.Next
		time.Sleep(100 * time.Millisecond)
	}
}

// PolyBookLevels returns a token's full order book.
func PolyBookLevels(token string) (bids, asks []Level, err error) {
	var d struct {
		Bids []struct{ Price, Size string } `json:"bids"`
		Asks []struct{ Price, Size string } `json:"asks"`
	}
	if err = getJSON("https://clob.polymarket.com/book?token_id="+url.QueryEscape(token), &d); err != nil {
		return
	}
	for _, b := range d.Bids {
		bids = append(bids, Level{num(b.Price), num(b.Size)})
	}
	for _, a := range d.Asks {
		asks = append(asks, Level{num(a.Price), num(a.Size)})
	}
	return
}

func getJSON(u string, v any) error {
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		req, _ := http.NewRequest("GET", u, nil)
		req.Header.Set("User-Agent", userAgent)
		var resp *http.Response
		if resp, err = kalshiClient.Do(req); err == nil {
			err = json.NewDecoder(resp.Body).Decode(v)
			resp.Body.Close()
			if err == nil {
				return nil
			}
		}
		time.Sleep(time.Duration(1<<attempt) * time.Second)
	}
	return err
}
