// Package hawkes fits multivariate Hawkes processes to event streams.
//
// A Hawkes process models each event (a trade, a post, a news article) as
// raising the near-term rate of further events, in its own stream and in
// others:
//
//	λ_i(t) = μ_i + Σ_{j ∈ P(i)} Σ_k α_ijk · Σ_{t_m^j < t} β_k e^{-β_k (t - t_m^j)}
//
// μ_i is stream i's background rate. α_ijk is the expected number of extra i
// events one j event triggers through decay scale k (1/β_k seconds), so the
// matrix α is the lead-lag graph with timing built in. Working on raw event
// times avoids the Epps effect that bars introduce: a lead of 20 seconds is
// invisible in 5-minute bars but explicit here.
//
// Estimation is EM on the branching structure (each event is either
// background or a child of an earlier event), which keeps every parameter
// non-negative and needs no step size. An L1 penalty on α (MAP with a Laplace
// prior) shrinks links the data does not support to zero. Parents are limited
// to a candidate set P(i), the same idea as the streaming engine's semantic
// candidate graph, which keeps the parameter count well below the event count.
//
// References: Bacry, Mastromatteo & Muzy, "Hawkes processes in finance"
// (2015); Xu, Farajtabar & Zha, "Learning Granger causality for Hawkes
// processes" (ICML 2016); Lewis & Mohler's EM for self-exciting processes.
package hawkes

import (
	"math"
	"sort"
)

// Event is one occurrence in stream Dim at time T (seconds). W > 1 stands
// for W identical events at the same instant (e.g. 40 articles naming Iran in
// one 15-minute news batch). Because events sharing a timestamp never excite
// each other, this is exactly equivalent to listing them separately, and much
// cheaper. W = 0 means 1.
type Event struct {
	T   float64
	Dim int
	W   float64
}

func (e Event) weight() float64 {
	if e.W == 0 {
		return 1
	}
	return e.W
}

// Model is a fitted multivariate Hawkes process.
type Model struct {
	D     int
	Betas []float64 // decay rates, 1/seconds
	Mu    []float64 // background rate per stream, events/second
	// Season, if set, is a shared hour-of-week profile multiplying every
	// background rate (mean 1 over time). Trading and posting both follow the
	// clock, and without this every pair of streams looks linked at the
	// slowest scale simply because both are busy during US daytime.
	Season  []float64
	Parents [][]int       // P(i): streams allowed to excite i (always includes i)
	Alpha   [][][]float64 // Alpha[i][p][k]: excitation of i by Parents[i][p] at scale k
}

// New builds a model whose stream i can be excited by itself and by the
// streams in cand[i]. With cand == nil every stream is self-exciting only.
func New(d int, betas []float64, cand [][]int) *Model {
	m := &Model{D: d, Betas: betas, Mu: make([]float64, d), Parents: make([][]int, d), Alpha: make([][][]float64, d)}
	for i := 0; i < d; i++ {
		ps := []int{i}
		seen := map[int]bool{i: true}
		if cand != nil {
			for _, j := range cand[i] {
				if !seen[j] {
					seen[j] = true
					ps = append(ps, j)
				}
			}
		}
		sort.Ints(ps[1:])
		m.Parents[i] = ps
		m.Alpha[i] = make([][]float64, len(ps))
		for p := range ps {
			m.Alpha[i][p] = make([]float64, len(betas))
		}
	}
	return m
}

// Options controls fitting.
type Options struct {
	Iters  int
	L1     float64 // penalty per unit of cross-excitation (0 = plain MLE)
	Tol    float64 // stop when the per-event log-likelihood improves less than this
	Window [2]float64
}

const slotSec = 3600
const weekSlots = 168

func slot(t float64) int {
	h := int64(math.Floor(t / slotSec))
	return int(((h % weekSlots) + weekSlots) % weekSlots)
}

// season returns the background multiplier at time t.
func (m *Model) season(t float64) float64 {
	if m.Season == nil {
		return 1
	}
	return m.Season[slot(t)]
}

// exposure returns, for each hour-of-week slot, the seconds of [from, to) in it.
func exposure(from, to float64) []float64 {
	e := make([]float64, weekSlots)
	for h := math.Floor(from / slotSec); h*slotSec < to; h++ {
		a, b := math.Max(from, h*slotSec), math.Min(to, (h+1)*slotSec)
		if b > a {
			e[slot(h*slotSec)] += b - a
		}
	}
	return e
}

// bgMass is ∫_from^to season(t) dt.
func (m *Model) bgMass(from, to float64) float64 {
	if m.Season == nil {
		return to - from
	}
	s := 0.0
	for h, x := range exposure(from, to) {
		s += m.Season[h] * x
	}
	return s
}

// UseSeason turns on the shared hour-of-week background profile.
func (m *Model) UseSeason() {
	m.Season = make([]float64, weekSlots)
	for h := range m.Season {
		m.Season[h] = 1
	}
}

// state holds the decayed event sums R[j][k] = Σ_{t_m^j < t} e^{-β_k (t - t_m^j)}.
type state struct {
	r    [][]float64
	last []float64
	pend []Event // events at the current timestamp, not yet history
}

func newState(d, k int) *state {
	s := &state{r: make([][]float64, d), last: make([]float64, d)}
	for j := range s.r {
		s.r[j] = make([]float64, k)
		s.last[j] = math.Inf(-1)
	}
	return s
}

// defer_ records that event e happened; it joins the history only once time
// moves past e.T, so events sharing a timestamp never excite each other. A
// trade and the price move it causes, or one bot hitting two markets in the
// same second, are simultaneous, not one predicting the other.
func (s *state) defer_(e Event) {
	s.pend = append(s.pend, e)
}

// flush adds deferred events older than t to the history.
func (s *state) flush(t float64, betas []float64) {
	if len(s.pend) == 0 || s.pend[0].T >= t {
		return
	}
	for _, e := range s.pend {
		r := s.at(e.Dim, e.T, betas)
		w := e.weight()
		for k := range r {
			r[k] += w
		}
	}
	s.pend = s.pend[:0]
}

// at decays stream j's sums to time t and returns them.
func (s *state) at(j int, t float64, betas []float64) []float64 {
	if dt := t - s.last[j]; dt > 0 && !math.IsInf(s.last[j], -1) {
		for k, b := range betas {
			s.r[j][k] *= math.Exp(-b * dt)
		}
	}
	s.last[j] = t
	return s.r[j]
}

// LogLik returns the log-likelihood of the events that fall in [from, to),
// using all earlier events as history, and the number of events scored.
func (m *Model) LogLik(evs []Event, from, to float64) (float64, int) {
	return m.LogLikDims(evs, from, to, nil)
}

// LogLikDims is LogLik restricted to the streams with only[i] true (nil = all).
// All streams still act as history; only the selected ones are scored.
func (m *Model) LogLikDims(evs []Event, from, to float64, only []bool) (float64, int) {
	ll, n := 0.0, 0
	K := len(m.Betas)
	st := newState(m.D, K)
	for _, e := range evs {
		if e.T >= to {
			break
		}
		st.flush(e.T, m.Betas)
		if e.T >= from && (only == nil || only[e.Dim]) {
			lam := m.Mu[e.Dim] * m.season(e.T)
			for p, j := range m.Parents[e.Dim] {
				r := st.at(j, e.T, m.Betas)
				for k, b := range m.Betas {
					lam += m.Alpha[e.Dim][p][k] * b * r[k]
				}
			}
			ll += e.weight() * math.Log(math.Max(lam, 1e-300))
			n += int(e.weight())
		}
		st.defer_(e)
	}
	// Compensator over [from, to): ∫ λ_i = μ_i (to-from) + Σ α_ijk Σ_m [kernel mass of event m inside the window].
	mass := m.kernelMass(evs, from, to)
	bm := m.bgMass(from, to)
	for i := 0; i < m.D; i++ {
		if only != nil && !only[i] {
			continue
		}
		ll -= m.Mu[i] * bm
		for p, j := range m.Parents[i] {
			for k := range m.Betas {
				ll -= m.Alpha[i][p][k] * mass[j][k]
			}
		}
	}
	return ll, n
}

// kernelMass[j][k] = Σ_{events m of j before `to`} ∫_{max(from,t_m)}^{to} β_k e^{-β_k (s - t_m)} ds.
func (m *Model) kernelMass(evs []Event, from, to float64) [][]float64 {
	mass := make([][]float64, m.D)
	for j := range mass {
		mass[j] = make([]float64, len(m.Betas))
	}
	for _, e := range evs {
		if e.T >= to {
			break
		}
		a := math.Max(from, e.T)
		w := e.weight()
		for k, b := range m.Betas {
			mass[e.Dim][k] += w * (math.Exp(-b*(a-e.T)) - math.Exp(-b*(to-e.T)))
		}
	}
	return mass
}

// Fit runs penalised EM on the events inside opt.Window and returns the
// final training log-likelihood per event.
func (m *Model) Fit(evs []Event, opt Options) float64 {
	from, to := opt.Window[0], opt.Window[1]
	T := to - from
	K := len(m.Betas)
	counts := make([]float64, m.D)
	nTrain := 0
	for _, e := range evs {
		if e.T >= from && e.T < to {
			counts[e.Dim] += e.weight()
			nTrain += int(e.weight())
		}
	}
	// Start: background explains half of each stream, excitation the rest, spread thinly.
	for i := 0; i < m.D; i++ {
		m.Mu[i] = math.Max(counts[i], 1) / T * 0.5
		for p := range m.Parents[i] {
			for k := range m.Betas {
				m.Alpha[i][p][k] = 0.5 / float64(len(m.Parents[i])*K)
			}
		}
	}
	mass := m.kernelMass(evs, from, to)
	expo := exposure(from, to)
	bgSlot := make([]float64, weekSlots)
	bg := make([]float64, m.D)
	acc := make([][][]float64, m.D)
	for i := range acc {
		acc[i] = make([][]float64, len(m.Parents[i]))
		for p := range acc[i] {
			acc[i][p] = make([]float64, K)
		}
	}
	prev := math.Inf(-1)
	var llPer float64
	contrib := make([]float64, 0, 64)
	for it := 0; it < opt.Iters; it++ {
		for i := range acc {
			bg[i] = 0
			for p := range acc[i] {
				clear(acc[i][p])
			}
		}
		st := newState(m.D, K)
		ll := 0.0
		for _, e := range evs {
			if e.T >= to {
				break
			}
			st.flush(e.T, m.Betas)
			if e.T >= from {
				i := e.Dim
				contrib = contrib[:0]
				sea := m.season(e.T)
				lam := m.Mu[i] * sea
				for p, j := range m.Parents[i] {
					r := st.at(j, e.T, m.Betas)
					for k, b := range m.Betas {
						c := m.Alpha[i][p][k] * b * r[k]
						contrib = append(contrib, c)
						lam += c
					}
				}
				lam = math.Max(lam, 1e-300)
				w := e.weight()
				ll += w * math.Log(lam)
				// E-step: split this event among background and each parent/scale.
				rb := w * m.Mu[i] * sea / lam
				bg[i] += rb
				if m.Season != nil {
					bgSlot[slot(e.T)] += rb
				}
				for p := range m.Parents[i] {
					for k := 0; k < K; k++ {
						acc[i][p][k] += w * contrib[p*K+k] / lam
					}
				}
			}
			st.defer_(e)
		}
		bm := m.bgMass(from, to)
		for i := 0; i < m.D; i++ {
			ll -= m.Mu[i] * bm
			for p, j := range m.Parents[i] {
				for k := range m.Betas {
					ll -= m.Alpha[i][p][k] * mass[j][k]
				}
			}
		}
		llPer = ll / float64(max(nTrain, 1))
		// M-step, with the L1 penalty on cross-excitation only (self-excitation
		// is expected in every stream and is not what we are testing).
		for i := 0; i < m.D; i++ {
			m.Mu[i] = math.Max(bg[i], 1e-9) / bm
		}
		if m.Season != nil {
			// Profile update (ECM): background events per slot over the
			// expected count at unit season, then renormalise to mean 1.
			sumMu := 0.0
			for _, mu := range m.Mu {
				sumMu += mu
			}
			tot, w := 0.0, 0.0
			for h := range m.Season {
				if expo[h] > 0 {
					m.Season[h] = math.Max(bgSlot[h], 1e-9) / (sumMu * expo[h])
				}
				tot += m.Season[h] * expo[h]
				w += expo[h]
			}
			for h := range m.Season {
				m.Season[h] *= w / tot
			}
			clear(bgSlot)
		}
		for i := 0; i < m.D; i++ {
			for p, j := range m.Parents[i] {
				pen := opt.L1
				if j == i {
					pen = 0
				}
				for k := range m.Betas {
					den := mass[j][k] + pen
					if den > 0 {
						m.Alpha[i][p][k] = acc[i][p][k] / den
					} else {
						m.Alpha[i][p][k] = 0
					}
				}
			}
		}
		if llPer-prev < opt.Tol && it > 5 {
			break
		}
		prev = llPer
	}
	return llPer
}

// Edge is one fitted cross-excitation link j → i.
type Edge struct {
	From, To int
	Branch   float64 // expected i events triggered per j event (Σ_k α)
	MeanLag  float64 // excitation-weighted mean delay, seconds
}

// Edges returns cross-excitation links with branching ratio at least minBranch.
func (m *Model) Edges(minBranch float64) []Edge {
	var out []Edge
	for i := 0; i < m.D; i++ {
		for p, j := range m.Parents[i] {
			if j == i {
				continue
			}
			b, lag := 0.0, 0.0
			for k, beta := range m.Betas {
				b += m.Alpha[i][p][k]
				lag += m.Alpha[i][p][k] / beta
			}
			if b >= minBranch {
				out = append(out, Edge{From: j, To: i, Branch: b, MeanLag: lag / b})
			}
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Branch > out[b].Branch })
	return out
}

// Live tracks a fitted model's intensities as events stream in, for
// real-time use: how excited each stream is right now, and how many events
// it should produce over the next few minutes given what has happened.
type Live struct {
	M  *Model
	st *state
}

// NewLive starts tracking with no history.
func (m *Model) NewLive() *Live { return &Live{M: m, st: newState(m.D, len(m.Betas))} }

// Add folds in one event. Events must arrive in time order; events sharing a
// timestamp do not excite each other.
func (l *Live) Add(e Event) {
	l.st.flush(e.T, l.M.Betas)
	l.st.defer_(e)
}

// Background is stream i's rate at time t without any excitation.
func (m *Model) Background(i int, t float64) float64 { return m.Mu[i] * m.season(t) }

// Rate is stream i's intensity at time t given all events before t.
func (l *Live) Rate(i int, t float64) float64 {
	l.st.flush(t, l.M.Betas)
	lam := l.M.Background(i, t)
	for p, j := range l.M.Parents[i] {
		r := l.st.at(j, t, l.M.Betas)
		for k, b := range l.M.Betas {
			lam += l.M.Alpha[i][p][k] * b * r[k]
		}
	}
	return lam
}

// Expected is the expected number of stream-i events in (t, t+tau] from the
// background plus the decaying excitation of events already seen (it ignores
// excitation by events that have not happened yet, so it is a lower bound).
func (l *Live) Expected(i int, t, tau float64) float64 {
	l.st.flush(t, l.M.Betas)
	n := l.M.Background(i, t) * tau
	for p, j := range l.M.Parents[i] {
		r := l.st.at(j, t, l.M.Betas)
		for k, b := range l.M.Betas {
			n += l.M.Alpha[i][p][k] * r[k] * (1 - math.Exp(-b*tau))
		}
	}
	return n
}

// Contributions returns, for stream i at time t, the excitation each parent
// stream is currently adding to its rate (parents with nothing to add are
// left out), largest first.
func (l *Live) Contributions(i int, t float64) []Contribution {
	l.st.flush(t, l.M.Betas)
	var out []Contribution
	for p, j := range l.M.Parents[i] {
		r := l.st.at(j, t, l.M.Betas)
		c := 0.0
		for k, b := range l.M.Betas {
			c += l.M.Alpha[i][p][k] * b * r[k]
		}
		if c > 0 {
			out = append(out, Contribution{From: j, Rate: c})
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Rate > out[b].Rate })
	return out
}

// Contribution is one parent's current share of a stream's rate.
type Contribution struct {
	From int
	Rate float64
}
