package live

import (
	"math"
	"math/rand/v2"
	"testing"

	"github.com/gauri-sharmaa/attention-flow/internal/source"
)

func TestBook(t *testing.T) {
	b := newBook()
	b.apply(source.BookUpdate{Snapshot: true,
		Bids: []source.Level{{Price: 0.40, Size: 100}, {Price: 0.39, Size: 50}, {Price: 0.30, Size: 999}},
		Asks: []source.Level{{Price: 0.42, Size: 20}, {Price: 0.43, Size: 30}}})
	if m, ok := b.mid(); !ok || math.Abs(m-0.41) > 1e-9 {
		t.Fatalf("mid = %v %v, want 0.41", m, ok)
	}
	// Within 1¢ of 0.41: bids 0.40 (100); asks 0.42 (20) → (100-20)/120.
	if im := b.imbalance(0.01 + 1e-9); math.Abs(im-80.0/120) > 1e-9 {
		t.Fatalf("imbalance = %v", im)
	}
	b.apply(source.BookUpdate{Bids: []source.Level{{Price: 0.40, Size: 0}}}) // level removed
	if bid, _, _ := b.top(); bid != 0.39 {
		t.Fatalf("best bid after removal = %v", bid)
	}
}

func TestRecorderResolves(t *testing.T) {
	r := newRecorder("", 600)
	r.predict(prediction{t: 0, cid: "a", p: 0.9, norm: 0.1})
	r.predict(prediction{t: 0, cid: "b", p: 0.1, norm: 0.1})
	r.predict(prediction{t: 100, cid: "a", p: 0.5, norm: 0.1}) // spans an outage: dropped
	r.outage(650, 720)
	moved := func(cid string, from, to float64) bool { return cid == "a" }
	r.resolve(650, moved) // both t=0 predictions are due; the t=100 one is not
	if len(r.done) != 2 || !r.done[0].hit || r.done[1].hit || len(r.pending) != 1 {
		t.Fatalf("after first resolve: done %+v pending %d", r.done, len(r.pending))
	}
	r.resolve(800, moved) // t=100 spans the outage: dropped, not scored
	if len(r.done) != 2 || len(r.pending) != 0 {
		t.Fatalf("after second resolve: done %+v pending %d", r.done, len(r.pending))
	}
	w := r.window("", -1)
	// Model: (0.9-1)^2 + (0.1-0)^2 = 0.02 over 2; normal: (0.1-1)^2 + 0.01 = 0.82.
	if math.Abs(w.Brier-0.01) > 1e-9 || w.Skill <= 0.9 {
		t.Fatalf("window = %+v", w)
	}
}

func TestOnlineLearns(t *testing.T) {
	o := newOnline(2)
	rng := rand.New(rand.NewPCG(1, 2))
	right := 0
	for i := 0; i < 4000; i++ {
		x := []float64{rng.Float64()*2 - 1, rng.Float64()*2 - 1}
		up := x[0]+0.3*rng.NormFloat64() > 0 // first feature carries the signal
		if i >= 3000 && (o.prob(x) >= 0.5) == up {
			right++
		}
		o.update(x, up)
	}
	if right < 800 {
		t.Fatalf("online learner right on %d of 1000 after training, want ≥ 800", right)
	}
}

func TestRecorderReloads(t *testing.T) {
	dir := t.TempDir()
	r := newRecorder(dir, 600)
	r.predict(prediction{t: 0, cid: "a", p: 0.3, norm: 0.1})
	r.resolve(700, func(string, float64, float64) bool { return true })
	r.settleDir(dirCall{t: 5, cid: "a", x: make([]float64, nBookFeatures), p: 0.5, pLast: 0.5}, true)
	r.predF.Close()
	r.dirF.Close()
	r2 := newRecorder(dir, 600)
	if len(r2.done) != 1 || !r2.done[0].hit || len(r2.dirDone) != 1 || r2.learner.n != 1 || r2.loadErrs != 0 {
		t.Fatalf("reloaded: done %d dir %d learner %d errs %d", len(r2.done), len(r2.dirDone), r2.learner.n, r2.loadErrs)
	}
}

// A 2¢ trade-price move must land in the market's move history, which is
// what the track record checks predictions against.
func TestIngestRecordsMoves(t *testing.T) {
	e := New(Config{MoveSize: 0.02, Horizon: 600})
	m := &market{LiveMarket: source.LiveMarket{ConditionID: "c"}, ref: -1, bk: newBook(), midRef: -1}
	e.markets, e.byID = []*market{m}, map[string]*market{"c": m}
	for i, p := range []float64{0.50, 0.51, 0.53, 0.52} { // 0.50 → 0.53 is the only 2¢ move
		e.ingest(source.Trade{T: int64(100 + i), ConditionID: "c", YesPrice: p}, true)
	}
	if len(m.moves) != 1 || m.moves[0] != 102 || e.status.LiveMoves != 1 {
		t.Fatalf("moves %v, live moves %d; want [102], 1", m.moves, e.status.LiveMoves)
	}
}
