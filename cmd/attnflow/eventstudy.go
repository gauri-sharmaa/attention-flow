package main

import (
	"bufio"
	"flag"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// cmdEventStudy asks, without a model: when outside attention surges, does
// trading in the markets about that name pick up, and before or after?
func cmdEventStudy(args []string) error {
	fs := flag.NewFlagSet("eventstudy", flag.ExitOnError)
	dir := fs.String("dir", "data/pmt", "polytrades output (ticks.csv, universe.txt)")
	files := fs.String("outside", "data/pmt/outside-news.csv,data/pmt/outside-reddit.csv,data/pmt/outside-hn.csv", "mention files")
	zMin := fs.Float64("z", 3, "a surge is a 15-minute count this many std above the trailing-week mean")
	boot := fs.Int("boot", 500, "bootstrap resamples for confidence intervals")
	fs.Parse(args)

	streams, evs, err := loadTicks(*dir)
	if err != nil {
		return err
	}
	// Trade times per market, sorted.
	trades := make([][]float64, len(streams))
	for _, e := range evs {
		trades[e.Dim] = append(trades[e.Dim], e.T)
	}
	t0, t1 := evs[0].T, evs[len(evs)-1].T
	const bin = 900.0
	counts := map[string]map[int64]float64{} // "source: name" → bin → mentions
	for _, path := range strings.Split(*files, ",") {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		sc := bufio.NewScanner(f)
		sc.Scan()
		for sc.Scan() {
			p := strings.SplitN(sc.Text(), ",", 3)
			if len(p) < 3 {
				continue
			}
			ts, err := strconv.ParseInt(p[0], 10, 64)
			if err != nil {
				continue
			}
			k := p[1] + ": " + p[2]
			if counts[k] == nil {
				counts[k] = map[int64]float64{}
			}
			// A mention counts in the bin it became public in (its end).
			counts[k][int64(math.Ceil(float64(ts)/bin))]++
		}
		f.Close()
	}

	type surge struct {
		t       float64 // when the surging bin ended (became public)
		markets []int
		key     string
	}
	var surges []surge
	b0, b1 := int64(math.Ceil(t0/bin)), int64(t1/bin)
	week := int64(7 * 86400 / bin)
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		name := k[strings.Index(k, ": ")+2:]
		re := regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\b`)
		var linked []int
		for i, s := range streams {
			if re.MatchString(s.name) {
				linked = append(linked, i)
			}
		}
		if len(linked) == 0 {
			continue
		}
		c := counts[k]
		minCount := 2.0
		if strings.HasPrefix(k, "news") {
			minCount = 5
		}
		last := int64(-1 << 40)
		for b := b0 + week; b <= b1; b++ {
			// Trailing-week mean and std of this stream's 15-minute counts.
			var s1, s2 float64
			for x := b - week; x < b; x++ {
				v := c[x]
				s1 += v
				s2 += v * v
			}
			n := float64(week)
			mu := s1 / n
			sd := math.Sqrt(math.Max(s2/n-mu*mu, 0))
			v := c[b]
			if v >= minCount && v >= mu+*zMin*math.Max(sd, 0.5) && b-last > 8 { // ≥2h apart
				surges = append(surges, surge{t: float64(b) * bin, markets: linked, key: k})
				last = b
			}
		}
	}

	// Response profile: trades in 10-minute steps around each surge, against
	// the same clock window on up to 7 days before and after (not surge days).
	const stepS, nSteps = 600.0, 12
	profile := func(t float64, m int) []float64 {
		tr := trades[m]
		count := func(a, b float64) float64 {
			return float64(sort.SearchFloat64s(tr, b) - sort.SearchFloat64s(tr, a))
		}
		out := make([]float64, 2*nSteps)
		for s := -nSteps; s < nSteps; s++ {
			a, b := t+float64(s)*stepS, t+float64(s+1)*stepS
			obs := count(a, b)
			var ctl []float64
			for d := -7; d <= 7; d++ {
				if d == 0 {
					continue
				}
				o := float64(d) * 86400
				if a+o >= t0 && b+o <= t1 {
					ctl = append(ctl, count(a+o, b+o))
				}
			}
			if len(ctl) < 4 {
				out[s+nSteps] = math.NaN()
				continue
			}
			m := 0.0
			for _, x := range ctl {
				m += x
			}
			m /= float64(len(ctl))
			out[s+nSteps] = math.Log((obs + 1) / (m + 1))
		}
		return out
	}
	type obs struct {
		group int // surge index, for the bootstrap
		prof  []float64
	}
	collect := func(times []float64, groups [][]int) []obs {
		var out []obs
		for g, t := range times {
			for _, m := range groups[g] {
				p := profile(t, m)
				if !math.IsNaN(p[0]) && !math.IsNaN(p[len(p)-1]) {
					out = append(out, obs{g, p})
				}
			}
		}
		return out
	}
	times := make([]float64, len(surges))
	groups := make([][]int, len(surges))
	bySource := map[string]int{}
	for i, s := range surges {
		times[i], groups[i] = s.t, s.markets
		bySource[s.key[:strings.Index(s.key, ":")]]++
	}
	real := collect(times, groups)
	// Placebo: the same name-market pairs at random times, matched on time of day.
	rng := rand.New(rand.NewPCG(61, 62))
	pt := make([]float64, len(surges))
	for i, t := range times {
		days := int((t1 - t0) / 86400)
		pt[i] = t0 + 86400*float64(rng.IntN(days)) + math.Mod(t-t0, 86400)
	}
	placebo := collect(pt, groups)

	fmt.Printf("surges    %d (news %d · reddit %d · hn %d) across names that appear in market questions · %d surge-market pairs\n",
		len(surges), bySource["news"], bySource["reddit"], bySource["hn"], len(real))
	fmt.Println("          trading vs the same clock time on other days, log ratio (0 = normal; +0.10 ≈ 10% busier)")
	summarise := func(label string, o []obs) {
		if len(o) == 0 {
			fmt.Printf("%s: none\n", label)
			return
		}
		mean := func(o []obs, lo, hi int) float64 {
			s, n := 0.0, 0
			for _, x := range o {
				for k := lo; k < hi; k++ {
					s += x.prof[k]
					n++
				}
			}
			return s / float64(n)
		}
		// Bootstrap over surges (pairs from one surge stay together).
		byG := map[int][]obs{}
		var gs []int
		for _, x := range o {
			if byG[x.group] == nil {
				gs = append(gs, x.group)
			}
			byG[x.group] = append(byG[x.group], x)
		}
		ci := func(lo, hi int) (float64, float64) {
			var vals []float64
			for b := 0; b < *boot; b++ {
				var res []obs
				for range gs {
					res = append(res, byG[gs[rng.IntN(len(gs))]]...)
				}
				vals = append(vals, mean(res, lo, hi))
			}
			sort.Float64s(vals)
			return vals[len(vals)/40], vals[len(vals)*39/40]
		}
		pre, post := mean(o, nSteps-6, nSteps), mean(o, nSteps, nSteps+6)
		pl, ph := ci(nSteps-6, nSteps)
		ql, qh := ci(nSteps, nSteps+6)
		fmt.Printf("%-9s hour before %+.3f [%+.3f, %+.3f] · hour after %+.3f [%+.3f, %+.3f]\n", label, pre, pl, ph, post, ql, qh)
		fmt.Printf("          by 10 min, −2h … +2h: ")
		for k := 0; k < 2*nSteps; k++ {
			if k == nSteps {
				fmt.Print("| ")
			}
			fmt.Printf("%+.2f ", mean(o, k, k+1))
		}
		fmt.Println()
	}
	summarise("surges", real)
	summarise("placebo", placebo)
	return nil
}
