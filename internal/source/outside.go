package source

import (
	"archive/zip"
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/gauri-sharmaa/attention-flow/internal/core"
)

// Outside attention: who is talking about a market's subject outside the
// market. Each mention is one event at the time it became public, written as
// "ts,source,keyword" rows so event-time models can use them directly.

var notNames = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`Will Won't The A An No Not Next Another Any Who What Which When How Before After By In On Of At To For
		January February March April May June July August September October November December Q1 Q2 Q3 Q4
		Yes Over Under Above Below Between End Out Up Down New First Second Third Most Least Best Top Highest Lowest
		Win Wins Won Become Be Is Are Announce Announces Released Release Hit Reach Dip Close Say Says Said
		Senate House Party Election President Prime Minister Mayor Governor Chancellor Premier Race Seat Seats
		Market Cap IPO AI Model GDP CPI Rate Rates Bank Interest Change Meeting Increase Decrease Cut Hike Bps
		HIGH LOW Day Billionaire Reserve English French Spanish German`) {
		notNames[w] = true
	}
}

func isCap(w string) bool {
	r := []rune(w)
	return len(r) > 1 && unicode.IsUpper(r[0])
}

// Keywords pulls the proper names out of market questions: maximal runs of
// capitalised words (allowing "of"/"the" inside a run, as in "Strait of
// Hormuz"), minus generic words. Names that appear in more markets come first.
func Keywords(questions []string, max int) []string {
	count := map[string]int{}
	for _, q := range questions {
		words := strings.FieldsFunc(q, func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' && r != '.' && r != '\''
		})
		seen := map[string]bool{}
		for i := 0; i < len(words); {
			if !isCap(words[i]) || notNames[words[i]] {
				i++
				continue
			}
			j := i + 1
			for j < len(words) && (isCap(words[j]) && !notNames[words[j]] || (words[j] == "of" || words[j] == "the") && j+1 < len(words) && isCap(words[j+1])) {
				j++
			}
			name := strings.TrimSuffix(strings.TrimRight(strings.Join(words[i:j], " "), ".'"), "'s")
			if len(name) > 2 && !seen[name] {
				seen[name] = true
				count[name]++
			}
			i = j
		}
	}
	var ks []string
	for k := range count {
		ks = append(ks, k)
	}
	sort.Slice(ks, func(a, b int) bool {
		if count[ks[a]] != count[ks[b]] {
			return count[ks[a]] > count[ks[b]]
		}
		return ks[a] < ks[b]
	})
	if len(ks) > max {
		ks = ks[:max]
	}
	return ks
}

// mentionWriter serialises "ts,source,keyword" rows from many goroutines.
type mentionWriter struct {
	mu sync.Mutex
	w  *bufio.Writer
	n  int
}

func (m *mentionWriter) add(ts int64, src, kw string) {
	m.mu.Lock()
	fmt.Fprintf(m.w, "%d,%s,%s\n", ts, src, kw)
	m.n++
	m.mu.Unlock()
}

func keywordMatcher(keywords []string) *Matcher {
	u := &core.Universe{}
	for i, k := range keywords {
		u.Entities = append(u.Entities, core.Entity{ID: i, Name: k})
	}
	return NewMatcher(u)
}

// FetchGDELT scans GDELT 2.1 Global Knowledge Graph files (one per 15
// minutes, every online news article worldwide) and records an event for each
// article whose named people, organisations or places include a keyword. The
// event time is the end of the article's 15-minute batch, when it became
// available, so news can never appear earlier than it really did.
func FetchGDELT(keywords []string, from, to time.Time, out io.Writer, workers int) (int, error) {
	m := keywordMatcher(keywords)
	mw := &mentionWriter{w: bufio.NewWriter(out)}
	client := &http.Client{Timeout: 2 * time.Minute}
	jobs := make(chan time.Time)
	var wg sync.WaitGroup
	var mu sync.Mutex
	done, failed := 0, 0
	var stamps []time.Time
	for t := from.Truncate(15 * time.Minute).Add(15 * time.Minute); !t.After(to); t = t.Add(15 * time.Minute) {
		stamps = append(stamps, t)
	}
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var hits []int
			for t := range jobs {
				err := func() error {
					u := fmt.Sprintf("https://data.gdeltproject.org/gdeltv2/%s.gkg.csv.zip", t.UTC().Format("20060102150405"))
					resp, err := client.Get(u)
					if err != nil {
						return err
					}
					defer resp.Body.Close()
					if resp.StatusCode != http.StatusOK {
						return fmt.Errorf("%s", resp.Status)
					}
					body, err := io.ReadAll(resp.Body)
					if err != nil {
						return err
					}
					zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
					if err != nil || len(zr.File) == 0 {
						return fmt.Errorf("bad zip")
					}
					f, err := zr.File[0].Open()
					if err != nil {
						return err
					}
					defer f.Close()
					sc := bufio.NewScanner(f)
					sc.Buffer(make([]byte, 4<<20), 4<<20)
					for sc.Scan() {
						cols := strings.Split(sc.Text(), "\t")
						if len(cols) < 24 {
							continue
						}
						// Persons (11), organisations (13), V2 locations (10), all names (23).
						text := cols[11] + " ; " + cols[13] + " ; " + cols[10] + " ; " + cols[23]
						hits = m.Match(text, hits)
						for _, id := range hits {
							mw.add(t.Unix(), "news", keywords[id])
						}
					}
					return sc.Err()
				}()
				mu.Lock()
				done++
				if err != nil {
					failed++
				}
				if done%96 == 0 {
					fmt.Fprintf(os.Stderr, "gdelt %d/%d files (%d failed)\n", done, len(stamps), failed)
				}
				mu.Unlock()
			}
		}()
	}
	for _, t := range stamps {
		jobs <- t
	}
	close(jobs)
	wg.Wait()
	return mw.n, mw.w.Flush()
}

// DefaultSubreddits are where news, politics, markets, crypto and tech get
// discussed in English.
var DefaultSubreddits = []string{
	"worldnews", "news", "politics", "geopolitics", "europe", "ukraine", "Economics", "economy",
	"wallstreetbets", "stocks", "CryptoCurrency", "Bitcoin", "ethereum", "technology", "OpenAI",
	"singularity", "artificial", "Polymarket", "neoliberal", "Conservative",
}

// FetchReddit pages through every post in the given subreddits (via the
// Arctic Shift archive) and records one event per post whose title mentions a
// keyword, at the post's creation time.
func FetchReddit(keywords, subs []string, from, to time.Time, out io.Writer) (int, error) {
	m := keywordMatcher(keywords)
	mw := &mentionWriter{w: bufio.NewWriter(out)}
	client := &http.Client{Timeout: 90 * time.Second}
	var hits []int
	for _, sub := range subs {
		after := from.Unix()
		posts := 0
		for after < to.Unix() {
			u := fmt.Sprintf("https://arctic-shift.photon-reddit.com/api/posts/search?subreddit=%s&after=%d&before=%d&limit=100&sort=asc&fields=created_utc,title",
				url.QueryEscape(sub), after, to.Unix())
			var body struct {
				Data []struct {
					Created int64  `json:"created_utc"`
					Title   string `json:"title"`
				} `json:"data"`
				Error string `json:"error"`
			}
			var err error
			for attempt := 0; attempt < 6; attempt++ {
				var resp *http.Response
				resp, err = client.Get(u)
				if err == nil {
					err = json.NewDecoder(resp.Body).Decode(&body)
					resp.Body.Close()
				}
				if err == nil && body.Error == "" {
					break
				}
				if err == nil {
					err = fmt.Errorf("%s", body.Error)
				}
				time.Sleep(time.Duration(2<<attempt) * time.Second)
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "r/%s: %v (skipping rest)\n", sub, err)
				break
			}
			if len(body.Data) == 0 {
				break
			}
			for _, p := range body.Data {
				hits = m.Match(p.Title, hits)
				for _, id := range hits {
					mw.add(p.Created, "reddit", keywords[id])
				}
			}
			posts += len(body.Data)
			last := body.Data[len(body.Data)-1].Created
			if last <= after {
				last = after + 1
			}
			after = last
			time.Sleep(300 * time.Millisecond)
		}
		fmt.Fprintf(os.Stderr, "r/%-16s %6d posts\n", sub, posts)
	}
	return mw.n, mw.w.Flush()
}

// FetchHN records one event per Hacker News story mentioning a keyword, at
// its creation time, via the Algolia search API.
func FetchHN(keywords []string, from, to time.Time, out io.Writer) (int, error) {
	mw := &mentionWriter{w: bufio.NewWriter(out)}
	client := &http.Client{Timeout: 60 * time.Second}
	m := keywordMatcher(keywords)
	for _, kw := range keywords {
		// Algolia returns at most 1000 hits per query, so walk the window a
		// week at a time.
		for start := from; start.Before(to); start = start.Add(7 * 24 * time.Hour) {
			end := start.Add(7 * 24 * time.Hour)
			if end.After(to) {
				end = to
			}
			u := fmt.Sprintf("https://hn.algolia.com/api/v1/search_by_date?query=%s&tags=story&hitsPerPage=1000&numericFilters=%s",
				url.QueryEscape(`"`+kw+`"`), url.QueryEscape(fmt.Sprintf("created_at_i>=%d,created_at_i<%d", start.Unix(), end.Unix())))
			resp, err := client.Get(u)
			if err != nil {
				return mw.n, err
			}
			var body struct {
				Hits []struct {
					Created int64  `json:"created_at_i"`
					Title   string `json:"title"`
				} `json:"hits"`
			}
			err = json.NewDecoder(resp.Body).Decode(&body)
			resp.Body.Close()
			if err != nil {
				continue
			}
			var hits []int
			for _, h := range body.Hits {
				// Algolia matches loosely; keep only titles that name the keyword.
				hits = m.Match(h.Title, hits)
				for _, id := range hits {
					if keywords[id] == kw {
						mw.add(h.Created, "hn", kw)
					}
				}
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	return mw.n, mw.w.Flush()
}
