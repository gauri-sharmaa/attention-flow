package live

import (
	"bufio"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The track record: every minute the engine writes down each market's odds
// of a 2¢ move in the next Horizon seconds, and Horizon seconds later checks
// whether one happened. Predictions made across a feed outage are dropped,
// since a missed move would count as a miss. Resolved predictions are
// appended to a CSV, so the record survives restarts.
//
// Direction: once a minute, each market with a live order book and no open
// call gets a call on which way the midpoint's next 2¢ move will go. It is
// made by an online logistic regression that only ever learns from calls
// that have already resolved, so every score is out of sample. Two
// baselines are scored on the same calls: a coin flip and "the same way as
// the last midpoint move" (with its hit rate learned the same way).

type prediction struct {
	t       float64
	cid     string
	p, norm float64
}

type outcome struct {
	prediction
	hit bool
}

type dirCall struct {
	t     float64
	cid   string
	x     []float64
	p     float64 // model's P(up)
	pLast float64 // baseline: P(up) from the last midpoint move
}

type dirOutcome struct {
	dirCall
	up bool
}

type recorder struct {
	horizon  float64
	pending  []prediction
	done     []outcome
	outages  [][2]float64
	dirDone  []dirOutcome
	learner  *online
	same     [2]float64 // last-move baseline: times the next move went the same way, and total
	predF    *os.File
	dirF     *os.File
	loadErrs int
}

const nBookFeatures = 7

func newRecorder(dir string, horizon float64) *recorder {
	r := &recorder{horizon: horizon, learner: newOnline(nBookFeatures)}
	if dir == "" {
		return r
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return r
	}
	r.load(dir)
	r.predF = openAppend(filepath.Join(dir, "predictions.csv"), "t,market,p,normal,hit")
	r.dirF = openAppend(filepath.Join(dir, "direction.csv"), "t,market,p_model,p_last,up,"+strings.Join(bookFeatureNames, ","))
	return r
}

var bookFeatureNames = []string{"imb1c", "imb3c", "spread", "flow5m", "last_mid_move", "price", "extremity"}

func openAppend(path, header string) *os.File {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil
	}
	if st, _ := f.Stat(); st != nil && st.Size() == 0 {
		fmt.Fprintln(f, header)
	}
	return f
}

// load replays earlier runs: past outcomes count toward the record, and past
// direction calls retrain the learner in the order they resolved.
func (r *recorder) load(dir string) {
	each := func(name string, fn func([]string)) {
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			return
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Scan()
		for sc.Scan() {
			fn(strings.Split(sc.Text(), ","))
		}
	}
	num := func(s string) float64 { v, _ := strconv.ParseFloat(s, 64); return v }
	each("predictions.csv", func(c []string) {
		if len(c) != 5 {
			r.loadErrs++
			return
		}
		r.done = append(r.done, outcome{prediction{num(c[0]), c[1], num(c[2]), num(c[3])}, c[4] == "1"})
	})
	each("direction.csv", func(c []string) {
		if len(c) != 5+nBookFeatures {
			r.loadErrs++
			return
		}
		x := make([]float64, nBookFeatures)
		for i := range x {
			x[i] = num(c[5+i])
		}
		o := dirOutcome{dirCall{num(c[0]), c[1], x, num(c[2]), num(c[3])}, c[4] == "1"}
		r.dirDone = append(r.dirDone, o)
		r.learn(o)
	})
}

func (r *recorder) predict(p prediction) { r.pending = append(r.pending, p) }

func (r *recorder) outage(from, to float64) { r.outages = append(r.outages, [2]float64{from, to}) }

// resolve settles every prediction whose horizon has passed. moved reports
// whether the market had a 2¢ move in (from, to].
func (r *recorder) resolve(now float64, moved func(cid string, from, to float64) bool) {
	k := 0
	for k < len(r.pending) && r.pending[k].t+r.horizon <= now {
		p := r.pending[k]
		k++
		if r.overlapsOutage(p.t, p.t+r.horizon) {
			continue
		}
		o := outcome{p, moved(p.cid, p.t, p.t+r.horizon)}
		r.done = append(r.done, o)
		if r.predF != nil {
			fmt.Fprintf(r.predF, "%.0f,%s,%.4f,%.4f,%d\n", o.t, o.cid, o.p, o.norm, b2i(o.hit))
		}
	}
	r.pending = r.pending[k:]
	// Outages older than any pending prediction can be forgotten.
	for len(r.outages) > 0 && r.outages[0][1] < now-2*r.horizon {
		r.outages = r.outages[1:]
	}
}

func (r *recorder) overlapsOutage(a, b float64) bool {
	for _, o := range r.outages {
		if o[0] < b && o[1] > a {
			return true
		}
	}
	return false
}

// pLast is the last-move baseline's P(up) for a market whose last midpoint
// move was in direction last (+1, -1, or 0 if none yet).
func (r *recorder) pLast(last float64) float64 {
	pSame := (r.same[0] + 1) / (r.same[1] + 2)
	switch {
	case last > 0:
		return pSame
	case last < 0:
		return 1 - pSame
	}
	return 0.5
}

// settleDir records the outcome of a direction call and learns from it.
func (r *recorder) settleDir(c dirCall, up bool) {
	o := dirOutcome{c, up}
	r.dirDone = append(r.dirDone, o)
	if r.dirF != nil {
		xs := make([]string, len(c.x))
		for i, v := range c.x {
			xs[i] = strconv.FormatFloat(v, 'f', 4, 64)
		}
		fmt.Fprintf(r.dirF, "%.0f,%s,%.4f,%.4f,%d,%s\n", c.t, c.cid, c.p, c.pLast, b2i(up), strings.Join(xs, ","))
	}
	r.learn(o)
}

func (r *recorder) learn(o dirOutcome) {
	if last := o.x[4]; last != 0 {
		r.same[1]++
		if (last > 0) == o.up {
			r.same[0]++
		}
	}
	r.learner.update(o.x, o.up)
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// online is logistic regression trained one example at a time (AdaGrad with
// a little L2), so it can learn while the dashboard runs.
type online struct {
	w, g2 []float64 // w[0] is the intercept
	n     int
}

func newOnline(d int) *online { return &online{w: make([]float64, d+1), g2: make([]float64, d+1)} }

func (o *online) prob(x []float64) float64 {
	s := o.w[0]
	for i, v := range x {
		s += o.w[i+1] * v
	}
	return 1 / (1 + math.Exp(-s))
}

func (o *online) update(x []float64, up bool) {
	y := 0.0
	if up {
		y = 1
	}
	err := o.prob(x) - y
	for i := range o.w {
		v := 1.0
		if i > 0 {
			v = x[i-1]
		}
		g := err * v
		if i > 0 {
			g += 1e-4 * o.w[i]
		}
		o.g2[i] += g * g
		o.w[i] -= 0.1 * g / math.Sqrt(o.g2[i]+1e-8)
	}
	o.n++
}

// ---- summaries for the dashboard ----

// TrackV is the track record as the dashboard shows it.
type TrackV struct {
	Since   int64     `json:"since"`
	Pending int       `json:"pending"`
	Windows []WindowV `json:"windows"`
	Calib   []CalibV  `json:"calib"`
	Dir     DirV      `json:"dir"`
	Alert   string    `json:"alert,omitempty"`
}

// WindowV scores the move odds over one time window. Brier is the mean
// squared error of the probabilities (lower is better); Skill compares the
// model with simply quoting each market's normal rate (above 0 = better).
type WindowV struct {
	Label  string  `json:"label"`
	N      int     `json:"n"`
	MeanP  float64 `json:"meanP"`
	Rate   float64 `json:"rate"`
	Brier  float64 `json:"brier"`
	BrierN float64 `json:"brierNormal"`
	Skill  float64 `json:"skill"`
}

// CalibV is one calibration bucket: predictions between Lo and Hi, their
// average, and how often a move actually followed.
type CalibV struct {
	Lo    float64 `json:"lo"`
	Hi    float64 `json:"hi"`
	N     int     `json:"n"`
	MeanP float64 `json:"meanP"`
	Rate  float64 `json:"rate"`
}

// DirV scores the order-book direction calls.
type DirV struct {
	N       int     `json:"n"`
	Open    int     `json:"open"`
	Acc     float64 `json:"acc"`
	AccLast float64 `json:"accLast"`
	LL      float64 `json:"ll"`
	LLLast  float64 `json:"llLast"`
	Ready   bool    `json:"ready"`
}

var calibEdges = []float64{0, 0.05, 0.1, 0.2, 0.3, 0.5, 1.0001}

func (r *recorder) summary(now float64, open int) TrackV {
	v := TrackV{Pending: len(r.pending)}
	if len(r.done) > 0 {
		v.Since = int64(r.done[0].t)
	}
	// A window is shown once the record is older than it (until then it
	// would just repeat the shorter one); the first is always shown.
	age := 0.0
	if len(r.done) > 0 {
		age = now - r.horizon - r.done[0].t
	}
	for k, w := range []struct {
		label string
		span  float64
	}{{"last hour", 3600}, {"last 24 hours", 86400}, {"all time", math.Inf(1)}} {
		prev := []float64{0, 3600, 86400}[k]
		if k == 0 || age > prev {
			v.Windows = append(v.Windows, r.window(w.label, now-r.horizon-w.span))
		}
	}
	v.Calib = make([]CalibV, len(calibEdges)-1)
	for i := range v.Calib {
		v.Calib[i] = CalibV{Lo: calibEdges[i], Hi: math.Min(calibEdges[i+1], 1)}
	}
	for _, o := range r.done {
		for i := range v.Calib {
			if o.p >= calibEdges[i] && o.p < calibEdges[i+1] {
				c := &v.Calib[i]
				c.N++
				c.MeanP += o.p
				if o.hit {
					c.Rate++
				}
				break
			}
		}
	}
	for i := range v.Calib {
		if c := &v.Calib[i]; c.N > 0 {
			c.MeanP = round(c.MeanP/float64(c.N), 3)
			c.Rate = round(c.Rate/float64(c.N), 3)
		}
	}
	// The early warning: over the last six hours (enough predictions to
	// tell), is the model doing worse than quoting the normal rate?
	if w := r.window("", now-r.horizon-6*3600); age >= 6*3600 && w.N >= 1000 && w.Skill < 0 {
		v.Alert = fmt.Sprintf("Over the last 6 hours the move odds did worse than each market's normal rate (skill %+.1f%%). The pattern may have stopped working.", 100*w.Skill)
	}
	d := DirV{Open: open, Ready: r.learner.n >= 100}
	for _, o := range r.dirDone {
		d.N++
		y := 0.0
		if o.up {
			y = 1
		}
		if (o.p >= 0.5) == o.up {
			d.Acc++
		}
		if (o.pLast >= 0.5) == o.up {
			d.AccLast++
		}
		ll := func(p float64) float64 {
			p = math.Min(math.Max(p, 1e-6), 1-1e-6)
			return -(y*math.Log(p) + (1-y)*math.Log(1-p))
		}
		d.LL += ll(o.p)
		d.LLLast += ll(o.pLast)
	}
	if d.N > 0 {
		n := float64(d.N)
		d.Acc, d.AccLast = round(d.Acc/n, 3), round(d.AccLast/n, 3)
		d.LL, d.LLLast = round(d.LL/n, 4), round(d.LLLast/n, 4)
	}
	v.Dir = d
	return v
}

func (r *recorder) window(label string, from float64) WindowV {
	w := WindowV{Label: label}
	for k := len(r.done) - 1; k >= 0 && r.done[k].t >= from; k-- {
		o := r.done[k]
		y := 0.0
		if o.hit {
			y = 1
		}
		w.N++
		w.MeanP += o.p
		w.Rate += y
		w.Brier += (o.p - y) * (o.p - y)
		w.BrierN += (o.norm - y) * (o.norm - y)
	}
	if w.N > 0 {
		n := float64(w.N)
		if w.BrierN > 0 {
			w.Skill = round(1-w.Brier/w.BrierN, 4)
		}
		w.MeanP, w.Rate = round(w.MeanP/n, 4), round(w.Rate/n, 4)
		w.Brier, w.BrierN = round(w.Brier/n, 5), round(w.BrierN/n, 5)
	}
	return w
}
