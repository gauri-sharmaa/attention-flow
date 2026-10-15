package xvenue

import (
	"math"
	"testing"
	"time"

	"github.com/gauri-sharmaa/attention-flow/internal/source"
)

func pm(q string, outs ...string) source.PolyMatchMarket {
	if len(outs) == 0 {
		outs = []string{"Yes", "No"}
	}
	return source.PolyMatchMarket{Question: q, Outcomes: outs, Tokens: []string{"a", "b"}}
}

func km(title, yes, event string) source.KalshiMarket {
	return source.KalshiMarket{Title: title, YesSub: yes, Event: event}
}

// Each case is a pair that reads alike; want says whether it settles the
// same, and same which Polymarket outcome is Kalshi YES.
func TestSettle(t *testing.T) {
	cases := []struct {
		name string
		p    source.PolyMatchMarket
		k    source.KalshiMarket
		ok   bool
		same int
	}{
		{"nomination = nominee", pm("Will Gavin Newsom win the 2028 Democratic presidential nomination?"),
			km("Will Gavin Newsom be the Democratic Presidential nominee in 2028?", "Gavin Newsom", "2028 Democratic presidential nominee"), true, 0},
		{"ticket is not a nomination", pm("Will Ron DeSantis win the 2028 Republican presidential nomination?"),
			km("Will Ron DeSantis and JD Vance be the 2028 Republican Presidential ticket?", "Ron DeSantis", "2028 Republican Presidential ticket"), false, 0},
		{"league is not the cup", pm("Will Hull City win the 2026-27 English Premier League (EPL) Championship?"),
			km("Will Hull City win the 2026-27 FA Cup?", "Hull City", "FA Cup Champion"), false, 0},
		{"draw is not a win", pm("Will Arsenal FC vs. Lille OSC end in a draw?"),
			km("Arsenal wins", "Arsenal", "Arsenal vs Lille"), false, 0},
		{"head-to-head, second team", pm("Notre Dame vs. BYU", "Notre Dame", "BYU"),
			km("BYU wins", "BYU", "Notre Dame vs BYU"), true, 1},
		{"tournament winner is not the match", pm("Shanghai Rolex Masters: Hubert Hurkacz vs Alejandro Davidovich Fokina", "Hubert Hurkacz", "Alejandro Davidovich Fokina"),
			km("ATP Shanghai: Alejandro Davidovich Fokina wins", "Alejandro Davidovich Fokina", "ATP Shanghai Winner"), false, 0},
		{"same city, other team", pm("Hapoel Tel Aviv vs. Besiktas", "Hapoel Tel Aviv", "Besiktas"),
			km("Maccabi Tel-Aviv wins", "Maccabi Tel-Aviv", "Maccabi Tel-Aviv vs FC Barcelona"), false, 0},
		{"same total", pm("Arsenal FC vs. Lille OSC: O/U 3.5", "Over", "Under"),
			km("Will over 3.5 goals be scored?", "Over 3.5 goals scored", "Arsenal vs Lille: Total Goals"), true, 0},
		{"same spread", pm("Spread: Arsenal FC (-1.5)", "Arsenal FC", "Lille OSC"),
			km("Arsenal wins by more than 1.5 goals?", "Arsenal wins by more than 1.5 goals", "Arsenal vs Lille: Spread"), true, 0},
		{"same player, different opponent", pm("Rovereto: Lola Radivojevic vs Teodora Kostovic", "Lola Radivojevic", "Teodora Kostovic"),
			km("Teodora Kostovic wins", "Teodora Kostovic", "Jacquemot vs Kostovic"), false, 0},
		{"first half is not the game", pm("Al-Ittihad Club vs. Shabab Al Ahli Dubai: O/U 2.5", "Over", "Under"),
			km("Will over 2.5 1H goals be scored?", "Over 2.5 1H goals scored", "Al-Ittihad vs Shabab Al-Ahli: First Half Total"), false, 0},
		{"same map of a series", pm("Dota 2: Dawn Bulls vs Spirit Academy - Game 2 Winner", "Dawn Bulls", "Spirit Academy"),
			km("Spirit Academy wins map 2", "Spirit Academy", "Spirit Academy vs. Dawn Bulls: Map 2"), true, 1},
		{"vice president is not president", pm("Will Alexandria Ocasio-Cortez win the 2028 US Presidential Election?"),
			km("Who will win the 2028 United States vice presidential election?", "Alexandria Ocasio-Cortez", "2028 Vice President"), false, 0},
		{"two nominees at once", pm("Will Marco Rubio win the 2028 Republican presidential nomination?"),
			km("Will James Talarico and Marco Rubio be the 2028 Democratic and Republican presidential nominees?", "James Talarico and Marco Rubio", "2028 nominees"), false, 0},
		{"total with no teams named", pm("Games Total: O/U 3.5", "Over", "Under"),
			km("Will over 3.5 maps be played in the Movistar KOI vs. Galions match?", "Over 3.5 maps", "KOI vs Galions: Total Maps"), false, 0},
	}
	for _, c := range cases {
		same, rule, ok := settle(c.p, c.k, 0.8)
		if ok != c.ok || (ok && same != c.same) {
			t.Errorf("%s: got ok=%v same=%d (%s), want ok=%v same=%d", c.name, ok, same, rule, c.ok, c.same)
		}
	}
}

// Game 3 and game 5 of a playoff series are different bets.
func TestSameGameNeedsSameDay(t *testing.T) {
	p := pm("Milwaukee Brewers vs. Los Angeles Dodgers", "Milwaukee Brewers", "Los Angeles Dodgers")
	p.GameStart = time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	k := km("Los Angeles D wins", "Los Angeles D", "Game 4: Milwaukee vs Los Angeles D")
	k.Close = time.Date(2026, 10, 16, 3, 0, 0, 0, time.UTC) // the next night's game
	if _, _, ok := settle(p, k, 0.8); ok {
		t.Fatal("matched games on different days")
	}
	k.Close = time.Date(2026, 10, 15, 3, 0, 0, 0, time.UTC) // same game
	if same, _, ok := settle(p, k, 0.8); !ok || same != 1 {
		t.Fatalf("same game: ok=%v same=%d", ok, same)
	}
}

func TestNumbersMustMatch(t *testing.T) {
	end := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	p := []source.PolyMatchMarket{{Question: "Will Core CPI MoM be 0.2% in September?", Outcomes: []string{"Yes", "No"}, Tokens: []string{"a", "b"}, End: end}}
	k := []source.KalshiMarket{{Title: "Will Core CPI MoM be 0.3% in September?", YesSub: "0.3%", Event: "CPI core in September", Close: end}}
	if got := Match(p, k); len(got) != 0 {
		t.Fatalf("matched different thresholds: %+v", got)
	}
}

// Each route must record, and charge fees on, each venue's own price.
func TestPriceRoutes(t *testing.T) {
	q := func(p, n float64) source.Quote { return source.Quote{Price: p, Size: n} }
	rs := priceRoutes(0.05, q(0.30, 100), q(0.68, 50), q(0.29, 20), q(0.72, 500))
	if len(rs) != 2 {
		t.Fatalf("got %d routes", len(rs))
	}
	a, b := rs[0], rs[1] // Kalshi YES + Polymarket other; Polymarket same + Kalshi NO
	if a.Kalshi != 0.30 || a.Poly != 0.72 || b.Kalshi != 0.68 || b.Poly != 0.29 {
		t.Fatalf("prices on the wrong venue: %+v", rs)
	}
	wantB := 1 - 0.68 - 0.29 - KalshiFee(0.68) - PolyFee(0.29, 0.05)
	if math.Abs(b.Net-wantB) > 1e-4 || b.Gross != 0.03 || b.Size != 20 {
		t.Fatalf("route b = %+v, want net %.4f gross 0.03 size 20", b, wantB)
	}
}

func TestPriceBasket(t *testing.T) {
	legs := []Leg{
		{Name: "A", Yes: source.Quote{Price: 0.50, Size: 100}, No: source.Quote{Price: 0.52, Size: 40}},
		{Name: "B", Yes: source.Quote{Price: 0.30, Size: 10}, No: source.Quote{Price: 0.72, Size: 100}},
		{Name: "C", Yes: source.Quote{Price: 0.15, Size: 100}, No: source.Quote{Price: 0.70, Size: 100}},
	}
	no, yes, nok, yok := PriceBasket(legs)
	if !nok || !yok {
		t.Fatal("both sides should price")
	}
	if math.Abs(no.Cost-1.94) > 1e-9 || math.Abs(no.Edge-0.06) > 1e-9 || no.Sets != 40 {
		t.Errorf("all-NO: got %+v, want cost 1.94 edge 0.06 sets 40", no)
	}
	if math.Abs(yes.Edge-0.05) > 1e-9 || yes.Sets != 10 {
		t.Errorf("all-YES: got %+v, want edge 0.05 sets 10", yes)
	}
	legs[0].FeeRate = 0.07 // 0.07·0.52·0.48 ≈ 1.75¢ on the NO leg
	no, _, _, _ = PriceBasket(legs)
	if math.Abs(no.Edge-(0.06-0.07*0.52*0.48)) > 1e-9 {
		t.Errorf("fee not charged: %+v", no)
	}
	legs[1].No = source.Quote{}
	if _, _, nok, _ = PriceBasket(legs); nok {
		t.Error("a leg with no seller can't be bought")
	}
}
