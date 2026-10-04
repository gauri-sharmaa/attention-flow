package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/gauri-sharmaa/attention-flow/internal/core"
	"github.com/gauri-sharmaa/attention-flow/internal/engine"
	"github.com/gauri-sharmaa/attention-flow/internal/semantic"
	"github.com/gauri-sharmaa/attention-flow/internal/sim"
)

func TestExportWritesPlayableSite(t *testing.T) {
	u, err := core.LoadUniverse("../../data/universe.txt")
	if err != nil {
		t.Fatal(err)
	}
	sc := sim.Default()
	sc.Bars = 3000
	evs, _ := sim.Run(u, sc)
	cfg := engine.DefaultConfig(60)
	e := engine.New(u, semantic.Candidates(u, 40, 0.05), cfg)
	dir := t.TempDir()
	n, err := Export(u, evs, e, dir, "test", 2*cfg.Warmup, 5, 50)
	if err != nil {
		t.Fatal(err)
	}
	if n != 50 {
		t.Fatalf("wrote %d frames, want 50", n)
	}
	for _, f := range []string{"index.html", "app.js", "style.css"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("missing %s", f)
		}
	}
	b, err := os.ReadFile(filepath.Join(dir, "frames.json"))
	if err != nil {
		t.Fatal(err)
	}
	var data struct {
		Meta   Meta `json:"meta"`
		Frames []struct {
			Edges []engine.EdgeView `json:"edges"`
			Disl  []engine.DislView `json:"disl"`
		} `json:"frames"`
	}
	if err := json.Unmarshal(b, &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Meta.Names) != len(u.Entities) || data.Meta.Label != "test" {
		t.Errorf("bad meta: %d names, label %q", len(data.Meta.Names), data.Meta.Label)
	}
	if len(data.Frames[0].Edges) == 0 {
		t.Error("first frame must carry the full edge list")
	}
	withDisl := 0
	for _, f := range data.Frames {
		if len(f.Disl) > 0 {
			withDisl++
		}
	}
	if withDisl < len(data.Frames)/2 {
		t.Errorf("only %d/%d frames have dislocations", withDisl, len(data.Frames))
	}
}
