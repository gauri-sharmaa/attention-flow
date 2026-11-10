# Do prediction markets lag each other?

A year of Polymarket data (Oct 2025 – Oct 2026), tested four ways. Each step asks whether one market's move tells you where a related market is about to go, and whether that can be traded once real costs are paid. Everything here is paper: no orders were placed.

**Short answer:** yes, related markets are linked, and quiet markets really do react late, by 10–30 minutes. But the late reaction is small, points the expected way only about two times in three, and is smaller than the bid-ask spread you pay to trade it. No version tested made money after costs.

| Step | Question | Answer |
|---|---|---|
| 1 | Which prices move first? | Linked pairs in all 12 months, but the leader keeps changing |
| 2 | Do quiet markets lag their busy relatives? | Yes, 10–30 min, but trading it loses about 1.8¢ a trade |
| 3 | Can a pickier rule fix that? | No; one sports subset looked good on 4 matches only |
| 4 | Does that subset survive live, second-by-second order books? | No: no edge even before costs |

---

## 1. Which prices move first?

**Method:** Hoffmann–Rosenbaum–Yoshida lead-lag estimator (`internal/leadlag`), built for prices that trade at irregular times. For each candidate pair of 1,205 markets, the covariance of price changes is scanned across lags; significance comes from 99 shuffles per pair. Twelve 30-day periods.

**Result** ([`docs/results/real-365d-price-leadlag.txt`](results/real-365d-price-leadlag.txt)):
- Every month has far more linked pairs than chance: 36–154 at p ≤ 5% against 3–19 expected by chance.
- But the leader repeats from one month to the next only about half the time, a coin flip, so "market A leads market B" isn't a stable fact.
- The 61 leads that last two or more periods are almost all **the same question at a different date or price level** ("OpenAI IPO by Dec 31" → "OpenAI not IPO by Dec 31", 10 seconds; "Predict.fun FDV above $400M" → "above $500M", 30 minutes). The busier contract reprices first.

## 2. Do quiet markets lag their busy relatives?

**Families** (`cmd/attnflow/families.go`): 470 groups, each a busy market and up to 4 quieter markets in the same event (other candidates, other dates, other price levels, handicaps of the same match).

**Labels** (`scripts/label_families.py`): every quiet market is marked as related or not, which way it should move when the busy one rises (same or opposite), topic, insider risk and scheduled release. The rules were written after reading all 389 families that have data, and checked by hand on 15 tricky cases. Markets that only share a theme ("Will Trump say X") are dropped.

**Jumps** (`cmd/attnflow/lag.go`): the busy market moving 5¢ or more within 10 minutes, the 25 biggest per family. Each jump is **confirmed with the busy market's own minute price history**: it still has to be at least 4¢ away 10 minutes later. Half of the jumps detected from single trades failed this check. They were stray prints in a thin book that snapped straight back.

**Reaction**, measured from each quiet market's minute price history for an hour after the jump (5,049 confirmed jump × follower cases, [`docs/results/real-365d-lag.txt`](results/real-365d-lag.txt)):

| Topic | Quiet market moved 2¢+ within the hour | Minutes to half its one-hour move |
|---|---|---|
| All | 28% | 15 |
| Sports | 51% | 30 |
| World news | 39% | 15 |
| Politics | 30% | 10 |
| Crypto | 16% | 10 |
| AI / tech | 12% | 15 |
| Economy | 12% | 15 |
| Elections | 9% | 10 |

When a quiet market reacts, the reaction is usually not over within the first minute (only about 1 in 6 is). The labelled direction matched the actual move 62% of the time for "opposite" pairs and 71% for "same" pairs.

**Trading it** (`scripts/train_lag.py`): enter one minute after the jump at that price +1¢, exit an hour later −1¢. The direction and a gradient-boosted model (topic, volumes, jump size, price level, time to resolution, first-minute move, labels) were fitted on the first 70% of the year and scored on the last 30% only.

| Rule (last 30% of the year) | Trades | Per trade | Won |
|---|---|---|---|
| Every jump | 1,518 | −1.8¢ | 10% |
| Model says p > 0.5 | 65 | −2.8¢ | 40% |
| Model says p > 0.7 | 19 | −1.0¢ | 37% |

Most of the time the quiet market barely moves, so the trade just pays its costs.

## 3. Can a pickier rule fix it?

`scripts/lag_improve.py` ([`docs/results/real-365d-lag-improve.txt`](results/real-365d-lag-improve.txt)), same fair test:

| Rule | Trades | Per trade |
|---|---|---|
| Expected-move filter (trade only if the market *should* still move 2–8¢) | 4–52 | −1.7 to +0.4¢ |
| Date ladders only ("X by June" / "X by December") | 226 | −1.2¢ (moves only +0.8¢ before 2¢ of costs) |
| Two-way races only (one side ≈ 100% minus the other) | 55 | −2.1¢ (already caught up within a minute) |
| Same side of a live match (same team's handicap or game winner) | 30 | **+9.7¢** |

The "same side" result rested on **4 matches** in the test months, carried by a few 50–80¢ swings. Over the training months it was +1.6¢ (t = 1.3, 276 trades in 41 matches). It needed a live test.

## 4. Live, second by second

`cmd/attnflow/sportsrec.go` recorded every change in the best bid and ask for 21 live matches (NBA, WNBA, football, some tennis and esports), about 2 hours and 2.4 million book updates. `scripts/sports_lag.py` took every main-market move of 3¢+ within 30 seconds and paper-traded each same-side or other-side market at **real quotes**: buy at the ask, sell at the bid ([`docs/results/live-sports-side-markets.txt`](results/live-sports-side-markets.txt)).

| Paper trade | Trades | Per trade | Won |
|---|---|---|---|
| Enter 1 s after the move, exit after 1 min | 6,323 | −4.3¢ | 6% |
| Enter 1 s after, exit after 5 min | 6,057 | −4.4¢ | 15% |
| Enter 30 s after, exit after 5 min | 6,039 | −4.3¢ | 14% |
| Rest an order at the bid instead (filled 855 times) | 840 | −5.2¢ | 23% |

- **The side markets are slow:** when one did move the expected way, almost none of the move had happened one second after the main market moved, and only a third after 30 seconds.
- **But on average they don't move the expected way:** from one second after the move, the side market's midpoint moved **−0.01¢** in the expected direction over 5 minutes.
- **The spread to cross averaged 4.6¢.** Resting an order instead mostly filled just before the price went against it.

## What we learned

- Linked markets are real, and the clearest leads are the same question at a different date or price level, where the busier contract reprices first.
- Quiet markets do update late, but most leader moves don't carry over to them in a predictable direction, so the delay isn't money.
- Two checks changed the conclusions and are worth reusing:
  - **Confirm jumps against the leader's own price history.** Single trades in thin books fake half of them.
  - **Test candidate edges on real order books at real quotes.** Minute snapshots made a 4-match fluke look like +9.7¢.

**Caveats:** one year of Polymarket only. Families come from shared events, so relatives in different events aren't included. Minute price history is not an order book (step 4 fixes that, but only for sports and only for one evening).

## How to rerun

```bash
go build -o attnflow ./cmd/attnflow
./attnflow polytrades -days 365 -n 400 -closed 1000 -bars=false -out data/pmt365   # a year of trades (hours; resumable)
./attnflow leadlag -dir data/pmt365 -periods 12        # step 1
./attnflow families                                    # step 2: build families (data/pmt365/families.json)
./attnflow lag                                         # step 2: reactions from minute price history
python3 scripts/label_families.py                      # step 2: follower labels
python3 scripts/train_lag.py                           # step 2: lag per topic + trading test
python3 scripts/lag_improve.py                         # step 3
./attnflow sportsrec -for 2h                           # step 4: record live order books (no orders)
python3 scripts/sports_lag.py                          # step 4: analysis
```

Raw data (trades, price history, order books) isn't in git; the commands above download or record it. Python needs `numpy` and `scikit-learn`.
