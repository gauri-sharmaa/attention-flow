package replay

import (
	"fmt"
	"strings"
)

// Format renders a report as plain text.
func Format(r *Report) string {
	var b strings.Builder
	p := func(f string, a ...any) { fmt.Fprintf(&b, f, a...) }
	d := r.Data
	p("data      %d entities · %d bars of %ds · %d events · %d candidate pairs · H=%d bars\n",
		d.Entities, d.Bars, d.BarSeconds, d.Events, d.Candidates, d.Horizon)

	f := r.Forecast
	p("\nforecast  (H-bar level change, %d scored)\n", f.N)
	p("  MAE  zero %.4f · decay %.4f · full %.4f  (%.1f%% better than decay)\n",
		f.MAEZero, f.MAEDecay, f.MAEFull, 100*(1-f.MAEFull/f.MAEDecay))
	p("  R²   decay %.3f · full %.3f\n", f.R2Decay, f.R2Full)
	p("  excess move  R² %.3f · corr %.3f\n", f.ExcessR2, f.ExcessCorr)
	if f.OracleCorr != 0 {
		p("  oracle       R² %.3f · corr %.3f  (true model; engine gets %.0f%% of the reachable R²)\n",
			f.OracleR2, f.OracleCorr, 100*f.ExcessR2/f.OracleR2)
	}

	p("\nsignals   by decile of |dislocation|/σ\n")
	p("  dec      n   mean|d|   hit   capture\n")
	for _, s := range r.Signals {
		p("  %3d %7d   %.4f   %.3f  %.2f\n", s.Decile, s.N, s.MeanAbs, s.HitRate, s.Capture)
	}

	if len(r.Clusters) > 1 {
		p("\nby cluster          excess corr  top-10%% hit   skill vs no-change  vs own history\n")
		for _, c := range r.Clusters {
			p("  %-18s %6.3f       %.3f        %+7.3f            %+7.3f   (n=%d)\n", c.Cluster, c.ExcessCorr, c.TopHit, c.SkillVsZero, c.SkillVsOwn, c.N)
		}
	}
	if len(r.Links) > 0 {
		p("\nlinks at end ")
		for _, l := range r.Links {
			p(" %s→%s %d ·", l.From, l.To, l.N)
		}
		p("\n")
	}

	p("\ncalibration  (Brier %.4f vs coin flip %.4f)\n", r.Brier, r.BrierRef)
	p("  stated   actual      n\n")
	for _, c := range r.Calib {
		if c.N == 0 {
			continue
		}
		p("  %.3f    %.3f  %7d\n", c.MeanP, c.HitRate, c.N)
	}

	s := r.Shocks
	p("\nshocks    %d detected · %d predicted child moves\n", s.Shocks, s.Children)
	p("  corr %.3f · right direction %.3f · ≥half the size %.3f\n", s.Corr, s.SignHit, s.BigHit)
	if s.SourceTrue > 0 {
		p("  detected = planted %.3f · planted detected %.3f · children truly downstream %.3f\n", s.SourceTrue, s.Recall, s.TrueChild)
	}
	for _, x := range s.ByDepth {
		p("  depth %d  n=%-6d direction %.3f  corr %.3f\n", x.Depth, x.N, x.SignHit, x.Corr)
	}

	if len(r.Graph) > 0 {
		p("\ngraph     learned vs planted\n")
		p("  bar     learned true  prec  recall  (in cand)  cand-recall  lag=  lag±1  false: retired shortcut confounded other\n")
		for _, g := range r.Graph {
			p("  %-7d %-7d %-5d %.3f %.3f   (%.3f)    %.3f        %.3f %.3f  %7d %8d %10d %5d\n",
				g.Bar, g.Learned, g.True, g.Precision, g.Recall, g.RecallInCand, g.CandRecall,
				g.LagExact, g.LagWithin1, g.Retired, g.Indirect, g.Confounded, g.Spurious+g.Reversed)
		}
	}

	l := r.Latency
	p("\nlatency   ingest p50 %.0fns · bar close p50 %.0fµs p99 %.0fµs max %.0fµs · %.2fM events/s\n",
		l.IngestNsP50, l.CloseUsP50, l.CloseUsP99, l.CloseUsMax, l.EventsPerS/1e6)
	e := r.Engine
	p("engine    %d edges added · %d dropped · %d shocks\n", e.Promoted, e.Dropped, e.Shocks)
	return b.String()
}
