// Package server streams engine snapshots to the dashboard.
//
// One goroutine owns the engine and replays events against a bar clock at a
// chosen speed; after each frame it marshals a snapshot once and fans it out to
// every connected browser over Server-Sent Events. Slow clients drop frames
// instead of slowing the engine.
package server

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"math"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gauri-sharmaa/attention-flow/internal/core"
	"github.com/gauri-sharmaa/attention-flow/internal/engine"
)

//go:embed web
var webFS embed.FS

type hub struct {
	mu   sync.Mutex
	subs map[chan []byte]struct{}
	last []byte
}

func (h *hub) publish(b []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.last = b
	for c := range h.subs {
		select {
		case c <- b:
		default: // client is behind; it will get the next frame
		}
	}
}

func (h *hub) subscribe() (chan []byte, []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := make(chan []byte, 4)
	h.subs[c] = struct{}{}
	return c, h.last
}

func (h *hub) unsubscribe(c chan []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.subs, c)
}

// Meta is sent once per page load.
type Meta struct {
	Label    string   `json:"label"`
	Clusters []string `json:"clusters"`
	Names    []string `json:"names"`
	Cluster  []int    `json:"cluster"`
	BarSec   int64    `json:"barSec"`
	Horizon  int      `json:"horizon"`
}

// Options configures Serve.
type Options struct {
	Addr      string
	Speed     float64 // bars per second
	SkipBars  int     // bars to process instantly before pacing (warm-up)
	Label     string
	NewEngine func() *engine.Engine
}

// Serve replays evs through fresh engines forever and serves the dashboard.
func Serve(u *core.Universe, evs []core.Event, opt Options) error {
	h := &hub{subs: map[chan []byte]struct{}{}}
	var speedBits atomic.Uint64
	speedBits.Store(math.Float64bits(opt.Speed))
	var paused atomic.Bool

	probe := opt.NewEngine()
	meta := Meta{Label: opt.Label, Clusters: u.Clusters, Cluster: u.ClusterIndex(),
		BarSec: probe.Config().BarSeconds, Horizon: probe.Config().Horizon}
	for _, e := range u.Entities {
		meta.Names = append(meta.Names, e.Name)
	}
	metaJSON, _ := json.Marshal(meta)

	go func() {
		for {
			run(opt.NewEngine(), evs, opt.SkipBars, h, &speedBits, &paused)
		}
	}()

	mux := http.NewServeMux()
	sub, _ := fs.Sub(webFS, "web")
	mux.Handle("/", http.FileServer(http.FS(sub)))
	mux.HandleFunc("/api/meta", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(metaJSON)
	})
	mux.HandleFunc("/api/control", func(w http.ResponseWriter, r *http.Request) {
		if v, err := strconv.ParseFloat(r.URL.Query().Get("speed"), 64); err == nil && v > 0 && v <= 1000 {
			speedBits.Store(math.Float64bits(v))
		}
		if p := r.URL.Query().Get("pause"); p != "" {
			paused.Store(p == "1")
		}
		fmt.Fprintf(w, `{"speed":%g,"paused":%v}`, math.Float64frombits(speedBits.Load()), paused.Load())
	})
	mux.HandleFunc("/api/stream", func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		c, last := h.subscribe()
		defer h.unsubscribe(c)
		send := func(b []byte) bool {
			if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
				return false
			}
			fl.Flush()
			return true
		}
		if last != nil && !send(last) {
			return
		}
		for {
			select {
			case b := <-c:
				if !send(b) {
					return
				}
			case <-r.Context().Done():
				return
			}
		}
	})
	log.Printf("dashboard on http://localhost%s (%s data, %d events)", opt.Addr, opt.Label, len(evs))
	return http.ListenAndServe(opt.Addr, mux)
}

// run replays the stream once, pacing bar closes to the current speed.
func run(e *engine.Engine, evs []core.Event, skip int, h *hub, speedBits *atomic.Uint64, paused *atomic.Bool) {
	lastT := 0
	next := time.Now()
	const maxFPS = 15
	frameEvery := 1
	lastFrame := time.Time{}
	for _, ev := range evs {
		e.Ingest(ev)
		if e.T == lastT {
			continue
		}
		lastT = e.T
		if e.T < skip {
			continue
		}
		for paused.Load() {
			time.Sleep(50 * time.Millisecond)
			next = time.Now()
		}
		speed := math.Float64frombits(speedBits.Load())
		frameEvery = max(1, int(math.Ceil(speed/maxFPS)))
		if e.T%frameEvery == 0 || time.Since(lastFrame) > time.Second {
			if b, err := json.Marshal(e.Snapshot(10)); err == nil {
				h.publish(b)
				lastFrame = time.Now()
			}
		}
		next = next.Add(time.Duration(float64(time.Second) / speed))
		if d := time.Until(next); d > 0 {
			time.Sleep(d)
		} else if d < -time.Second {
			next = time.Now() // fell behind (e.g. after a pause); do not burst
		}
	}
	e.Flush()
}
