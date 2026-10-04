package engine_test

import (
	"math"
	"testing"

	"github.com/gauri-sharmaa/attention-flow/internal/core"
	"github.com/gauri-sharmaa/attention-flow/internal/engine"
	"github.com/gauri-sharmaa/attention-flow/internal/replay"
	"github.com/gauri-sharmaa/attention-flow/internal/semantic"
	"github.com/gauri-sharmaa/attention-flow/internal/sim"
)

func universe(t testing.TB) *core.Universe {
	u, err := core.LoadUniverse("../../data/universe.txt")
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// TestRecoversPlantedStructure is the main end-to-end check: on a simulated
// stream with a known answer key the engine must find the planted lead-lag
// graph with the right lags, forecast better than decay alone, state
// calibrated confidence, and predict shock spread in the right direction.
func TestRecoversPlantedStructure(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end replay")
	}
	u := universe(t)
	sc := sim.Default()
	sc.Bars = 8000
	evs, truth := sim.Run(u, sc)
	cfg := engine.DefaultConfig(sc.BarSeconds)
	rep := replay.Run(u, evs, semantic.Candidates(u, 40, 0.05), cfg, replay.Options{
		EvalStart: 2 * cfg.Warmup, Truth: truth, Checkpoints: []int{truth.MidBar - 1, sc.Bars - 2},
	})
	for _, g := range rep.Graph {
		if g.Precision < 0.85 || g.RecallInCand < 0.9 || g.LagWithin1 < 0.95 {
			t.Errorf("graph at bar %d: precision %.2f, recall in candidates %.2f, lag±1 %.2f",
				g.Bar, g.Precision, g.RecallInCand, g.LagWithin1)
		}
	}
	f := rep.Forecast
	if f.MAEFull >= f.MAEDecay || f.ExcessCorr < 0.3 {
		t.Errorf("forecast: MAE full %.4f vs decay %.4f, excess corr %.3f", f.MAEFull, f.MAEDecay, f.ExcessCorr)
	}
	if top := rep.Signals[9]; top.HitRate < 0.7 {
		t.Errorf("top-decile hit rate %.3f, want ≥ 0.7", top.HitRate)
	}
	for _, b := range rep.Calib {
		if b.N > 5000 && math.Abs(b.MeanP-b.HitRate) > 0.05 {
			t.Errorf("calibration bin %.2f-%.2f: stated %.3f, actual %.3f", b.Lo, b.Hi, b.MeanP, b.HitRate)
		}
	}
	if rep.Brier >= rep.BrierRef {
		t.Errorf("Brier %.4f not better than coin flip %.4f", rep.Brier, rep.BrierRef)
	}
	s := rep.Shocks
	if s.SignHit < 0.65 || s.TrueChild < 0.85 || s.Recall < 0.6 {
		t.Errorf("shocks: direction %.3f, truly downstream %.3f, recall %.3f", s.SignHit, s.TrueChild, s.Recall)
	}
}

// TestBarClock checks that event order within a bar does not matter and that
// a bar with no data for an entity carries its level forward.
func TestBarClock(t *testing.T) {
	u := &core.Universe{Clusters: []string{"x"}}
	for i := 0; i < 3; i++ {
		u.Entities = append(u.Entities, core.Entity{ID: i, Name: string(rune('a' + i)), Cluster: "x", Subtopic: "x/y"})
	}
	run := func(evs []core.Event) *engine.Engine {
		e := engine.New(u, nil, engine.DefaultConfig(60))
		for _, ev := range evs {
			e.Ingest(ev)
		}
		e.Flush()
		return e
	}
	a := run([]core.Event{
		{TS: 0, Entity: 0, Value: 10}, {TS: 5, Entity: 1, Value: 20}, {TS: 9, Entity: 2, Value: 30},
		{TS: 60, Entity: 0, Value: 11}, {TS: 61, Entity: 2, Value: 33},
	})
	b := run([]core.Event{
		{TS: 9, Entity: 2, Value: 30}, {TS: 0, Entity: 0, Value: 10}, {TS: 5, Entity: 1, Value: 20},
		{TS: 61, Entity: 2, Value: 33}, {TS: 60, Entity: 0, Value: 11},
	})
	for j := 0; j < 3; j++ {
		if a.Level(j) != b.Level(j) {
			t.Errorf("entity %d: level depends on event order (%v vs %v)", j, a.Level(j), b.Level(j))
		}
	}
	if a.T != 2 {
		t.Fatalf("closed %d bars, want 2", a.T)
	}
	if got, want := a.Level(1), math.Log(20); got != want {
		t.Errorf("missing entity level = %v, want carried-forward %v", got, want)
	}
	if got, want := a.R(0, 1), math.Log(11)-math.Log(10); math.Abs(got-want) > 1e-12 {
		t.Errorf("innovation = %v, want %v", got, want)
	}
}

// TestGapBars checks that a gap in the stream closes the empty bars in between.
func TestGapBars(t *testing.T) {
	u := &core.Universe{Clusters: []string{"x"}, Entities: []core.Entity{{Name: "a", Cluster: "x", Subtopic: "x/y"}}}
	e := engine.New(u, nil, engine.DefaultConfig(60))
	e.Ingest(core.Event{TS: 0, Entity: 0, Value: 5})
	e.Ingest(core.Event{TS: 600, Entity: 0, Value: 6})
	e.Flush()
	if e.T != 11 {
		t.Fatalf("closed %d bars, want 11", e.T)
	}
	if got := e.R(0, 10); math.Abs(got-math.Log(6.0/5)) > 1e-12 {
		t.Errorf("innovation after gap = %v", got)
	}
}
