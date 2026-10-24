"""Learn how fast quiet Polymarket markets catch up with a busy relative.

Input: data/pmt365/lag_events.json from `attnflow lag` (one row per leader
jump x follower, with the follower's price change 1, 2, 5, 10, 15, 30 and 60
minutes later), and optionally data/pmt365/families_llm.json from
`attnflow famllm` (Claude's labels: related, direction, topic, insider risk).

Two questions, both answered per topic:
  1. Lag: when the busy market jumps, how often does the quiet one move, and
     how many minutes until it has made half of its one-hour move?
  2. Is the lag tradable? A paper trade enters ONE MINUTE after the jump (time
     to see it and act) at that minute's price plus 1c, in the direction the
     follower is expected to move, and exits after 60 minutes minus 1c.
     The expected direction and the model are fitted on the first 70% of the
     year and scored on the last 30%, never the other way round.

The model (gradient-boosted trees) uses only what is known at entry: topic,
volumes, jump size, price levels, time to resolution, time of day, how much
the follower already moved in the first minute, and Claude's labels if
present. It predicts whether a trade will clear its costs.
"""

import json
import math
import os
import sys
from collections import defaultdict

import numpy as np
from sklearn.ensemble import HistGradientBoostingClassifier

DIR = sys.argv[1] if len(sys.argv) > 1 else "data/pmt365"
COST = 0.01  # paid on the way in and again on the way out
H = [1, 2, 5, 10, 15, 30, 60]

evs = json.load(open(os.path.join(DIR, "lag_events.json")))
llm = {}
p = os.path.join(DIR, "families_llm.json")
if os.path.exists(p):
    llm = json.load(open(p))
print(f"{len(evs)} jump x follower reactions" + (f", {len(llm)} with Claude labels" if llm else ", no Claude labels yet"))

for e in evs:
    lab = llm.get(e["FollowerID"])
    e["topic"] = lab["topic"] if lab else e["Topic"]
    e["related"] = lab["related"] if lab else None
    e["llm_dir"] = {"same": 1, "opposite": -1}.get(lab["direction"], 0) if lab else 0
    e["insider"] = {"low": 0, "medium": 1, "high": 2}.get(lab["insider_risk"], 1) if lab else 1
    e["release"] = 1 if lab and lab["release"] else 0
    e["m"] = dict(zip(H, e["Moves"]))

if llm:
    before = len(evs)
    evs = [e for e in evs if e["related"] is not False]
    print(f"dropped {before - len(evs)} rows Claude judged unrelated")

evs.sort(key=lambda e: e["T"])
split_t = evs[int(len(evs) * 0.7)]["T"]
train = [e for e in evs if e["T"] < split_t]
test = [e for e in evs if e["T"] >= split_t]


def half_lag(e):
    """Minutes until the follower made half its 60-minute move (None if it barely moved)."""
    m60 = e["m"][60]
    if abs(m60) < 0.02:
        return None
    for h in H:
        if abs(e["m"][h]) >= abs(m60) / 2 and np.sign(e["m"][h]) == np.sign(m60):
            return h
    return 60


# ---- 1. lag per topic (all data: this part is description, not a trading test)
rows = defaultdict(list)
for e in evs:
    rows[e["topic"]].append(e)
    rows["ALL"].append(e)
print("\n1. HOW QUIET MARKETS REACT when their busy relative jumps 5c+ in 10 minutes")
print(f"{'topic':16s} {'events':>7s} {'reacted':>8s} {'median lag':>11s} {'done by 1 min':>14s} {'avg 60-min move':>16s}")
for k in sorted(rows, key=lambda k: -len(rows[k])):
    es = rows[k]
    lags = [half_lag(e) for e in es]
    moved = [l for l in lags if l is not None]
    early = [e for e in es if half_lag(e) is not None and abs(e["m"][1]) >= abs(e["m"][60]) / 2]
    print(f"{k:16s} {len(es):7d} {100 * len(moved) / len(es):7.0f}% {np.median(moved) if moved else float('nan'):9.0f}m "
          f"{100 * len(early) / max(len(moved), 1):13.0f}% {100 * np.mean([abs(e['m'][60]) for e in es]):14.1f}c")
print("reacted = moved 2c+ within the hour; median lag = minutes to half its one-hour move; done by 1 min = share of reactions mostly over before anyone could act")

# ---- 2. tradable? direction learned on the training period
pair_dir = defaultdict(float)
topic_dir = defaultdict(float)
for e in train:
    s = np.sign(e["LeaderMove"]) * (e["m"][60] - e["m"][1])
    pair_dir[e["FollowerID"]] += s
    topic_dir[e["topic"]] += s


def direction(e):
    if e["llm_dir"]:
        return e["llm_dir"]
    d = pair_dir.get(e["FollowerID"], 0) or topic_dir.get(e["topic"], 0)
    return int(np.sign(d))


def pnl(e):
    s = direction(e) * np.sign(e["LeaderMove"])
    if s == 0:
        return None
    entry, exit_ = e["m"][1], e["m"][60]
    return s * (exit_ - entry) - 2 * COST


topics = sorted({e["topic"] for e in evs})


def features(e):
    f = [
        math.log10(max(e["FollowerVol"], 1)), math.log10(max(e["LeaderVol"], 1)),
        abs(e["LeaderMove"]), e["LeaderPrice"], e["Before"], min(e["Before"], 1 - e["Before"]),
        math.log10(max(e["HoursToEnd"], 0.1)), e["Siblings"], (e["T"] // 3600) % 24,
        direction(e) * np.sign(e["LeaderMove"]) * e["m"][1],  # how far it already moved our way in minute 1
        e["insider"], e["release"],
    ]
    f += [1.0 if e["topic"] == t else 0.0 for t in topics]
    return f


tr = [(features(e), pnl(e)) for e in train if pnl(e) is not None]
te = [(features(e), pnl(e), e) for e in test if pnl(e) is not None]
X = np.array([f for f, _ in tr]); y = np.array([g > 0 for _, g in tr])
model = HistGradientBoostingClassifier(max_iter=200, learning_rate=0.05, max_depth=4, random_state=0).fit(X, y)
probs = model.predict_proba(np.array([f for f, _, _ in te]))[:, 1] if te else []


def summary(gains):
    if len(gains) < 2:
        return f"{len(gains):6d} trades"
    g = np.array(gains)
    return f"{len(g):6d} trades  {100 * g.mean():+6.2f}c each  (t {g.mean() / (g.std(ddof=1) / math.sqrt(len(g))):+5.2f})  won {100 * (g > 0).mean():3.0f}%"


print(f"\n2. IS IT TRADABLE? test period = last 30% of the year ({len(te)} trades), enter 1 min after the jump +1c, exit at 60 min -1c")
print("   every jump:        " + summary([g for _, g, _ in te]))
for th in (0.5, 0.6, 0.7):
    print(f"   model says p>{th}:   " + summary([g for (_, g, _), p_ in zip(te, probs) if p_ > th]))
print("\n   by topic, model p>0.5:")
for t in topics:
    gs = [g for (_, g, e), p_ in zip(te, probs) if p_ > 0.5 and e["topic"] == t]
    if gs:
        print(f"   {t:16s} " + summary(gs))
print("\ncaveats: prices are Polymarket's minute price history, not the order book, so the 1c costs are a guess; a stale price")
print("doesn't prove an offer was there to take; the families came from shared events, so cross-event relatives are missing until Claude labels them.")
