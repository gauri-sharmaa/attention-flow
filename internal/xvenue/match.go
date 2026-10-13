// Package xvenue matches the same question across Polymarket and Kalshi and
// measures what it would cost to hold both sides.
//
// Matching is the risky part: two markets that read alike can still settle
// differently ("ends in a draw" vs "team wins", "is the nominee" vs "is on
// the ticket"). A pair is kept only when it fits one of a few rules whose
// settlement is the same on both venues; everything else is dropped.
package xvenue

import (
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/gauri-sharmaa/attention-flow/internal/source"
)

// Pair is one question listed on both venues.
type Pair struct {
	Poly   source.PolyMatchMarket `json:"poly"`
	Kalshi source.KalshiMarket    `json:"kalshi"`
	// Same is the index of the Polymarket outcome that equals Kalshi YES.
	Same  int     `json:"same"`
	Rule  string  `json:"rule"`
	Score float64 `json:"score"`
}

var (
	stop   = setOf("will the a an of in on by to be is for at or and before after than more less from with any this that next 2026 2027 who what which does do yes no market price close end")
	wordRe = regexp.MustCompile(`[a-z][a-z']+`)
	numRe  = regexp.MustCompile(`\$?(\d+(?:\.\d+)?)\s*(k|m|bn|b|%|bps)?\b`)
)

func setOf(s string) map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(s) {
		m[w] = true
	}
	return m
}

func words(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range wordRe.FindAllString(strings.ToLower(s), -1) {
		if len(w) > 2 && !stop[w] {
			out[w] = true
		}
	}
	return out
}

// numbers extracts every number in s (thresholds, years, dates, scores),
// scaled by k/m/b suffixes, as a sorted key.
func numbers(s string) string {
	s = strings.ReplaceAll(strings.ToLower(s), ",", "")
	var out []float64
	for _, m := range numRe.FindAllStringSubmatch(s, -1) {
		v := atof(m[1])
		switch m[2] {
		case "k":
			v *= 1e3
		case "m":
			v *= 1e6
		case "b", "bn":
			v *= 1e9
		}
		out = append(out, math.Round(v*1e4)/1e4)
	}
	sort.Float64s(out)
	k := ""
	for i, v := range out {
		if i == 0 || v != out[i-1] {
			k += strings.TrimRight(strings.TrimRight(fmtf(v), "0"), ".") + ","
		}
	}
	return k
}

// Match finds pairs between the two venues. Candidates must share rare
// words, carry exactly the same numbers and end within 16 days of each
// other; then a settlement rule must accept them.
func Match(poly []source.PolyMatchMarket, kalshi []source.KalshiMarket) []Pair {
	type doc struct {
		w    map[string]bool
		nums string
	}
	kd := make([]doc, len(kalshi))
	pd := make([]doc, len(poly))
	df := map[string]int{}
	for i, k := range kalshi {
		text := k.Title + " " + k.YesSub
		kd[i] = doc{words(text + " " + k.Event), numbers(text)}
		for w := range kd[i].w {
			df[w]++
		}
	}
	for i, p := range poly {
		pd[i] = doc{words(p.Question + " " + p.Event), numbers(p.Question)}
		for w := range pd[i].w {
			df[w]++
		}
	}
	n := float64(len(kalshi) + len(poly))
	idf := func(w string) float64 { return math.Log(n / float64(df[w])) }
	inv := map[string][]int{}
	for i, d := range kd {
		for w := range d.w {
			if df[w] < 300 {
				inv[w] = append(inv[w], i)
			}
		}
	}
	cos := func(a, b map[string]bool) float64 {
		var num, na, nb float64
		for w := range a {
			x := idf(w) * idf(w)
			na += x
			if b[w] {
				num += x
			}
		}
		for w := range b {
			nb += idf(w) * idf(w)
		}
		if na == 0 || nb == 0 {
			return 0
		}
		return num / math.Sqrt(na*nb)
	}
	var out []Pair
	for i, p := range poly {
		hits := map[int]int{}
		for w := range pd[i].w {
			for _, j := range inv[w] {
				hits[j]++
			}
		}
		var best *Pair
		for j := range hits {
			k := kalshi[j]
			if kd[j].nums != pd[i].nums {
				continue
			}
			if !p.End.IsZero() && !k.Close.IsZero() && math.Abs(p.End.Sub(k.Close).Hours()) > 16*24 {
				continue
			}
			s := cos(pd[i].w, kd[j].w)
			if s < 0.45 || (best != nil && s <= best.Score) {
				continue
			}
			if same, rule, ok := settle(p, k, s); ok {
				best = &Pair{Poly: p, Kalshi: k, Same: same, Rule: rule, Score: math.Round(s*1000) / 1000}
			}
		}
		if best != nil {
			out = append(out, *best)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Score > out[b].Score })
	return out
}

var (
	winRe     = regexp.MustCompile(`^Will (.+?) win the (\d{4}(?:-\d\d)?) (.+?)(?: Championship)?\?$`)
	notSameRe = regexp.MustCompile(`(?i)draw|leading at halftime|by ko|submission|distance|exact score|corners|halftime|1st half|first half`)
	vsRe      = regexp.MustCompile(`(?i) vs\.? `)
	spreadRe  = regexp.MustCompile(`^Spread: (.+?) \(-(\d+\.5)\)$`)
	totalRe   = regexp.MustCompile(`^[^:]+: O/U (\d+(?:\.\d+)?)$`)
	propRe    = regexp.MustCompile(`O/U|Spread|Both Teams|Exact|draw`)
)

// hasWord reports whether w appears in s as a whole word (both normalized).
func hasWord(s, w string) bool {
	for _, f := range strings.Fields(s) {
		if f == w {
			return true
		}
	}
	return false
}

// sameGame checks that a game-level bet is about the same match: both name
// a team in common, and the game times agree. The same two teams can meet
// again within days (a playoff series), so Polymarket's game start must be
// within 12 hours of Kalshi's expected finish; Polymarket's end date is just
// a deadline a week out, used only when there is no game time.
func sameGame(p source.PolyMatchMarket, k source.KalshiMarket) bool {
	switch {
	case k.Close.IsZero():
	case !p.GameStart.IsZero():
		if math.Abs(k.Close.Sub(p.GameStart).Hours()) > 12 {
			return false
		}
	case !p.End.IsZero() && math.Abs(p.End.Sub(k.Close).Hours()) > 36:
		return false
	}
	pw := strings.Fields(norm(p.Question + " " + p.Event))
	kw := norm(k.Event + " " + k.Title)
	for _, w := range pw {
		if len(w) > 3 && !gameStop[w] && hasWord(kw, w) {
			return true
		}
	}
	return false
}

var gameStop = setOf("spread total goals points games maps over under both teams score wins more than full game time")

func norm(s string) string {
	s = strings.ToLower(s)
	s = strings.NewReplacer("st.", "state", "-", " ", "'", "").Replace(s)
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == ' ' {
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// settle decides whether p and k settle identically, and which Polymarket
// outcome equals Kalshi YES.
func settle(p source.PolyMatchMarket, k source.KalshiMarket, score float64) (int, string, bool) {
	q, kt, kev, yes := p.Question, k.Title, k.Event, strings.TrimSpace(k.YesSub)
	yesNo := p.Outcomes[0] == "Yes" && p.Outcomes[1] == "No"
	switch {
	case yesNo:
		// Same person or team winning the same thing.
		if strings.Contains(kt, " and ") && !strings.Contains(q, " and ") {
			return 0, "", false // two people or outcomes at once: a different bet
		}
		if hasWord(norm(kt+" "+kev), "vice") != hasWord(norm(q), "vice") {
			return 0, "", false
		}
		if notSameRe.MatchString(q) {
			return 0, "", false
		}
		if m := winRe.FindStringSubmatch(q); m != nil {
			who := norm(m[1])
			if yes == "" || !(strings.Contains(norm(yes), who) || strings.Contains(who, norm(yes))) || !strings.Contains(kt, m[2]) {
				return 0, "", false
			}
			comp := strings.ReplaceAll(norm(m[3]), "english premier league epl", "premier league")
			hay := norm(kt + " " + kev)
			if strings.Contains(q, "nomination") && strings.Contains(strings.ToLower(kt), "nominee") {
				party := strings.ToLower(strings.Fields(m[3])[0])
				if strings.Contains(strings.ToLower(kt), party) {
					return 0, "nomination = nominee", true
				}
				return 0, "", false
			}
			for _, w := range strings.Fields(comp) {
				if w == "championship" {
					continue
				}
				if w == "us" && (hasWord(hay, "united") || hasWord(hay, "us")) {
					continue
				}
				if !hasWord(hay, w) {
					return 0, "", false
				}
			}
			return 0, "same winner", true
		}
		if score >= 0.9 {
			return 0, "near-identical wording", true
		}
	case vsRe.MatchString(q) && strings.Contains(kt, " wins") && !propRe.MatchString(q):
		// Head-to-head with the two sides as outcomes; Kalshi's event must
		// be the same match-up, not a tournament.
		if !vsRe.MatchString(kev) || !sameGame(p, k) {
			return 0, "", false
		}
		if strings.Contains(strings.ToLower(q), "women") != strings.Contains(strings.ToLower(kev), "women") {
			return 0, "", false
		}
		idx := -1
		for i, o := range p.Outcomes {
			if no, ny := norm(o), norm(yes); no != "" && ny != "" && (strings.Contains(ny, no) || strings.Contains(no, ny)) {
				if idx >= 0 {
					return 0, "", false // both match: ambiguous
				}
				idx = i
			}
		}
		if idx < 0 {
			return 0, "", false
		}
		return idx, "head-to-head", true
	case p.Outcomes[0] == "Over" && strings.Contains(kev, "Total") && strings.HasPrefix(strings.TrimPrefix(yes, "Reg Time: "), "Over"):
		if m := totalRe.FindStringSubmatch(q); m != nil && strings.Contains(yes, m[1]) && sameGame(p, k) {
			return 0, "same total", true
		}
	case strings.Contains(q, "Both Teams to Score") && strings.Contains(kev, "BTTS") && yesNo && sameGame(p, k):
		return 0, "both teams to score", true
	}
	if m := spreadRe.FindStringSubmatch(q); m != nil && strings.Contains(kt, "wins by more than") && strings.Contains(kt, m[2]) && sameGame(p, k) {
		team := norm(m[1])
		hit := false
		for _, w := range strings.Fields(team) {
			if len(w) > 3 && strings.Contains(norm(kt), w) {
				hit = true
			}
		}
		if !hit {
			return 0, "", false
		}
		for i, o := range p.Outcomes {
			if f := strings.Fields(norm(o)); len(f) > 0 && strings.Contains(team, f[0]) {
				return i, "same spread", true
			}
		}
	}
	return 0, "", false
}

func atof(s string) float64 { v, _ := strconv.ParseFloat(s, 64); return v }

func fmtf(v float64) string { return strconv.FormatFloat(v, 'f', 4, 64) }
