package server

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"

	"github.com/gauri-sharmaa/attention-flow/internal/core"
	"github.com/gauri-sharmaa/attention-flow/internal/engine"
)

// frame is a snapshot whose edge list is omitted when unchanged, which keeps a
// few hundred frames to a few MB.
type frame struct {
	*engine.Snapshot
	Edges []engine.EdgeView `json:"edges,omitempty"`
}

// Export replays evs once and writes a self-contained static dashboard to dir:
// the web assets plus frames.json, one frame every `every` bars after skip.
func Export(u *core.Universe, evs []core.Event, e *engine.Engine, dir, label string, skip, every, maxFrames int) (int, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}
	sub, _ := fs.Sub(webFS, "web")
	err := fs.WalkDir(sub, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(sub, p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, p), b, 0o644)
	})
	if err != nil {
		return 0, err
	}
	meta := Meta{Label: label, Clusters: u.Clusters, Cluster: u.ClusterIndex(),
		BarSec: e.Config().BarSeconds, Horizon: e.Config().Horizon}
	for _, en := range u.Entities {
		meta.Names = append(meta.Names, en.Name)
	}
	var frames []frame
	var prevEdges []engine.EdgeView
	lastT := 0
	for _, ev := range evs {
		e.Ingest(ev)
		if e.T == lastT {
			continue
		}
		lastT = e.T
		if e.T < skip || (e.T-skip)%every != 0 {
			continue
		}
		s := e.Snapshot(10)
		f := frame{Snapshot: s}
		if !reflect.DeepEqual(s.Edges, prevEdges) {
			f.Edges = s.Edges
			prevEdges = s.Edges
		}
		if len(frames) == 0 {
			f.Edges = s.Edges
		}
		s.Edges = nil
		frames = append(frames, f)
		if len(frames) >= maxFrames {
			break
		}
	}
	b, err := json.Marshal(struct {
		Meta   Meta    `json:"meta"`
		Frames []frame `json:"frames"`
	}{meta, frames})
	if err != nil {
		return 0, err
	}
	return len(frames), os.WriteFile(filepath.Join(dir, "frames.json"), b, 0o644)
}
