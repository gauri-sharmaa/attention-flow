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

Ninety days of every trade on the 382 busiest Polymarket markets (1.67M trades, including markets that resolved during the window), plus 2M news, Reddit and Hacker News mentions. Every test is walk-forward: fit on one 30-day period, scored on the next, which it never saw. Each has a placebo that shifts streams by whole days, keeping their rhythm and destroying real timing.

| Question | Method | Month 2 | Month 3 | Placebo | Answer |
|---|---|---|---|---|---|
| Does trading in one market set off related markets? | Hawkes, trade times | +0.032 | +0.008 | −0.022 / −0.042 | **Yes, modestly** |
| Does trading activity predict price moves? | Hawkes, trades → 2¢ moves | +0.114 | +0.166 | −0.37 / −0.45 | **Yes** |
| Do some prices move before related prices? | Hoffmann–Rosenbaum–Yoshida | 55 pairs (2 by chance) | 121 (5) | – | **Linked, but the leader flips** |
| Is the order-flow signal profitable? | paper trading, 1¢ per side | −9.3%/trade | −6.7%/trade | −13.7% / −8.0% | No |
| Does news / Reddit / HN buzz lead trading? | event study, 1,743 surges | flat | flat | flat | No |

Gains are held-out log-likelihood per event (nats). A result counts when it beats zero and the placebo in every month.

- **Prices:** linked markets move together every month, but which one moves first holds only about 46% of the time from one month to the next, a coin flip. Seventeen pairs keep a stable leader. Almost all are the same question at different dates or strikes, where the busier contract reprices first (Iran blockade "by Dec 31" leads "by Oct 31" by up to 30 minutes).
- **Trading:** order-flow direction has a little information, +0.4% per trade before costs (t = 1.3). A 1¢ spread costs far more. Even half-cent costs lose 3.6% per trade.
- **Buzz:** around news and Reddit surges, trading in the related markets stays flat before and after. Newsy days are a few percent busier overall, with no timing.

Demo page: [docs/site/index.html](docs/site/index.html), built by `python3 docs/site/build.py` from the scorecards in [docs/results](docs/results).

Bars blur timing, so the methods that work use raw event times:

- **Hawkes processes** (`internal/hawkes`): each trade raises the near-term rate of more trades in its own and related markets, and the fitted excitation matrix is the lead-lag graph. Same-second events never excite each other: a trade and the price move it causes are simultaneous, and blocking that cut the first cross-market estimate from +0.104 to +0.030. A shared hour-of-week background stops US daytime from passing for a link.
- **Hoffmann–Rosenbaum–Yoshida lead-lag** (`internal/leadlag`): covariance of price changes over overlapping observation intervals, scanned across lags, with a p-value from 99 shuffles.
- **Event study** (`attnflow eventstudy`): trading around each surge against the same clock time on other days.

Methods follow Bacry, Mastromatteo & Muzy, [Hawkes processes in finance](https://arxiv.org/abs/1502.04592); Xu, Farajtabar & Zha, [Learning Granger causality for Hawkes processes](http://proceedings.mlr.press/v48/xuc16.pdf); and Hoffmann, Rosenbaum & Yoshida, [Estimation of the lead-lag parameter from non-synchronous data](https://arxiv.org/abs/1303.4871).

## Run it

Go 1.24, no dependencies.

```sh
go build -o attnflow ./cmd/attnflow

./attnflow sim                                   # simulate 2 weeks with an answer key
./attnflow replay -truth data/sim/truth.json     # score everything
./attnflow serve                                 # dashboard at localhost:8080
./attnflow export -out site                      # static dashboard for hosting

./attnflow polytrades -days 90 -closed 300 -out data/pmt90   # every trade, incl. resolved markets
./attnflow hawkes -dir data/pmt90 -folds 2       # does trading spread between markets? (walk-forward)
./attnflow hawkes -dir data/pmt90 -moves 0.02 -folds 2       # does trading predict price moves?
./attnflow leadlag -dir data/pmt90               # which prices move first
./attnflow backtest -dir data/pmt90              # paper-trade the order-flow signal
./attnflow eventstudy                            # does buzz lead trading?
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
internal/leadlag   Hoffmann–Rosenbaum–Yoshida lead-lag for asynchronous prices
internal/server    SSE server + dashboard
```
