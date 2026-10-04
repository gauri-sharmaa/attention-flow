# Design notes

The questions this answers: what exactly is estimated, why each choice, and where it breaks.

## Data model

Each topic *j* has an attention level `x_j = log(views)` per bar. The engine works on innovations `r_j,t = x_j,t − x_j,t−1`. Events can arrive in any order within a bar. Ingest only stores the latest value (35 ns), and all work happens when the bar closes. A missing observation carries the level forward.

## Sector factors

Topics in a sector move together. Without removing that, every pair in AI would look linked.

- Factor per sector by cross-sectional GLS: `f = Σ(λ/σ²)r / Σ(λ²/σ²)`.
- Each topic gets a **leave-one-out** factor, so its own move never explains itself.
- Loadings λ are EW regressions of `r` on the leave-one-out factor, renormalised to mean 1.
- Inputs are winsorised at 4σ so one shock doesn't register as a sector move.

## Per-topic forecast (RLS)

Each topic has its own recursive-least-squares model with forgetting (half-life 2,000 bars):

```
r_j,t ≈ Σ_parents Σ_{l∈lag±1} β·r_i,t−l  +  γ₀·f_t(LOO) + Σ_k γ_k·f_t−k  +  δ·(x_j,t−1 − baseline)  +  c
```

- The lag ±1 window absorbs timing jitter.
- Lagged factor terms capture slow responders: topics that follow their sector a few bars late.
- The decay term captures attention fading back to normal.
- Each bar the model is scored *before* it learns from that bar, so its residual `v` is an honest forecast error.

When the graph changes, the model is rebuilt and keeps the weights and covariance block of every feature it already had.

## Lead-lag discovery

For every candidate pair (a, b), lag l = 1..L and both directions, it keeps an EW moment `E[u_a,t−l · v_b,t]`:

- `u_a` is the leader's factor-removed move. That is what propagates.
- `v_b` is the follower's **forecast error** given its current parents.

Testing against forecast errors is the important choice. If a → b → c and b → c is already learned, c's error no longer contains a's signal, so a → c is never proposed. It works like forward selection, done online.

**Add** when the Fisher z of the best lag clears 5.5, beats the reverse direction by 2, and the follower has room (max 6 parents). At most one new parent per follower per round. The pair's moments reset afterwards so it isn't re-proposed from stale statistics.

**Drop** on either:
- **Coefficient test:** the summed β over the edge's lag window has |t| < 2 after 600 bars, using `var(β̂) ≈ σ²·1ᵀP1` from the RLS covariance.
- **Health check:** `E[v·c]/E[c²] < −0.6`, where `c` is the edge's own contribution to the forecast. A live edge sits near 0. A dead edge's whole contribution shows up as error, so the ratio goes to −1. This reacts in a few hundred bars, while the RLS coefficient takes thousands to decay. It cut stale edges after the regime change from 21 to 0.

## Forecast, dislocation and catch-up

Lags are ≥ 1, so the H-step forecast needs no solver. Step h only reads values from steps < h, so it's one pass per step through the graph, with multi-hop propagation included. A second path uses only decay and intercept.

- **Dislocation** = full path − decay path at H. It's the move the graph and factor say is coming that the level hasn't made yet.
- **Catch-up time** = first step where 90% of it has arrived.
- **Drivers** = first-hop contributions from parents and the factor.

## Confidence

Every forecast is scored H bars later inside the engine:

- `σ` is rescaled by the running mean squared standardised error, per topic.
- `P(direction right)` comes from a pooled, EW-weighted table of past hit rates by score `|d|/σ`. It is made monotone with pool-adjacent-violators and shrunk toward Φ(score) in thin bins.

On the simulator, stated and realised rates match to within about 1 point in every bin.

## Shocks

A bar is a shock when its robust z-score (running median/MAD, which spikes can't drag) exceeds 7.

- If a parent shocked on schedule (within its lag ±2), the shock is labelled an **echo** of that parent rather than a new source.
- The shock is pushed through the graph as an impulse response to predict each follower's move, timing and probability.
- Probability is the product of Laplace-smoothed per-edge hit rates, updated as shocks resolve.
- Realised moves are measured net of the follower's sector factor.
- Only 2 hops are shown: on the simulator, hop 3 is close to chance.

## Simulator and oracle

`internal/sim` plants:
- 3 sector factors, 30% slow responders
- ~255 directed edges with lags 1–8, mostly between related topics plus some between unrelated ones
- heavy-tailed shocks, attention decay, 1% missing data
- 20% of edges replaced halfway through

The oracle forecasts with the true graph, betas, delays and decay, from the same observations. Engine R² ÷ oracle R² is the share of the predictable signal captured.

## Known limits

- **Candidate recall caps graph recall.** Edges between topics with no shared words are never tested. On the simulator that's 15% of edges. Testing all pairs finds them (100% recall, 88–90% precision) at 3× the per-bar cost. That's fine at 150 topics but not at thousands. Sentence embeddings would raise recall without that cost.
- **Confounding through untested parents.** If p → a and p → c but p → c isn't a candidate, a can look like it leads c. These are most of the remaining false edges.
- **Linear and positive-scale.** Attention reacting non-linearly (saturation, thresholds) is only approximated.
- **Real data is hourly.** Wikipedia pageviews give hourly bars, so lags are in hours and there are about 10× fewer bars than the minute simulation. The hourly config shortens windows to match, and accuracy drops (see README). Minute-level feeds (social APIs, attention-market trades) plug in through the same CSV.
- **Not a trading system.** Nothing here models fees, liquidity or market impact on an attention market.
