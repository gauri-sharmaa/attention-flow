package xvenue

import (
	"time"

	"github.com/gauri-sharmaa/attention-flow/internal/source"
)

// A Route is one way to hold both sides of a pair so that exactly $1 pays
// out whatever happens: Kalshi YES with the opposite Polymarket outcome, or
// the same Polymarket outcome with Kalshi NO.
type Route struct {
	Name   string  `json:"route"`
	Kalshi float64 `json:"kalshi"` // price paid on Kalshi
	Poly   float64 `json:"poly"`   // price paid on Polymarket
	Gross  float64 `json:"gross"`  // $1 minus the cost, before fees
	Net    float64 `json:"net"`    // after both venues' taker fees
	Size   float64 `json:"size"`   // contracts available at those prices on both venues
}

// KalshiFee is Kalshi's taker fee per contract at price p (7% × p × (1−p)).
func KalshiFee(p float64) float64 { return 0.07 * p * (1 - p) }

// PolyFee is Polymarket's taker fee per share at price p. Its docs give
// two forms (rate·p·(1−p) and rate·p·(p·(1−p))^e); this uses the larger.
func PolyFee(p, rate float64) float64 { return rate * p * (1 - p) }

// Quote prices both routes from the venues' current order books.
func Quote(p Pair) ([]Route, error) {
	kYes, kNo, err := source.KalshiAsks(p.Kalshi.Ticker)
	if err != nil {
		return nil, err
	}
	same, err1 := source.PolyAsk(p.Poly.Tokens[p.Same])
	other, err2 := source.PolyAsk(p.Poly.Tokens[1-p.Same])
	var out []Route
	add := func(name string, k, q source.Quote, err error) {
		if err != nil || k.Price <= 0 || q.Price <= 0 {
			return
		}
		cost := k.Price + q.Price
		out = append(out, Route{Name: name, Kalshi: k.Price, Poly: q.Price, Gross: round4(1 - cost),
			Net: round4(1 - cost - KalshiFee(k.Price) - PolyFee(q.Price, p.Poly.FeeRate)), Size: min(k.Size, q.Size)})
	}
	add("kalshi yes + poly other", kYes, other, err2)
	add("poly same + kalshi no", same, kNo, err1)
	return out, nil
}

// Annualized turns a locked-in gain into a yearly return: the gain over the
// money tied up, scaled by the time until the market resolves.
func Annualized(r Route, resolves, now time.Time) float64 {
	days := resolves.Sub(now).Hours() / 24
	cost := 1 - r.Gross
	if days < 1 {
		days = 1
	}
	return r.Net / cost * 365 / days
}

func round4(x float64) float64 {
	if x < 0 {
		return -round4(-x)
	}
	return float64(int64(x*10000+0.5)) / 10000
}
