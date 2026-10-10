package source

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"
)

// Trade is one Polymarket trade reduced to what the live view needs. Trader
// identities in the API response are never kept.
type Trade struct {
	T           int64   // unix seconds
	ConditionID string  // market
	YesPrice    float64 // trade price expressed as the YES price
	USD         float64 // dollar size
	Dir         int     // +1 pushes YES up (buy YES / sell NO), -1 down
	Key         string  // dedupe key (transaction hash + asset)
}

// LiveMarket is the public description of a tradable market.
type LiveMarket struct {
	ConditionID string
	Question    string
	Event       string // parent event slug: sibling outcomes share it
	Sector      string
	YesToken    string
	NoToken     string
	Volume24h   float64
}

// LiveMarkets returns the n busiest tradable markets (see ListActiveMarkets).
func LiveMarkets(n int) ([]LiveMarket, error) {
	ms, err := ListActiveMarkets(n)
	if err != nil {
		return nil, err
	}
	out := make([]LiveMarket, 0, len(ms))
	for _, m := range ms {
		var toks []string
		if json.Unmarshal([]byte(m.ClobTokenIds), &toks) != nil || len(toks) < 2 {
			continue
		}
		ev := m.Slug
		title := m.Question
		if len(m.Events) > 0 {
			ev, title = m.Events[0].Slug, m.Events[0].Title
		}
		out = append(out, LiveMarket{ConditionID: m.ConditionID, Question: m.Question, Event: ev,
			Sector: sectorOf(m.Question + " " + title), YesToken: toks[0], NoToken: toks[1], Volume24h: m.Volume24hr})
	}
	return out, nil
}

func toTrade(t pmTrade, conditionID string, m LiveMarket) Trade {
	p, dir := t.Price, 1
	if t.Asset == m.NoToken {
		p = 1 - p
	}
	if (t.Asset == m.NoToken) == (t.Side == "BUY") {
		dir = -1
	}
	return Trade{T: t.Timestamp, ConditionID: conditionID, YesPrice: p, USD: t.Size * t.Price, Dir: dir}
}

// History returns a market's trades since `from`, oldest first.
func History(m LiveMarket, from int64) ([]Trade, error) {
	client := &http.Client{Timeout: 60 * time.Second}
	raw, err := tradesIn(client, m.ConditionID, from, time.Now().Unix()+1)
	if err != nil {
		return nil, err
	}
	out := make([]Trade, 0, len(raw))
	for i := len(raw) - 1; i >= 0; i-- {
		out = append(out, toTrade(raw[i], m.ConditionID, m))
	}
	return out, nil
}

// Stream follows trades in the given markets in real time over Polymarket's
// public market websocket and calls fn for each one. It returns when the
// connection drops; the caller reconnects.
func Stream(ctx context.Context, markets map[string]LiveMarket, fn func(Trade)) error {
	byToken := map[string]LiveMarket{}
	var ids []string
	for _, m := range markets {
		byToken[m.YesToken], byToken[m.NoToken] = m, m
		ids = append(ids, m.YesToken, m.NoToken)
	}
	c, _, err := websocket.Dial(ctx, "wss://ws-subscriptions-clob.polymarket.com/ws/market", nil)
	if err != nil {
		return err
	}
	defer c.CloseNow()
	c.SetReadLimit(1 << 24) // the first message is every order book
	sub, _ := json.Marshal(map[string]any{"assets_ids": ids, "type": "market"})
	if err := c.Write(ctx, websocket.MessageText, sub); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { // the server drops quiet connections without a ping
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Second):
				c.Write(ctx, websocket.MessageText, []byte("PING"))
			}
		}
	}()
	for {
		_, msg, err := c.Read(ctx)
		if err != nil {
			return err
		}
		if len(msg) == 0 || (msg[0] != '{' && msg[0] != '[') {
			continue // PONG
		}
		var batch []wsEvent
		if msg[0] == '[' {
			json.Unmarshal(msg, &batch)
		} else {
			var ev wsEvent
			json.Unmarshal(msg, &ev)
			batch = []wsEvent{ev}
		}
		for _, ev := range batch {
			m, ok := byToken[ev.AssetID]
			if ev.EventType != "last_trade_price" || !ok {
				continue
			}
			p, _ := strconv.ParseFloat(ev.Price, 64)
			size, _ := strconv.ParseFloat(ev.Size, 64)
			ms, _ := strconv.ParseInt(ev.Timestamp, 10, 64)
			tr := toTrade(pmTrade{Side: ev.Side, Asset: ev.AssetID, Price: p, Size: size, Timestamp: ms / 1000}, m.ConditionID, m)
			tr.Key = ev.TxHash + "/" + ev.AssetID
			fn(tr)
		}
	}
}

type wsEvent struct {
	EventType string `json:"event_type"`
	AssetID   string `json:"asset_id"`
	Price     string `json:"price"`
	Size      string `json:"size"`
	Side      string `json:"side"`
	Timestamp string `json:"timestamp"` // milliseconds
	TxHash    string `json:"transaction_hash"`
}
