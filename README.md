# attention flow

A streaming engine that learns how attention moves between topics, then flags the ones that haven't caught up yet.

When OpenAI spikes, ChatGPT tends to follow a few minutes later, and Sam Altman after that. This engine finds those lead-lag links on its own, estimates where each topic's attention *should* be given what its leaders already did, and says how sure it is and how long the catch-up should take.

![dashboard](docs/dashboard.png)

## How it works

```
events ─► bar clock ─► sector factors ─► per-topic forecasts ─► lead-lag search ─► graph edits
                                                     │                                   │
                                                     └──────── H-bar forecast ◄──────────┘
                                                                   │
                                                  dislocations · shocks · confidence
```

1. **Candidates.** Names, tags and Wikipedia text pick which pairs are worth testing. 147 topics → 1,076 pairs instead of 10,731.
2. **Sector factors.** A shared AI / crypto / tech attention factor, so "everything in AI went up" doesn't look like a lead-lag link.
3. **Lead-lag search.** For every candidate pair and lag, a running correlation between the leader's move and the follower's *forecast error*. Testing on errors means a link only counts if nothing already explains it.
4. **Graph edits.** Links are added when significant and dropped when the follower's model stops needing them, or when their predicted effect keeps failing to show up.
5. **Forecast.** Every topic is projected H bars ahead through the graph. The gap between that path and a plain fade-back path is the **dislocation**.
6. **Shocks.** Bursts are pushed through the graph to predict who moves next, by how much and when.

Everything updates online, once per bar. The same code runs live and in replay. Details in [docs/DESIGN.md](docs/DESIGN.md).

## Results

Attention data has no answer key, so the engine is scored on a simulator that plants one: known links, lags, sector factors, shocks, and a regime change halfway through. Every number is out of sample, and the oracle is a forecaster that knows the planted model.

| | 2 weeks, minute bars | 90 days, hourly (3 seeds) |
|---|---|---|
| Links found / links that are real | 85% / 92% | 60–68% / 86–88% |
| Lag correct (±1 bar) | 100% | 99–100% |
| Share of predictable move captured (vs oracle) | 79% | 53–62% |
| Strongest 10% of signals, right direction | 83% | 81–89% |
| Stated confidence → actual hit rate | 78% → 79%, 97% → 97% | Brier 0.229–0.235 (coin flip 0.25) |
| Shock followers moving the predicted way | 82% | 72–75% |
| Re-learns graph after regime change | ~1,000 bars | ~1,000 bars |

Engine cost: 175 µs per bar for 147 topics (p99 0.5 ms), 35 ns to ingest an event. Full scorecards are in [docs/results](docs/results).

## Real data

Thirty days of every trade on the 228 busiest Polymarket markets, plus 2M news, Reddit and Hacker News mentions of the names in those markets. Everything is fit on the first 70% of the month and scored on the last 30%. Each test has a placebo: the same streams shifted by whole days, which keeps their own burstiness and time-of-day pattern but destroys real timing.

| Question | Model | Held-out gain | Placebo | Answer |
|---|---|---|---|---|
| Does attention lead attention, hourly? | engine, Wikipedia | ≈ 0 | – | No: related topics move in the same hour |
| Do market prices lead each other? | engine, 5-min bars | ≈ 0 | – | No: prices behave like an efficient market |
| Does trading in one market set off trading in related ones? | Hawkes, trade times | +0.030 nats/trade | −0.006 to −0.009 | **Yes, modestly** |
| Does trading activity predict price moves? | Hawkes, trade times | +0.243 nats/move | −0.18 to −0.44 | **Yes**: ~6 min ahead in its own market, ~12 min in related ones |
| Does news, Reddit or HN buzz predict trading? | Hawkes, mention times | −0.012 | −0.002 | No |
| Does trading predict buzz? | Hawkes, mention times | −0.001 | −0.005 | No |

What made the difference was dropping bars. In Polymarket, information moves in seconds to minutes, so 5-minute bars smear it away. The Hawkes model (`internal/hawkes`) works on raw event times. Each trade raises the near-term rate of further trades in its own market and in related ones, and the fitted excitation matrix is the lead-lag graph. Three checks keep it honest:

- **Same-second events never excite each other.** A trade and the price move it causes, or one bot hitting two markets in the same second, are simultaneous, not predictive. Adding this cut the first cross-market estimate from +0.104 to +0.030.
- **A shared hour-of-week background.** Every market is busier in US daytime, and without this, that alone looks like a link.
- **News in the model.** The cross-market links barely change (26.75 → 26.68 total excitation), so they aren't just markets reacting to the same headlines.

Scorecards: [docs/results](docs/results). Methods follow Bacry, Mastromatteo & Muzy, [Hawkes processes in finance](https://arxiv.org/abs/1502.04592), and Xu, Farajtabar & Zha, [Learning Granger causality for Hawkes processes](http://proceedings.mlr.press/v48/xuc16.pdf).

## Run it

Go 1.24, no dependencies.

```sh
go build -o attnflow ./cmd/attnflow

./attnflow sim                                   # simulate 2 weeks with an answer key
./attnflow replay -truth data/sim/truth.json     # score everything
./attnflow serve                                 # dashboard at localhost:8080
./attnflow export -out site                      # static dashboard for hosting

./attnflow polytrades                            # every trade on the busiest Polymarket markets
./attnflow hawkes                                # does trading spread between markets?
./attnflow hawkes -moves 0.02                    # does trading predict price moves?
./attnflow outside                               # news, Reddit, Hacker News mentions
./attnflow hawkes -extra data/pmt/outside-news.csv,data/pmt/outside-reddit.csv,data/pmt/outside-hn.csv
```

Real data (Wikipedia hourly pageviews for the topics in [data/universe.txt](data/universe.txt)):

```sh
./attnflow fetch -contact you@example.com -days 90
./attnflow replay -events data/wiki/events.csv -text data/wiki/text.json -bar 3600
./attnflow serve  -events data/wiki/events.csv -text data/wiki/text.json -bar 3600 -label wikipedia
```

Any other feed (Kaito mindshare, Google Trends, attention-market prices) only needs to write the same `ts,entity,value` CSV.

## Layout

```
cmd/attnflow       CLI
internal/engine    streaming inference: factors, RLS forecasts, lead-lag graph, shocks
internal/sim       simulator with a planted answer key
internal/replay    out-of-sample scoring, oracle ceiling
internal/semantic  TF-IDF candidate graph
internal/stats     online estimators (EW, robust median/MAD, RLS)
internal/ring      lock-free SPSC queue for ingest
internal/source    Wikipedia, Polymarket, Bluesky, GDELT, Reddit, Hacker News
internal/hawkes    multivariate Hawkes processes on raw event times
internal/server    SSE server + dashboard
```
