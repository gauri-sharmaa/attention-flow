// Command attnflow runs the attention lead-lag and fair-value engine.
//
//	attnflow sim     -out data/sim            simulate a stream with a known answer key
//	attnflow replay  -events E -truth T       replay a stream and print the scorecard
//	attnflow fetch   -out data/wiki           download Wikipedia pageviews (needs network)
//	attnflow serve   -events E                live dashboard, replaying E in real time
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
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
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: attnflow sim|replay|fetch|serve [flags]")
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
	fs.Parse(args)
	u, err := core.LoadUniverse(*uni)
	if err != nil {
		return err
	}
	cfg := sim.Default()
	cfg.Bars, cfg.Seed = *bars, *seed
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
	if barSeconds >= 3600 {
		// Hourly data: far fewer bars, so shorter windows and a looser bar for
		// promotion (the z-score already accounts for the smaller sample).
		cfg.StatHalfLife, cfg.ModelHalfLife, cfg.FactorHalf = 720, 1000, 500
		cfg.MaxLag, cfg.Horizon = 6, 12
		cfg.Warmup, cfg.MinEdgeAge = 336, 240
		cfg.PromoteZ = 4.5
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
	k := fs.Int("k", 12, "semantic neighbours per entity")
	out := fs.String("out", "", "write the report as JSON here")
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
	cand := semantic.Candidates(u, *k, 0.05)
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
		opt.Checkpoints = []int{tr.MidBar - 1, tr.MidBar + tr.MidBar/2, 2*tr.MidBar - 2}
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
	contact := fs.String("contact", "", "contact email for the Wikimedia User-Agent (required by their API policy)")
	fs.Parse(args)
	u, err := core.LoadUniverse(*uni)
	if err != nil {
		return err
	}
	end := time.Now().UTC().Truncate(time.Hour)
	return source.FetchWikipedia(u, *out, end.AddDate(0, 0, -*days), end, *contact)
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	uni := fs.String("universe", "data/universe.txt", "universe file")
	text := fs.String("text", "", "optional entity descriptions")
	events := fs.String("events", "data/sim/events.csv", "events CSV to replay")
	bar := fs.Int64("bar", 60, "bar width in seconds")
	speed := fs.Float64("speed", 20, "bars per second to replay")
	addr := fs.String("addr", ":8080", "listen address")
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
	e := engine.New(u, semantic.Candidates(u, 12, 0.05), cfg)
	return server.Serve(*addr, e, evs, *speed, *label)
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
