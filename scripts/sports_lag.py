"""Can a match's side markets be traded after the main market moves?

Input: a `attnflow sportsrec` recording (groups.json, books.csv). For every
match, the main market (the match itself, or the busiest "Will X win on ..."
market) leads; its side markets (handicaps, spreads, game/map/set winners)
follow. A move is the main market's midpoint changing 3c or more within 30
seconds, judged only while its bid-ask spread is 5c or less (one per minute).

For each move and side market:
  - direction: the same team's side market should move with the main market,
    the other team's against it (markets with no clear side are skipped)
  - paper trade: enter L seconds after the move (L = 1, 2, 5, 10, 30) at the
    real best price (the ask to buy, the bid to sell), exit 60 or 300 seconds
    after the move at the real opposite price. Both spreads are paid; nothing
    is estimated. Quotes with a spread over 10c are skipped.
"""
import bisect
import csv
import json
import math
import os
import re
import sys
from collections import defaultdict

import numpy as np

DIR = sys.argv[1] if len(sys.argv) > 1 else "data/sportsrec"
LAT = [1, 2, 5, 10, 30]
EXITS = [60, 300]

groups = json.load(open(os.path.join(DIR, "groups.json")))
quotes = defaultdict(lambda: ([], [], []))  # market -> times, bids, asks
with open(os.path.join(DIR, "books.csv")) as f:
    for r in csv.DictReader(f):
        t, b, a = float(r["t"]), float(r["bid"]), float(r["ask"])
        q = quotes[r["market"]]
        q[0].append(t); q[1].append(b); q[2].append(a)


def at(m, t):
    ts, bs, as_ = quotes[m]
    k = bisect.bisect_right(ts, t) - 1
    if k < 0:
        return None
    return bs[k], as_[k]


def norm(s):
    return re.sub(r"[^a-z0-9 ]", " ", s.lower()).split()


def teams(title):
    t = re.sub(r"\s*\(bo\d\).*$", "", title, flags=re.I)
    t = t.split(": ", 1)[-1] if ": " in t else t
    parts = re.split(r"\s+vs\.?\s+", t, flags=re.I)
    return (parts[0], parts[1]) if len(parts) == 2 else (None, None)


def is_team(name, team):
    a, b = norm(name), norm(team)
    if not a or not b:
        return False
    return a[0] in b or b[0] in a or a[-1] in b or b[-1] in a


def side_of(follower, A, B):
    """+1 if the follower is about team A, -1 if team B, 0 if unclear."""
    f = follower
    m = re.match(r"(?i)(?:1h )?spread:\s*(.+?)\s*\(", f)
    if m:
        t = m.group(1)
        return 1 if is_team(t, A) and not is_team(t, B) else -1 if is_team(t, B) else 0
    m = re.search(r"(?i)handicap:\s*(.+?)\s*\([+-][\d.]+\)\s*vs\s*(.+?)\s*\(", f)
    if m:
        x, y = m.group(1), m.group(2)
        if is_team(y, B) or is_team(x, A):
            return 1
        if is_team(y, A) or is_team(x, B):
            return -1
        return 0
    m = re.search(r"(?i)(?:set \d winner:\s*)?(.+?)\s+vs\.?\s+(.+?)(?:\s+-\s+(?:game|map|set) \d winner)?$", f.split(": ", 1)[-1])
    if m and re.search(r"(?i)winner", f):
        x = m.group(1)
        return 1 if is_team(x, A) else -1 if is_team(x, B) else 0
    m = re.match(r"(?i)will (.+?) win on", f)
    if m:
        return 1 if is_team(m.group(1), A) else -1 if is_team(m.group(1), B) else 0
    return 0


trades = {(L, X): [] for L in LAT for X in EXITS}
passive = {X: [] for X in EXITS}  # wait with an order at the best bid (or ask, to sell) instead of crossing
passive_tried = 0
moves_seen = 0
stale = defaultdict(list)
for title, g in groups.items():
    ms = g["Markets"]
    A, B = teams(title)
    if not A:
        continue
    mains = [m for m in ms if m["Question"] == title or re.search(r"(?i)win on \d", m["Question"])]
    if not mains:
        continue
    lead = max(mains, key=lambda m: len(quotes[m["ConditionID"]][0]))
    lq = lead["Question"]
    lead_side = 1 if lq == title else side_of(lq, A, B)
    if lead_side == 0:
        continue
    fol = []
    for m in ms:
        if m["ConditionID"] == lead["ConditionID"] or "draw" in m["Question"].lower():
            continue
        s = side_of(m["Question"], A, B)
        if s:
            fol.append((m, s * lead_side))
    ts, bs, as_ = quotes[lead["ConditionID"]]
    last = -1e18
    for i in range(len(ts)):
        b, a = bs[i], as_[i]
        if not (0 < b < a < 1) or a - b > 0.05 or ts[i] - last < 60:
            continue
        k = bisect.bisect_right(ts, ts[i] - 30) - 1
        if k < 0 or not (0 < bs[k] < as_[k] < 1) or as_[k] - bs[k] > 0.05:
            continue
        d = (b + a) / 2 - (bs[k] + as_[k]) / 2
        if abs(d) < 0.03:
            continue
        last, t0 = ts[i], ts[i]
        moves_seen += 1
        for m, s in fol:
            cid = m["ConditionID"]
            pre = at(cid, t0 - 1)
            if not pre or not (0 < pre[0] < pre[1] < 1) or pre[1] - pre[0] > 0.10:
                continue
            want = s * np.sign(d)  # +1: follower should rise
            end = at(cid, t0 + 300)
            # Passive version: one second after the move, rest an order at the
            # side market's best bid (to buy) or best ask (to sell). Counted as
            # filled only if within 60 seconds the opposite best price reaches
            # our price (someone was willing to trade there); then exit at the
            # opposite side 60/300 seconds after the move, paying that spread.
            q1 = at(cid, t0 + 1)
            if q1 and 0 < q1[0] < q1[1] < 1 and q1[1] - q1[0] <= 0.10:
                passive_tried += 1
                price = q1[0] if want > 0 else q1[1]
                ts_f, bs_f, as_f = quotes[cid]
                k0, k1 = bisect.bisect_left(ts_f, t0 + 1), bisect.bisect_right(ts_f, t0 + 60)
                filled = any((as_f[j] <= price if want > 0 else bs_f[j] >= price) for j in range(k0, k1))
                if filled:
                    for X in EXITS:
                        e = at(cid, t0 + X)
                        if e and 0 < e[0] < e[1] < 1:
                            passive[X].append(((e[0] - price) if want > 0 else (price - e[1]), title))
            for L in LAT:
                q = at(cid, t0 + L)
                if not q or not (0 < q[0] < q[1] < 1) or q[1] - q[0] > 0.10:
                    continue
                if end and 0 < end[0] < end[1] < 1:
                    total = want * ((end[0] + end[1]) / 2 - (pre[0] + pre[1]) / 2)
                    done = want * ((q[0] + q[1]) / 2 - (pre[0] + pre[1]) / 2)
                    if total >= 0.02:
                        stale[L].append(done / total)
                for X in EXITS:
                    e = at(cid, t0 + X)
                    if not e or not (0 < e[0] < e[1] < 1):
                        continue
                    pnl = (e[0] - q[1]) if want > 0 else (q[0] - e[1])
                    trades[(L, X)].append((pnl, title))


def summary(xs):
    if len(xs) < 2:
        return f"{len(xs):5d} trades"
    g = np.array([x for x, _ in xs])
    nm = len({t for _, t in xs})
    return f"{len(g):5d} trades in {nm:3d} matches  {100 * g.mean():+6.2f}c each  (t {g.mean() / (g.std(ddof=1) / math.sqrt(len(g)) + 1e-12):+5.2f})  won {100 * (g > 0).mean():3.0f}%"


span = max((q[0][-1] for q in quotes.values() if q[0]), default=0) - min((q[0][0] for q in quotes.values() if q[0]), default=0)
print(f"SPORTS SIDE-MARKET LAG · {len(groups)} matches, {span / 3600:.1f} hours of order books, {moves_seen} main-market moves (3c+ in 30s)")
print("enter L seconds after the move at the real ask (or bid to sell); exit at the real opposite price 60s / 300s after the move\n")
for X in EXITS:
    print(f"exit after {X}s:")
    for L in LAT:
        print(f"   enter after {L:2d}s: " + summary(trades[(L, X)]))
print(f"\nwaiting at the bid/ask instead (order rested 1s after the move, filled in {len(passive[60])} of {passive_tried} tries within 60s):")
for X in EXITS:
    print(f"   exit after {X}s: " + summary(passive[X]))
print("\nhow much of the side market's 5-minute move had already happened when you could enter (when it moved 2c+ the right way):")
for L in LAT:
    if stale[L]:
        print(f"   after {L:2d}s: {100 * np.median(stale[L]):3.0f}% done (median, n={len(stale[L])})")
