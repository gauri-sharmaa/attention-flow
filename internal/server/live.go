package server

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"time"

	"github.com/gauri-sharmaa/attention-flow/internal/live"
)

// ServeLive serves the live Polymarket dashboard and streams engine snapshots
// over Server-Sent Events every two seconds.
func ServeLive(addr string, eng *live.Engine) error {
	mux := http.NewServeMux()
	sub, _ := fs.Sub(webFS, "web/live")
	mux.Handle("/", http.FileServer(http.FS(sub)))
	mux.HandleFunc("/api/live", func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		send := func() bool {
			b, err := json.Marshal(eng.Snapshot())
			if err != nil {
				return false
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
				return false
			}
			fl.Flush()
			return true
		}
		if !send() {
			return
		}
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				if !send() {
					return
				}
			case <-r.Context().Done():
				return
			}
		}
	})
	log.Printf("live dashboard on http://localhost%s", addr)
	return http.ListenAndServe(addr, mux)
}
