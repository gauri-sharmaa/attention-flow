// Package source loads real attention data.
//
// Wikipedia pageviews are the one free, public, hourly attention signal that
// covers companies, people and concepts alike, so they are the default feed.
// The engine only sees core.Event, so adding Kaito mindshare, Google Trends or
// an attention-market price feed means writing one more function that emits
// events in the same CSV format.
package source

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/gauri-sharmaa/attention-flow/internal/core"
)

const pageviewsAPI = "https://wikimedia.org/api/rest_v1/metrics/pageviews/per-article/en.wikipedia/all-access/user/%s/hourly/%s/%s"
const extractAPI = "https://en.wikipedia.org/api/rest_v1/page/summary/%s"

type pvResponse struct {
	Items []struct {
		Timestamp string `json:"timestamp"` // YYYYMMDDHH
		Views     int64  `json:"views"`
	} `json:"items"`
}

// ParsePageviews converts one per-article API response into events.
func ParsePageviews(r io.Reader, entity int) ([]core.Event, error) {
	var resp pvResponse
	if err := json.NewDecoder(r).Decode(&resp); err != nil {
		return nil, err
	}
	out := make([]core.Event, 0, len(resp.Items))
	for _, it := range resp.Items {
		ts, err := time.Parse("2006010215", it.Timestamp)
		if err != nil {
			return nil, fmt.Errorf("timestamp %q: %w", it.Timestamp, err)
		}
		// +1 so a zero-view hour is still a valid (tiny) observation.
		out = append(out, core.Event{TS: ts.Unix(), Entity: int32(entity), Value: float64(it.Views + 1)})
	}
	return out, nil
}

type summary struct {
	Extract string `json:"extract"`
}

func get(client *http.Client, u, contact string) (*http.Response, error) {
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}
	// Wikimedia asks API clients to identify themselves with a contact address.
	req.Header.Set("User-Agent", "attention-flow/0.1 (research; "+contact+")")
	for attempt := 0; ; attempt++ {
		resp, err := client.Do(req)
		if err == nil && resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
			return resp, nil
		}
		if err == nil {
			resp.Body.Close()
		}
		if attempt == 4 {
			if err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("GET %s: %s", u, resp.Status)
		}
		time.Sleep(time.Duration(1<<attempt) * time.Second)
	}
}

// FetchWikipedia downloads hourly pageviews for every entity between from and
// to, plus each article's summary text, and writes events.csv and text.json.
func FetchWikipedia(u *core.Universe, outDir string, from, to time.Time, contact string) error {
	if contact == "" {
		return fmt.Errorf("pass -contact you@example.com (Wikimedia requires a contact in the User-Agent)")
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	client := &http.Client{Timeout: 30 * time.Second}
	var all []core.Event
	text := map[string]string{}
	for _, e := range u.Entities {
		title := url.PathEscape(e.Wiki)
		resp, err := get(client, fmt.Sprintf(pageviewsAPI, title, from.Format("2006010215"), to.Format("2006010215")), contact)
		if err != nil {
			return fmt.Errorf("%s: %w", e.Name, err)
		}
		if resp.StatusCode == http.StatusNotFound {
			resp.Body.Close()
			fmt.Fprintf(os.Stderr, "skip %s: no pageviews for %q\n", e.Name, e.Wiki)
			continue
		}
		evs, err := ParsePageviews(resp.Body, e.ID)
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", e.Name, err)
		}
		all = append(all, evs...)

		if resp, err := get(client, fmt.Sprintf(extractAPI, title), contact); err == nil {
			var s summary
			if resp.StatusCode == http.StatusOK && json.NewDecoder(resp.Body).Decode(&s) == nil {
				text[e.Name] = s.Extract
			}
			resp.Body.Close()
		}
		fmt.Fprintf(os.Stderr, "%-32s %5d hours\n", e.Name, len(evs))
		time.Sleep(100 * time.Millisecond) // stay well under the API rate limit
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].TS < all[j].TS })
	f, err := os.Create(filepath.Join(outDir, "events.csv"))
	if err != nil {
		return err
	}
	defer f.Close()
	if err := core.WriteEvents(f, all); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(text, "", "  ")
	return os.WriteFile(filepath.Join(outDir, "text.json"), b, 0o644)
}

// AttachText loads text.json (entity name → description) into the universe.
func AttachText(u *core.Universe, path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var text map[string]string
	if err := json.Unmarshal(b, &text); err != nil {
		return err
	}
	for i := range u.Entities {
		u.Entities[i].Text = text[u.Entities[i].Name]
	}
	return nil
}
