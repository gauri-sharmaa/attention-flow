package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

// cmdFamLLM asks Claude to read each family (a busy market and its quieter
// relatives) and label every follower: is it really related to the leader,
// should it move the same way or the opposite way when the leader jumps,
// what topic it is, and whether insiders could know its answer early.
// Word overlap can't tell "wins the nomination" from "wins the election";
// a language model reading the questions can.
//
// Needs ANTHROPIC_API_KEY (or another credential the SDK finds). Results go
// to families_llm.json, keyed by follower condition ID; families already
// labelled are skipped, so the command can be re-run after an interruption.
func cmdFamLLM(args []string) error {
	fs := flag.NewFlagSet("famllm", flag.ExitOnError)
	dir := fs.String("dir", "data/pmt365", "families.json lives here")
	model := fs.String("model", "claude-opus-5-5", "Claude model")
	limit := fs.Int("limit", 0, "label at most this many families (0: all)")
	fs.Parse(args)
	b, err := os.ReadFile(filepath.Join(*dir, "families.json"))
	if err != nil {
		return err
	}
	var fams []family
	if err := json.Unmarshal(b, &fams); err != nil {
		return err
	}
	outPath := filepath.Join(*dir, "families_llm.json")
	labels := map[string]followerLabel{}
	if b, err := os.ReadFile(outPath); err == nil {
		json.Unmarshal(b, &labels)
	}

	if os.Getenv("ANTHROPIC_API_KEY") == "" && os.Getenv("ANTHROPIC_AUTH_TOKEN") == "" {
		return fmt.Errorf("no Claude API credentials: set ANTHROPIC_API_KEY and run again")
	}
	client := anthropic.NewClient()
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"followers": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id":           map[string]any{"type": "string"},
						"related":      map[string]any{"type": "boolean"},
						"direction":    map[string]any{"type": "string", "enum": []string{"same", "opposite", "unclear"}},
						"topic":        map[string]any{"type": "string", "enum": topics},
						"insider_risk": map[string]any{"type": "string", "enum": []string{"low", "medium", "high"}},
						"release":      map[string]any{"type": "string"},
					},
					"required":             []string{"id", "related", "direction", "topic", "insider_risk", "release"},
					"additionalProperties": false,
				},
			},
		},
		"required":             []string{"followers"},
		"additionalProperties": false,
	}
	done := 0
	for _, f := range fams {
		if *limit > 0 && done >= *limit {
			break
		}
		todo := false
		for _, m := range f.Followers {
			if _, ok := labels[m.ConditionID]; !ok {
				todo = true
			}
		}
		if !todo {
			continue
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "Event: %s\nLeader (busy market): %s\n\nFollowers (quieter markets):\n", f.Event, f.Leader.Question)
		for _, m := range f.Followers {
			fmt.Fprintf(&sb, "- id=%s: %s\n", m.ConditionID, m.Question)
		}
		resp, err := client.Beta.Messages.New(context.Background(), anthropic.BetaMessageNewParams{
			Model:     *model,
			MaxTokens: 4000,
			Betas:     []anthropic.AnthropicBeta{"server-side-fallback-2026-07-01"},
			Fallbacks: anthropic.BetaFallbacksParamOfDefault(),
			OutputConfig: anthropic.BetaOutputConfigParam{
				Effort: anthropic.BetaOutputConfigEffortLow,
				Format: anthropic.BetaJSONOutputFormatParam{Schema: schema},
			},
			System: []anthropic.BetaTextBlockParam{{Text: llmInstructions}},
			Messages: []anthropic.BetaMessageParam{
				anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(sb.String())),
			},
		})
		if err != nil {
			var apierr *anthropic.Error
			if errors.As(err, &apierr) && (apierr.StatusCode == 401 || apierr.StatusCode == 403) {
				return fmt.Errorf("the Claude API rejected the credentials (%d): set ANTHROPIC_API_KEY", apierr.StatusCode)
			}
			fmt.Fprintln(os.Stderr, f.Event+":", err)
			continue
		}
		if resp.StopReason == anthropic.BetaStopReasonRefusal {
			fmt.Fprintln(os.Stderr, f.Event+": declined")
			continue
		}
		var text string
		for _, block := range resp.Content {
			if t, ok := block.AsAny().(anthropic.BetaTextBlock); ok {
				text += t.Text
			}
		}
		var got struct {
			Followers []followerLabel `json:"followers"`
		}
		if err := json.Unmarshal([]byte(text), &got); err != nil {
			fmt.Fprintln(os.Stderr, f.Event+": unreadable answer:", err)
			continue
		}
		for _, l := range got.Followers {
			labels[l.ID] = l
		}
		done++
		if b, err := json.MarshalIndent(labels, "", " "); err == nil {
			os.WriteFile(outPath, b, 0o644)
		}
		if done%25 == 0 {
			fmt.Fprintf(os.Stderr, "labelled %d families\n", done)
		}
	}
	fmt.Printf("%d followers labelled in %s\n", len(labels), outPath)
	return nil
}

type followerLabel struct {
	ID          string `json:"id"`
	Related     bool   `json:"related"`
	Direction   string `json:"direction"`
	Topic       string `json:"topic"`
	InsiderRisk string `json:"insider_risk"`
	Release     string `json:"release"`
}

var topics = []string{"politics", "elections", "geopolitics", "economy", "crypto", "tech_ai", "sports", "entertainment", "weather_science", "business", "other"}

const llmInstructions = `You label prediction markets for a study of how quiet markets react when a related busy market moves.

For each follower, answer:
- related: would news that moves the leader also change the follower's fair probability? Markets that merely share words are not related.
- direction: when the leader's YES price rises, should the follower's YES price rise ("same"), fall ("opposite"), or is it unclear? Mutually exclusive outcomes of one event usually move opposite.
- topic: the closest topic from the list.
- insider_risk: how likely is it that some traders know the follower's answer before the public (a company announcing its own numbers, a creator's own view counts, an official's own decision)?
- release: if the follower settles on a number or announcement released at a scheduled time, name it briefly (for example "ISM report, first business day of the month, 10:00 ET"); otherwise an empty string.

Return every follower id you were given, exactly once.`
