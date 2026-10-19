package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"time"

	"github.com/gauri-sharmaa/attention-flow/internal/rewards"
	"github.com/gauri-sharmaa/attention-flow/internal/source"
)

// cmdRewardSim runs the liquidity-rewards paper test (see rewards.Sim) on
// the reward markets that looked best from their order books at the start:
// no live games (a lone quote in a game in progress is the one that gets run
// over), at least three days from ending, and positive in the estimate even
// after exiting every fill as a taker. The chosen markets are saved, so a
// restart resumes the same test.
func cmdRewardSim(args []string) error {
	fs := flag.NewFlagSet("rewardsim", flag.ExitOnError)
	dir := fs.String("dir", "data/rewardsim", "where the test keeps its state and logs")
	n := fs.Int("markets", 40, "markets to follow")
	top := fs.Int("top", 400, "reward pools to consider")
	dur := fs.Duration("for", 24*time.Hour, "how long the test runs in all (a resumed test counts from its start)")
	maxInv := fs.Float64("max-inventory", 5, "most shares held either way, as a multiple of the order size")
	fs.Parse(args)
	os.MkdirAll(*dir, 0o755)
	path := filepath.Join(*dir, "markets.json")
	var ms []rewards.SimMarket
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, &ms)
		fmt.Fprintf(os.Stderr, "resuming with the %d markets chosen at the start\n", len(ms))
	} else {
		rows, _, _, err := estimateRewards(*top)
		if err != nil {
			return err
		}
		sort.Slice(rows, func(a, b int) bool {
			return rows[a].earn-rows[a].fills*rows[a].lossExit > rows[b].earn-rows[b].fills*rows[b].lossExit
		})
		for _, r := range rows {
			if len(ms) >= *n {
				break
			}
			if r.sector == "sports" || r.meta.End.Before(time.Now().Add(72*time.Hour)) || r.mid < 0.05 || r.mid > 0.95 ||
				r.earn-r.fills*r.lossExit <= 0 {
				continue
			}
			ms = append(ms, rewards.SimMarket{
				LiveMarket: source.LiveMarket{ConditionID: r.meta.ConditionID, Question: r.meta.Question, Event: r.meta.Event, Sector: r.sector,
					YesToken: r.meta.Tokens[0], NoToken: r.meta.Tokens[1], FeeRate: r.meta.FeeRate},
				Daily: r.rm.Daily, MaxSpread: r.rm.MaxSpread, MinSize: r.rm.MinSize, End: r.meta.End})
		}
		b, _ := json.MarshalIndent(ms, "", " ")
		if err := os.WriteFile(path, b, 0o644); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "chose %d markets\n", len(ms))
	}
	if len(ms) == 0 {
		return fmt.Errorf("no markets qualified")
	}
	sim := rewards.NewSim(*dir, ms, *maxInv)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	end := sim.Started().Add(*dur)
	if time.Now().After(end) {
		fmt.Print(sim.Report())
		return nil
	}
	ctx, cancel := context.WithDeadline(ctx, end)
	defer cancel()
	go func() {
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				os.WriteFile(filepath.Join(*dir, "report.txt"), []byte(sim.Report()), 0o644)
			}
		}
	}()
	sim.Run(ctx)
	report := sim.Report()
	os.WriteFile(filepath.Join(*dir, "report.txt"), []byte(report), 0o644)
	fmt.Print(report)
	return nil
}
