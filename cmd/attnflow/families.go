package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gauri-sharmaa/attention-flow/internal/source"
)

// A family is one busy market (the leader) and the quiet markets that should
// react to it (followers). For now families come from shared events: every
// other outcome of "Who will win X" or "Fed decision in December" reacts when
// one outcome jumps, and the smaller ones trade far less often.

type family struct {
	Event     string            `json:"event"`
	Slug      string            `json:"slug"`
	Topic     string            `json:"topic"`
	Leader    source.PolyMeta   `json:"leader"`
	Followers []source.PolyMeta `json:"followers"`
}

// cmdFamilies builds families from the busiest markets of the year and
// downloads the followers' trade history.
func cmdFamilies(args []string) error {
	fs := flag.NewFlagSet("families", flag.ExitOnError)
	dir := fs.String("dir", "data/pmt365", "polytrades output (cache/ and meta.json)")
	ratio := fs.Float64("quiet", 5, "a follower traded at most 1/this of the leader's volume")
	minVol := fs.Float64("min-volume", 2000, "followers need at least this lifetime volume ($) to have any trades to study")
	maxF := fs.Int("max-followers", 8, "followers kept per family (the busiest of the quiet ones)")
	days := fs.Int("days", 365, "history to download")
	rebuild := fs.Bool("rebuild", false, "rebuild families.json even if it exists")
	fs.Parse(args)
	var fams []family
	if b, err := os.ReadFile(filepath.Join(*dir, "families.json")); err == nil && !*rebuild && json.Unmarshal(b, &fams) == nil && len(fams) > 0 {
		fmt.Fprintf(os.Stderr, "using the %d families in families.json\n", len(fams))
		return downloadFollowers(*dir, fams, *days)
	}

	files, _ := filepath.Glob(filepath.Join(*dir, "cache", "*.json"))
	cached := map[string]bool{}
	for _, f := range files {
		base := filepath.Base(f)
		if i := strings.LastIndex(base, "-"); i > 0 {
			cached[base[:i]] = true
		}
	}
	ids := make([]string, 0, len(cached))
	for id := range cached {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	meta, err := loadMeta(filepath.Join(*dir, "meta.json"), ids)
	if err != nil {
		return err
	}
	// Older metadata lacks the event slug: look those up again.
	var noSlug []string
	for _, id := range ids {
		if m, ok := meta[id]; ok && m.EventSlug == "" {
			noSlug = append(noSlug, id)
		}
	}
	if len(noSlug) > 0 {
		fmt.Fprintf(os.Stderr, "fetching event slugs for %d markets…\n", len(noSlug))
		got, err := source.PolyMetaByID(noSlug)
		if err != nil {
			return err
		}
		for k, v := range got {
			meta[k] = v
		}
		if b, err := json.Marshal(meta); err == nil {
			os.WriteFile(filepath.Join(*dir, "meta.json"), b, 0o644)
		}
	}

	bySlug := map[string]bool{}
	for _, id := range ids {
		if m := meta[id]; m.EventSlug != "" {
			bySlug[m.EventSlug] = true
		}
	}
	slugs := make([]string, 0, len(bySlug))
	for s := range bySlug {
		slugs = append(slugs, s)
	}
	sort.Strings(slugs)
	for i, slug := range slugs {
		ms, err := source.PolyEventMarkets(slug)
		if err != nil || len(ms) < 2 {
			continue
		}
		var leader *source.PolyMeta
		for k := range ms {
			if cached[ms[k].ConditionID] && len(ms[k].Tokens) == 2 && (leader == nil || ms[k].Volume > leader.Volume) {
				leader = &ms[k]
			}
		}
		if leader == nil {
			continue
		}
		f := family{Event: leader.Event, Slug: slug, Leader: *leader, Topic: source.SectorOf(leader.Question + " " + leader.Event)}
		if vsRe.MatchString(leader.Question) {
			f.Topic = "sports"
		}
		for _, m := range ms {
			if m.ConditionID != leader.ConditionID && len(m.Tokens) == 2 && m.Volume >= *minVol && m.Volume <= leader.Volume / *ratio {
				f.Followers = append(f.Followers, m)
			}
		}
		sort.Slice(f.Followers, func(a, b int) bool { return f.Followers[a].Volume > f.Followers[b].Volume })
		if len(f.Followers) > *maxF {
			f.Followers = f.Followers[:*maxF]
		}
		if len(f.Followers) > 0 {
			fams = append(fams, f)
		}
		if i%50 == 0 {
			fmt.Fprintf(os.Stderr, "events %d/%d · %d families\n", i, len(slugs), len(fams))
		}
		time.Sleep(80 * time.Millisecond)
	}
	b, _ := json.MarshalIndent(fams, "", " ")
	if err := os.WriteFile(filepath.Join(*dir, "families.json"), b, 0o644); err != nil {
		return err
	}
	nf := 0
	for _, f := range fams {
		nf += len(f.Followers)
	}
	fmt.Printf("%d families, %d quiet followers\n", len(fams), nf)
	return downloadFollowers(*dir, fams, *days)
}

// downloadFollowers fetches every follower's trades into the shared cache
// (cached markets are skipped, so an interrupted run picks up where it left off).
func downloadFollowers(dir string, fams []family, days int) error {
	nf := 0
	for _, f := range fams {
		nf += len(f.Followers)
	}
	from := time.Now().AddDate(0, 0, -days).Unix()
	done := 0
	for _, f := range fams {
		for _, m := range f.Followers {
			if _, err := source.MarketTradeHistory(filepath.Join(dir, "cache"), m.ConditionID, from); err != nil {
				fmt.Fprintln(os.Stderr, "trades:", m.ConditionID, err)
			}
			if done++; done%100 == 0 {
				fmt.Fprintf(os.Stderr, "follower trades %d/%d\n", done, nf)
			}
		}
	}
	return nil
}
