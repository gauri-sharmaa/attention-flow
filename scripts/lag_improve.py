"""Two ways to make the lag trade pickier, tested on data/pmt365/lag_events.json.

Same setup as train_lag.py: only jumps the leader's price history confirms,
rules fitted on the first 70% of the year, trades scored on the last 30%,
enter one minute after the jump at that price +1c, exit at 60 minutes -1c.

  1. Expected-move filter: from the training period, each follower's typical
     move per unit of leader move (its beta). After a jump, the follower
     should move beta x the leader's move; trade only when what's left of that
     (expected minus what it already did in the first minute) beats a threshold.
  2. Built-in links only: pairs whose relation is mechanical, not just topical:
     date ladders ("X by June" / "X by December"), the same side's handicap or
     game winner, and two-way races (one candidate's YES is roughly the other's NO).
"""
import json, math, os, re, sys
from collections import defaultdict
import numpy as np

DIR = sys.argv[1] if len(sys.argv) > 1 else "data/pmt365"
COST = 0.01
evs = json.load(open(os.path.join(DIR, "lag_events.json")))
lab = json.load(open(os.path.join(DIR, "families_llm.json")))
evs = [e for e in evs if e.get("LeaderMoves") and np.sign(e["LeaderMoves"][3]) == np.sign(e["LeaderMove"])
       and abs(e["LeaderMoves"][3]) >= 0.04 and lab.get(e["FollowerID"], {}).get("related", True)]
evs.sort(key=lambda e: e["T"])
split = evs[int(len(evs) * 0.7)]["T"]
train = [e for e in evs if e["T"] < split]
test = [e for e in evs if e["T"] >= split]
M = lambda e, i: e["Moves"][i]  # 0:1m 1:2m 2:5m 3:10m 4:15m 5:30m 6:60m


def kind(e):
    l = lab.get(e["FollowerID"], {})
    f, ld = e["Follower"].lower(), e["Leader"].lower()
    if l.get("direction") == "same" and re.search(r"\bby\b|before|through", f):
        return "date ladder"
    if l.get("direction") == "same" and re.search(r"handicap|spread|winner", f):
        return "same side"
    if e["Siblings"] == 1 and l.get("direction") == "opposite" and "draw" not in f and "draw" not in ld:
        return "two-way race"
    return None


# 1. betas from the training period (follower 60-min move vs the leader's confirmed move)
xs, ys = defaultdict(list), defaultdict(list)
for e in train:
    xs[e["FollowerID"]].append(e["LeaderMoves"][3]); ys[e["FollowerID"]].append(M(e, 6))
beta = {}
for k in xs:
    x, y = np.array(xs[k]), np.array(ys[k])
    if len(x) >= 3 and (x @ x) > 0:
        beta[k] = float(x @ y / (x @ x))


def trade(e, side):
    return side * (M(e, 6) - M(e, 0)) - 2 * COST


def summary(gs):
    if len(gs) < 2:
        return f"{len(gs):5d} trades"
    g = np.array(gs)
    return f"{len(g):5d} trades  {100*g.mean():+6.2f}c each  (t {g.mean()/(g.std(ddof=1)/math.sqrt(len(g))):+5.2f})  won {100*(g>0).mean():3.0f}%"


print(f"{len(evs)} confirmed jumps x related followers; test period {len(test)} rows\n")
print("1. EXPECTED-MOVE FILTER: trade when beta x leader move, minus the follower's first-minute move, is at least:")
for th in (0.02, 0.03, 0.05, 0.08):
    gs = []
    for e in test:
        b = beta.get(e["FollowerID"])
        if b is None:
            continue
        gap = b * e["LeaderMoves"][3] - M(e, 0)
        if abs(gap) >= th:
            gs.append(trade(e, np.sign(gap)))
    print(f"   {100*th:.0f}c:  " + summary(gs))

print("\n2. BUILT-IN LINKS ONLY (direction from the labels, every confirmed jump):")
for k in ("date ladder", "same side", "two-way race"):
    gs = []
    for e in test:
        if kind(e) != k:
            continue
        d = 1 if lab[e["FollowerID"]]["direction"] == "same" else -1
        gs.append(trade(e, d * np.sign(e["LeaderMove"])))
    print(f"   {k:13s} " + summary(gs))

print("\n3. BOTH: built-in links, and the expected move left is at least 3c:")
gs = []
for e in test:
    k, b = kind(e), beta.get(e["FollowerID"])
    if not k or b is None:
        continue
    gap = b * e["LeaderMoves"][3] - M(e, 0)
    d = (1 if lab[e["FollowerID"]]["direction"] == "same" else -1) * np.sign(e["LeaderMove"])
    if abs(gap) >= 0.03 and np.sign(gap) == d:
        gs.append(trade(e, d))
print("   " + summary(gs))

print("\n4. WHERE THE COSTS GO: average follower move in the trade direction (1->60 min), before costs, for the built-in links:")
for k in ("date ladder", "same side", "two-way race"):
    mv = [ (1 if lab[e["FollowerID"]]["direction"] == "same" else -1) * np.sign(e["LeaderMove"]) * (M(e, 6) - M(e, 0)) for e in test if kind(e) == k]
    if mv:
        print(f"   {k:13s} {100*np.mean(mv):+.2f}c before costs vs 2c of costs  (n={len(mv)})")
