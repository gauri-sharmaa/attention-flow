// Command attnflow runs the attention lead-lag and fair-value engine.
//
//	attnflow sim     -out data/sim            simulate a stream with a known answer key
//	attnflow replay  -events E -truth T       replay a stream and print the scorecard
//	attnflow fetch   -out data/wiki           download Wikipedia pageviews (needs network)
//	attnflow serve                            live Polymarket dashboard at localhost:8080 (no keys needed)
//	attnflow serve -sim                       replay the simulator instead
//	attnflow export  -events E -out site      static dashboard (no server) for hosting
//	attnflow markets -out data/poly           Polymarket prices for markets about each topic
//	attnflow bluesky -out data/bsky           record live Bluesky mentions per minute
//	attnflow polyall -out data/pm             every busy Polymarket market, 5-minute prices
//	attnflow polytrades -out data/pmt         per market: trading activity + price from its trade log
//	attnflow hawkes -dir data/pmt             does trading in one market excite related markets? (event time)
//	attnflow leadlag -dir data/pmt90          which prices move first (Hoffmann-Rosenbaum-Yoshida), period by period
//	attnflow backtest -dir data/pmt90         paper-trade the order-flow signal, walk-forward, with costs
//	attnflow eventstudy -dir data/pmt         when outside buzz surges, does trading follow? (model-free)
//	attnflow outside -dir data/pmt            news, Reddit and Hacker News mentions of the markets' subjects
//	attnflow resample -events E -bar 3600     sum mention counts into wider bars
package main

import (
	"context"
	"log"

	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/gauri-sharmaa/attention-flow/internal/live"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gauri-sharmaa/attention-flow/internal/core"
	"github.com/gauri-sharmaa/attention-flow/internal/engine"
	"github.com/gauri-sharmaa/attention-flow/internal/replay"
	"github.com/gauri-sharmaa/attention-flow/internal/semantic"
	"github.com/gauri-sharmaa/attention-flow/internal/server"
	"github.com/gauri-sharmaa/attention-flow/internal/sim"
	"github.com/gauri-sharmaa/attention-flow/internal/source"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "sim":
		err = cmdSim(os.Args[2:])
	case "replay":
		err = cmdReplay(os.Args[2:])
	case "fetch":
		err = cmdFetch(os.Args[2:])
	case "serve":
		err = cmdServe(os.Args[2:])
	case "export":
		err = cmdExport(os.Args[2:])
	case "markets":
		err = cmdMarkets(os.Args[2:])
	case "bluesky":
		err = cmdBluesky(os.Args[2:])
	case "polyall":
		err = cmdPolyAll(os.Args[2:])
	case "polytrades":
		err = cmdPolyTrades(os.Args[2:])
	case "hawkes":
		err = cmdHawkes(os.Args[2:])
	case "leadlag":
		err = cmdLeadLag(os.Args[2:])
	case "backtest":
		err = cmdBacktest(os.Args[2:])
	case "eventstudy":
		err = cmdEventStudy(os.Args[2:])
	case "outside":
		err = cmdOutside(os.Args[2:])
	case "resample":
		err = cmdResample(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: attnflow sim|replay|fetch|markets|polyall|polytrades|hawkes|leadlag|backtest|eventstudy|outside|bluesky|resample|serve|export [flags]")
	os.Exit(2)
}

func loadUniverse(path, textPath string) (*core.Universe, error) {
	u, err := core.LoadUniverse(path)
	if err != nil {
		return nil, err
	}
	if textPath != "" {
		if err := source.AttachText(u, textPath); err != nil {
			return nil, err
		}
	}
	return u, nil
}

func cmdSim(args []string) error {
	fs := flag.NewFlagSet("sim", flag.ExitOnError)
	uni := fs.String("universe", "data/universe.txt", "universe file")
	out := fs.String("out", "data/sim", "output directory")
	bars := fs.Int("bars", 20000, "bars to simulate")
	seed := fs.Uint64("seed", 7, "random seed")
	bar := fs.Int64("bar", 60, "bar width in seconds")
	fs.Parse(args)
	u, err := core.LoadUniverse(*uni)
	if err != nil {
		return err
	}
	cfg := sim.Default()
	cfg.Bars, cfg.Seed, cfg.BarSeconds = *bars, *seed, *bar
	evs, truth := sim.Run(u, cfg)
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	f, err := os.Create(filepath.Join(*out, "events.csv"))
	if err != nil {
		return err
	}
	if err := core.WriteEvents(f, evs); err != nil {
		return err
	}
	f.Close()
	if err := writeJSON(filepath.Join(*out, "truth.json"), truth); err != nil {
		return err
	}
	fmt.Printf("%d events, %d planted edges, %d shocks -> %s\n", len(evs), len(truth.Edges), len(truth.Shocks), *out)
	return nil
}

func engineConfig(barSeconds int64) engine.Config {
	cfg := engine.DefaultConfig(barSeconds)
	if barSeconds >= 300 && barSeconds < 3600 {
		// 5-minute bars: lags up to an hour, forecasts an hour ahead, windows
		// of about a week. Prediction-market prices have no daily cycle.
		cfg.MaxLag, cfg.Horizon = 12, 12
		cfg.StatHalfLife, cfg.ModelHalfLife, cfg.FactorHalf = 2000, 2000, 1000
		cfg.Warmup, cfg.MinEdgeAge = 1000, 600
	}
	if barSeconds >= 3600 {
		// Hourly data: far fewer bars, so shorter windows. The promotion
		// z-score already accounts for the smaller sample.
		cfg.StatHalfLife, cfg.ModelHalfLife, cfg.FactorHalf = 720, 1000, 500
		cfg.MaxLag, cfg.Horizon = 6, 12
		cfg.Warmup, cfg.MinEdgeAge = 336, 240
		cfg.Period = 24
	}
	return cfg
}

func cmdReplay(args []string) error {
	fs := flag.NewFlagSet("replay", flag.ExitOnError)
	uni := fs.String("universe", "data/universe.txt", "universe file")
	text := fs.String("text", "", "optional entity descriptions (JSON from fetch)")
	events := fs.String("events", "data/sim/events.csv", "events CSV")
	truthPath := fs.String("truth", "", "answer key from sim (optional)")
	bar := fs.Int64("bar", 60, "bar width in seconds")
	k := fs.Int("k", 40, "semantic neighbours per entity")
	minSim := fs.Float64("minsim", 0.05, "minimum semantic similarity for a candidate pair")
	out := fs.String("out", "", "write the report as JSON here")
	promote := fs.Float64("promote", 0, "override the edge promotion z-score")
	statHL := fs.Float64("stathl", 0, "override the lead-lag statistics half-life (bars)")
	placebo := fs.String("placebo", "", "cluster:hours — circularly shift that cluster's series in time to measure false links")
	drop := fs.String("drop", "", "drop all events of entities whose cluster contains this text (ablation)")
	smooth := fs.String("smooth", "", "text:halflife — treat entities whose cluster contains text as counts and smooth them into burst intensity")
	fs.Parse(args)

	u, err := loadUniverse(*uni, *text)
	if err != nil {
		return err
	}
	evs, err := core.LoadEvents(*events)
	if err != nil {
		return err
	}
	if *placebo != "" {
		if evs, err = shiftCluster(u, evs, *placebo); err != nil {
			return err
		}
	}
	if *smooth != "" {
		text, hl, _ := strings.Cut(*smooth, ":")
		h, err := strconv.ParseFloat(hl, 64)
		if err != nil {
			return fmt.Errorf("-smooth wants text:halflife, got %q", *smooth)
		}
		evs = core.SmoothCounts(evs, clusterMatch(u, text), h)
	}
	if *drop != "" {
		m := clusterMatch(u, *drop)
		kept := evs[:0]
		for _, e := range evs {
			if !m[e.Entity] {
				kept = append(kept, e)
			}
		}
		evs = kept
	}
	cfg := engineConfig(*bar)
	if *promote > 0 {
		cfg.PromoteZ = *promote
	}
	if *statHL > 0 {
		cfg.StatHalfLife = *statHL
	}
	cand := semantic.Candidates(u, *k, *minSim)
	opt := replay.Options{EvalStart: cfg.Warmup * 2}
	if *truthPath != "" {
		var tr sim.Truth
		b, err := os.ReadFile(*truthPath)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(b, &tr); err != nil {
			return err
		}
		opt.Truth = &tr
		m := tr.MidBar
		opt.Checkpoints = []int{m - 1, m + 250, m + 500, m + 1000, m + 2000, m + 4000, 2*m - 2}
	}
	start := time.Now()
	rep := replay.Run(u, evs, cand, cfg, opt)
	fmt.Print(replay.Format(rep))
	fmt.Printf("\n(replayed in %.1fs)\n", time.Since(start).Seconds())
	if *out != "" {
		return writeJSON(*out, rep)
	}
	return nil
}

func cmdFetch(args []string) error {
	fs := flag.NewFlagSet("fetch", flag.ExitOnError)
	uni := fs.String("universe", "data/universe.txt", "universe file")
	out := fs.String("out", "data/wiki", "output directory")
	days := fs.Int("days", 90, "days of hourly history")
	contact := fs.String("contact", "https://github.com/gauri-sharmaa/attention-flow", "contact URL or email for the Wikimedia User-Agent (required by their API policy)")
	gran := fs.String("granularity", "hourly", "hourly or daily (use -bar 86400 when replaying daily data)")
	dumps := fs.Bool("dumps", false, "build hourly data from raw dump files (no API rate limits, ~55 MB per hour downloaded)")
	workers := fs.Int("workers", 6, "parallel downloads with -dumps")
	ago := fs.Int("ago", 0, "end the window this many days ago (to backfill older history)")
	fs.Parse(args)
	u, err := core.LoadUniverse(*uni)
	if err != nil {
		return err
	}
	end := time.Now().UTC().Truncate(time.Hour).Add(-3 * time.Hour) // dumps lag a couple of hours
	end = end.AddDate(0, 0, -*ago)
	if *dumps {
		if *contact == "" {
			return fmt.Errorf("pass -contact <url or email>")
		}
		return source.FetchWikiDumps(u, *out, end.AddDate(0, 0, -*days), end, *workers, *contact)
	}
	return source.FetchWikipedia(u, *out, end.AddDate(0, 0, -*days), end, *gran, *contact)
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":8080", "listen address")
	n := fs.Int("markets", 200, "how many of the busiest Polymarket markets to follow")
	hours := fs.Int("hours", 48, "hours of trade history the model is fitted on")
	sim := fs.Bool("sim", false, "replay the simulator instead of live Polymarket")
	uni := fs.String("universe", "data/universe.txt", "universe file (-sim)")
	text := fs.String("text", "", "optional entity descriptions (-sim)")
	events := fs.String("events", "data/sim/events.csv", "events CSV to replay (-sim)")
	bar := fs.Int64("bar", 60, "bar width in seconds (-sim)")
	speed := fs.Float64("speed", 20, "bars per second to replay (-sim)")
	label := fs.String("label", "simulated", "data label shown in the UI (-sim)")
	fs.Parse(args)
	if !*sim {
		cfg := live.DefaultConfig()
		cfg.Markets, cfg.History = *n, time.Duration(*hours)*time.Hour
		eng := live.New(cfg)
		go func() {
			if err := eng.Run(); err != nil {
				log.Fatal(err)
			}
		}()
		return server.ServeLive(*addr, eng)
	}
	u, err := loadUniverse(*uni, *text)
	if err != nil {
		return err
	}
	evs, err := core.LoadEvents(*events)
	if err != nil {
		return err
	}
	cfg := engineConfig(*bar)
	cand := semantic.Candidates(u, 40, 0.05)
	return server.Serve(u, evs, server.Options{
		Addr: *addr, Speed: *speed, Label: *label, SkipBars: 2 * cfg.Warmup,
		NewEngine: func() *engine.Engine { return engine.New(u, cand, cfg) },
	})
}

func cmdExport(args []string) error {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	uni := fs.String("universe", "data/universe.txt", "universe file")
	text := fs.String("text", "", "optional entity descriptions")
	events := fs.String("events", "data/sim/events.csv", "events CSV")
	bar := fs.Int64("bar", 60, "bar width in seconds")
	out := fs.String("out", "site", "output directory")
	every := fs.Int("every", 3, "bars between frames")
	frames := fs.Int("frames", 400, "maximum frames")
	label := fs.String("label", "simulated", "data label shown in the UI")
	fs.Parse(args)
	u, err := loadUniverse(*uni, *text)
	if err != nil {
		return err
	}
	evs, err := core.LoadEvents(*events)
	if err != nil {
		return err
	}
	cfg := engineConfig(*bar)
	e := engine.New(u, semantic.Candidates(u, 40, 0.05), cfg)
	n, err := server.Export(u, evs, e, *out, *label, 3*cfg.Warmup, *every, *frames)
	if err != nil {
		return err
	}
	fmt.Printf("%d frames -> %s\n", n, *out)
	return nil
}

func cmdMarkets(args []string) error {
	fs := flag.NewFlagSet("markets", flag.ExitOnError)
	uni := fs.String("universe", "data/universe.txt", "universe file")
	out := fs.String("out", "data/poly", "output directory")
	days := fs.Int("days", 30, "days of hourly history")
	per := fs.Int("per", 1, "markets per topic")
	minVol := fs.Float64("minvol", 50000, "minimum market volume (USD)")
	fs.Parse(args)
	u, err := core.LoadUniverse(*uni)
	if err != nil {
		return err
	}
	return source.FetchPolymarket(u, *uni, *out, time.Now().AddDate(0, 0, -*days), *per, *minVol)
}

func cmdPolyAll(args []string) error {
	fs := flag.NewFlagSet("polyall", flag.ExitOnError)
	out := fs.String("out", "data/pm", "output directory")
	n := fs.Int("n", 400, "how many of the busiest tradable markets")
	days := fs.Int("days", 30, "days of history")
	bar := fs.Int64("bar", 300, "bar width in seconds (60 = 1-minute prices)")
	workers := fs.Int("workers", 6, "parallel downloads")
	fs.Parse(args)
	return source.FetchAllMarkets(*out, *n, *days, *bar, *workers)
}

func cmdPolyTrades(args []string) error {
	fs := flag.NewFlagSet("polytrades", flag.ExitOnError)
	out := fs.String("out", "data/pmt", "output directory")
	n := fs.Int("n", 400, "how many of the busiest tradable markets")
	closed := fs.Int("closed", 0, "also include this many of the biggest markets that resolved during the window")
	days := fs.Int("days", 30, "days of history")
	bar := fs.Int64("bar", 300, "bar width in seconds")
	workers := fs.Int("workers", 6, "parallel downloads")
	bars := fs.Bool("bars", true, "also write bar series (events.csv) for the streaming engine")
	fs.Parse(args)
	return source.FetchMarketTrades(*out, *n, *closed, *days, *bar, *workers, *bars)
}

func cmdOutside(args []string) error {
	fs := flag.NewFlagSet("outside", flag.ExitOnError)
	dir := fs.String("dir", "data/pmt", "polytrades output (markets.txt, ticks.csv)")
	maxKW := fs.Int("keywords", 120, "how many names to track")
	workers := fs.Int("workers", 8, "parallel GDELT downloads")
	srcs := fs.String("sources", "news,reddit,hn", "which sources to fetch")
	fs.Parse(args)
	b, err := os.ReadFile(filepath.Join(*dir, "markets.txt"))
	if err != nil {
		return err
	}
	kws := source.Keywords(strings.Split(strings.TrimSpace(string(b)), "\n"), *maxKW)
	if err := os.WriteFile(filepath.Join(*dir, "keywords.txt"), []byte(strings.Join(kws, "\n")+"\n"), 0o644); err != nil {
		return err
	}
	// Same window as the trades.
	evs, err := core.LoadEvents(filepath.Join(*dir, "events.csv"))
	if err != nil {
		return err
	}
	from, to := time.Unix(evs[0].TS, 0).UTC(), time.Unix(evs[len(evs)-1].TS, 0).UTC()
	fmt.Fprintf(os.Stderr, "%d keywords · %s to %s\n", len(kws), from.Format("Jan 2"), to.Format("Jan 2"))
	type job struct {
		name string
		run  func(io.Writer) (int, error)
	}
	all := map[string]job{
		"news":   {"news", func(w io.Writer) (int, error) { return source.FetchGDELT(kws, from, to, w, *workers) }},
		"reddit": {"reddit", func(w io.Writer) (int, error) { return source.FetchReddit(kws, source.DefaultSubreddits, from, to, w) }},
		"hn":     {"hn", func(w io.Writer) (int, error) { return source.FetchHN(kws, from, to, w) }},
	}
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for _, s := range strings.Split(*srcs, ",") {
		j, ok := all[s]
		if !ok {
			return fmt.Errorf("unknown source %q", s)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			f, err := os.Create(filepath.Join(*dir, "outside-"+j.name+".csv"))
			if err != nil {
				errs <- err
				return
			}
			defer f.Close()
			fmt.Fprintln(f, "ts,source,keyword")
			n, err := j.run(f)
			fmt.Fprintf(os.Stderr, "%s: %d mentions\n", j.name, n)
			if err != nil {
				errs <- fmt.Errorf("%s: %w", j.name, err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	return <-errs
}

func cmdBluesky(args []string) error {
	fs := flag.NewFlagSet("bluesky", flag.ExitOnError)
	uni := fs.String("universe", "data/universe.txt", "universe file")
	out := fs.String("out", "data/bsky", "output directory")
	dur := fs.Duration("for", 0, "stop after this long (0 = run until killed)")
	fs.Parse(args)
	u, err := core.LoadUniverse(*uni)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *dur > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *dur)
		defer cancel()
	}
	err = source.CollectBluesky(ctx, u, filepath.Join(*out, "events.csv"), time.Minute, nil)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	return err
}

func cmdResample(args []string) error {
	fs := flag.NewFlagSet("resample", flag.ExitOnError)
	events := fs.String("events", "data/bsky/events.csv", "count events CSV")
	bar := fs.Int64("bar", 3600, "new bar width in seconds")
	out := fs.String("out", "", "output CSV (required)")
	offset := fs.Int("offset", 0, "add this to every entity id (to merge with a larger universe)")
	fs.Parse(args)
	if *out == "" {
		return fmt.Errorf("pass -out")
	}
	evs, err := core.LoadEvents(*events)
	if err != nil {
		return err
	}
	rs := core.ResampleCounts(evs, *bar)
	for i := range rs {
		rs[i].Entity += int32(*offset)
	}
	f, err := os.Create(*out)
	if err != nil {
		return err
	}
	defer f.Close()
	return core.WriteEvents(f, rs)
}

func clusterMatch(u *core.Universe, text string) map[int32]bool {
	m := map[int32]bool{}
	for _, e := range u.Entities {
		if strings.Contains(e.Cluster, text) {
			m[int32(e.ID)] = true
		}
	}
	return m
}

// shiftCluster circularly shifts every series in one cluster by a fixed number
// of hours, keeping each series' own dynamics but destroying any real timing
// relationship with the other clusters. Links the engine still finds between
// the shifted cluster and the rest are false discoveries.
func shiftCluster(u *core.Universe, evs []core.Event, spec string) ([]core.Event, error) {
	name, h, ok := strings.Cut(spec, ":")
	hours, err := strconv.Atoi(h)
	if !ok || err != nil {
		return nil, fmt.Errorf("-placebo wants cluster:hours, got %q", spec)
	}
	in := map[int32]bool{}
	for _, e := range u.Entities {
		if e.Cluster == name {
			in[int32(e.ID)] = true
		}
	}
	if len(in) == 0 {
		return nil, fmt.Errorf("no entities in cluster %q", name)
	}
	lo, hi := evs[0].TS, evs[len(evs)-1].TS
	span, shift := hi-lo+1, int64(hours)*3600
	out := make([]core.Event, len(evs))
	for i, e := range evs {
		if in[e.Entity] {
			e.TS = lo + (e.TS-lo+shift)%span
		}
		out[i] = e
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TS < out[j].TS })
	return out, nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
