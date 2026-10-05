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
const jetstream = "wss://jetstream2.us-east.bsky.network/subscribe?wantedCollections=app.bsky.feed.post"

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
	for ctx.Err() == nil {
		c, _, err := websocket.Dial(ctx, jetstream, nil)
		if err != nil {
			fmt.Fprintln(os.Stderr, "connect:", err)
			time.Sleep(5 * time.Second)
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
			ts := time.UnixMicro(msg.TimeUS)
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
