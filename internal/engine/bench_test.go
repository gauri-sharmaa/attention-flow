package engine_test

import (
	"testing"

	"github.com/gauri-sharmaa/attention-flow/internal/core"
	"github.com/gauri-sharmaa/attention-flow/internal/engine"
	"github.com/gauri-sharmaa/attention-flow/internal/semantic"
	"github.com/gauri-sharmaa/attention-flow/internal/sim"
)

// BenchmarkReplay measures end-to-end cost per event on a simulated stream.
func BenchmarkReplay(b *testing.B) {
	u, err := core.LoadUniverse("../../data/universe.txt")
	if err != nil {
		b.Fatal(err)
	}
	cfg := sim.Default()
	cfg.Bars = 4000
	evs, _ := sim.Run(u, cfg)
	cand := semantic.Candidates(u, 40, 0.05)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e := engine.New(u, cand, engine.DefaultConfig(60))
		for _, ev := range evs {
			e.Ingest(ev)
		}
		e.Flush()
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*len(evs)), "ns/event")
	b.ReportMetric(float64(b.Elapsed().Microseconds())/float64(b.N*cfg.Bars), "µs/bar")
}
