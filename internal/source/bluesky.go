package source

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/coder/websocket"

	"github.com/gauri-sharmaa/attention-flow/internal/core"
)

// Jetstream is Bluesky's public JSON firehose: every new post, in real time.
// Instances drop connections now and then, so the collector rotates through
// them and resumes from the last post it saw (the cursor replays a short
// buffer on the server side).
var jetstreams = []string{
	"wss://jetstream2.us-east.bsky.network",
	"wss://jetstream1.us-east.bsky.network",
	"wss://jetstream1.us-west.bsky.network",
	"wss://jetstream2.us-west.bsky.network",
}

// Matcher finds which entities a post mentions. Names match as whole words
// (or whole phrases for multi-word names) and are case-sensitive, so "Apple"
// counts but "apple pie" does not, and "Base" is not every use of "base".
type Matcher struct {
	single map[string][]int    // one-word name -> entities
	multi  map[string][][2]any // first word -> (remaining words, entity)
}

// NewMatcher indexes the universe's entity names.
func NewMatcher(u *core.Universe) *Matcher {
	m := &Matcher{single: map[string][]int{}, multi: map[string][][2]any{}}
	for _, e := range u.Entities {
		w := words(e.Name)
		if len(w) == 0 {
			continue
		}
		if len(w) == 1 {
			m.single[w[0]] = append(m.single[w[0]], e.ID)
		} else {
			m.multi[w[0]] = append(m.multi[w[0]], [2]any{w[1:], e.ID})
		}
	}
	return m
}

func words(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '.' && r != '-'
	})
}

// Match returns the distinct entities mentioned in text.
func (m *Matcher) Match(text string, out []int) []int {
	out = out[:0]
	w := words(text)
	for i := range w {
		t := strings.TrimRight(w[i], ".-")
		for _, id := range m.single[t] {
			out = appendUnique(out, id)
		}
		for _, cand := range m.multi[t] {
			rest, id := cand[0].([]string), cand[1].(int)
			if i+len(rest) >= len(w) {
				continue // phrase would run past the end of the post
			}
			ok := true
			for k, r := range rest {
				if strings.TrimRight(w[i+1+k], ".-") != r {
					ok = false
					break
				}
			}
			if ok {
				out = appendUnique(out, id)
			}
		}
	}
	return out
}

func appendUnique(xs []int, x int) []int {
	for _, y := range xs {
		if y == x {
			return xs
		}
	}
	return append(xs, x)
}

type jetMsg struct {
	TimeUS int64  `json:"time_us"`
	Kind   string `json:"kind"`
	Commit struct {
		Operation string `json:"operation"`
		Record    struct {
			Text string `json:"text"`
		} `json:"record"`
	} `json:"commit"`
}

// CollectBluesky streams the firehose and appends one event per entity per
// bar (mention count + 1) to outPath until ctx ends. It reconnects on errors.
func CollectBluesky(ctx context.Context, u *core.Universe, outPath string, bar time.Duration, emit func([]core.Event)) error {
	f, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if st, _ := f.Stat(); st.Size() == 0 {
		fmt.Fprintln(f, "ts,entity,value")
	}
	m := NewMatcher(u)
	counts := make([]int, len(u.Entities))
	cur := time.Now().Truncate(bar)
	var posts, matched int
	flush := func(upTo time.Time) {
		for cur.Before(upTo) {
			evs := make([]core.Event, len(counts))
			for j, c := range counts {
				evs[j] = core.Event{TS: cur.Unix(), Entity: int32(j), Value: float64(c + 1)}
				fmt.Fprintf(f, "%d,%d,%d\n", cur.Unix(), j, c+1)
				counts[j] = 0
			}
			if emit != nil {
				emit(evs)
			}
			fmt.Fprintf(os.Stderr, "%s  %d posts, %d mentions\n", cur.Format("15:04"), posts, matched)
			posts, matched = 0, 0
			cur = cur.Add(bar)
		}
	}
	var hits []int
	var lastUS int64
	for attempt := 0; ctx.Err() == nil; attempt++ {
		u := jetstreams[attempt%len(jetstreams)] + "/subscribe?wantedCollections=app.bsky.feed.post"
		if lastUS > 0 {
			u += fmt.Sprintf("&cursor=%d", lastUS)
		}
		c, _, err := websocket.Dial(ctx, u, nil)
		if err != nil {
			fmt.Fprintln(os.Stderr, "connect:", err)
			time.Sleep(2 * time.Second)
			continue
		}
		c.SetReadLimit(1 << 20)
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				fmt.Fprintln(os.Stderr, "read:", err)
				break
			}
			var msg jetMsg
			if json.Unmarshal(data, &msg) != nil || msg.Kind != "commit" || msg.Commit.Operation != "create" {
				continue
			}
			if msg.TimeUS <= lastUS {
				continue // replayed by the cursor after a reconnect
			}
			lastUS = msg.TimeUS
			ts := time.UnixMicro(msg.TimeUS)
			if ts.Before(cur) {
				continue
			}
			if !ts.Before(cur.Add(bar)) {
				flush(ts.Truncate(bar))
			}
			posts++
			hits = m.Match(msg.Commit.Record.Text, hits)
			for _, id := range hits {
				counts[id]++
				matched++
			}
		}
		c.CloseNow()
	}
	return ctx.Err()
}
