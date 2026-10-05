package source

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gauri-sharmaa/attention-flow/internal/core"
)

const dumpURL = "https://dumps.wikimedia.org/other/pageviews/%d/%d-%02d/pageviews-%s0000.gz"

// FetchWikiDumps builds hourly pageviews from Wikimedia's raw hourly dump
// files instead of the REST API. Each file is ~55 MB compressed and lists every
// page on every wiki; it is streamed and filtered on the fly (nothing is
// written to disk), keeping English desktop ("en") plus mobile ("en.m") views
// for the universe's articles. Slower than the API, but not rate-limited.
func FetchWikiDumps(u *core.Universe, outDir string, from, to time.Time, workers int, contact string) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	ids := map[string]int{}
	for _, e := range u.Entities {
		ids[e.Wiki] = e.ID
	}
	var hours []time.Time
	for h := from.Truncate(time.Hour); h.Before(to); h = h.Add(time.Hour) {
		hours = append(hours, h)
	}
	client := &http.Client{Timeout: 5 * time.Minute}
	jobs := make(chan time.Time)
	var mu sync.Mutex
	var all []core.Event
	var done, failed int
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for h := range jobs {
				evs, err := fetchHour(client, h, ids, contact)
				mu.Lock()
				done++
				if err != nil {
					failed++
					fmt.Fprintf(os.Stderr, "skip %s: %v\n", h.Format("2006-01-02 15h"), err)
				} else {
					all = append(all, evs...)
				}
				if done%24 == 0 {
					fmt.Fprintf(os.Stderr, "%d/%d hours\n", done, len(hours))
				}
				mu.Unlock()
			}
		}()
	}
	for _, h := range hours {
		jobs <- h
	}
	close(jobs)
	wg.Wait()
	if failed > len(hours)/10 {
		return fmt.Errorf("%d of %d hours failed", failed, len(hours))
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].TS < all[j].TS })
	f, err := os.Create(filepath.Join(outDir, "events.csv"))
	if err != nil {
		return err
	}
	defer f.Close()
	return core.WriteEvents(f, all)
}

func fetchHour(client *http.Client, h time.Time, ids map[string]int, contact string) ([]core.Event, error) {
	url := fmt.Sprintf(dumpURL, h.Year(), h.Year(), int(h.Month()), h.Format("20060102-15"))
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(2<<attempt) * time.Second)
		}
		req, _ := http.NewRequest("GET", url, nil)
		req.Header.Set("User-Agent", "attention-flow/0.1 (research; "+contact+")")
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			lastErr = fmt.Errorf("%s", resp.Status)
			if resp.StatusCode == http.StatusNotFound {
				return nil, lastErr
			}
			continue
		}
		views, err := ParseDump(resp.Body, ids)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		evs := make([]core.Event, 0, len(ids))
		for id, v := range views {
			evs = append(evs, core.Event{TS: h.Unix(), Entity: int32(id), Value: float64(v + 1)})
		}
		// Articles with no views that hour still get a (tiny) observation.
		seen := map[int]bool{}
		for id := range views {
			seen[id] = true
		}
		for _, id := range ids {
			if !seen[id] {
				evs = append(evs, core.Event{TS: h.Unix(), Entity: int32(id), Value: 1})
			}
		}
		return evs, nil
	}
	return nil, lastErr
}

// ParseDump reads one gzipped hourly pageviews file and sums English desktop
// and mobile views per tracked article. Lines look like:
//
//	en.m Bitcoin 1234 0
func ParseDump(r io.Reader, ids map[string]int) (map[int]int64, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	out := map[int]int64{}
	sc := bufio.NewScanner(gz)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		var rest []byte
		switch {
		case len(line) > 3 && string(line[:3]) == "en ":
			rest = line[3:]
		case len(line) > 5 && string(line[:5]) == "en.m ":
			rest = line[5:]
		default:
			continue
		}
		sp := strings.IndexByte(string(rest), ' ')
		if sp < 0 {
			continue
		}
		id, ok := ids[string(rest[:sp])]
		if !ok {
			continue
		}
		f := strings.Fields(string(rest[sp+1:]))
		if len(f) == 0 {
			continue
		}
		n, err := strconv.ParseInt(f[0], 10, 64)
		if err == nil {
			out[id] += n
		}
	}
	return out, sc.Err()
}
