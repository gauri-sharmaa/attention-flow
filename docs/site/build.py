"""Fill docs/site/template.html with the real results and write index.html.

Inputs (all produced by attnflow commands, see README):
  docs/site/links.json                           hawkes -json (on the 90-day data)
  docs/results/real-365d-hawkes-walkforward.txt  hawkes -folds 11
  docs/results/real-365d-activity-to-price.txt   hawkes -moves 0.02 -folds 11
  docs/results/real-365d-price-leadlag.txt       leadlag -periods 12
  docs/results/real-365d-paper-trading.txt       backtest -periods 12
  docs/results/real-outside-attention-eventstudy.txt  eventstudy
"""
import json
import re
import sys
from pathlib import Path

root = Path(__file__).resolve().parent
res = root.parent / "results"


def read(name):
    return (res / name).read_text()


def folds(text):
    return [(d, float(g), float(p)) for d, g, p in
            re.findall(r"test (\S+–\S+ \d+|\S+ \d+–\S+ \d+)\s.*?gain ([+-][\d.]+)\s+placebo ([+-][\d.]+)", text)]


net = json.loads((root / "links.json").read_text())
# Show links between different events with real strength; sibling outcomes of
# one event (e.g. "by Oct 31" / "by Dec 31") trade together trivially.
net["links"] = [l for l in net["links"] if not l["sameEvent"] and l["branch"] >= 0.05]
used = {l["From"] for l in net["links"]} | {l["To"] for l in net["links"]}
net["nodes"] = [n for n in net["nodes"] if n["id"] in used]
hw = folds(read("real-365d-hawkes-walkforward.txt"))
mv = folds(read("real-365d-activity-to-price.txt"))
ll = read("real-365d-price-leadlag.txt")
bt = read("real-365d-paper-trading.txt")
es = read("real-outside-attention-eventstudy.txt")


def cells(fs):
    return [{"ok": g > 0 and g > p, "t": f"{d}: gain {g:+.3f}, placebo {p:+.3f}"} for d, g, p in fs]


# Price lead-lag: significant pairs per period vs chance, and stability.
periods = re.findall(r"p≤1%\s+(\d+) \(chance ≈ (\d+)\)", ll)
nlead = re.findall(r"→ \d+\s+(\d+) leads", ll)
same = re.findall(r"same leader next period (\d+) \(", ll)
held = re.search(r"leads that hold in at least two periods: (\d+)", ll).group(1)
leads = []
for m in re.finditer(r"(\d) periods\s+([\d.]+(?:s|m)) ahead\s+(.+)\n\s+→ (.+)", ll):
    lag = m.group(2).replace("m", " min") if m.group(2).endswith("m") else m.group(2)
    leads.append({"lag": lag, "from": m.group(3).strip(), "to": m.group(4).strip()})

# Event study: hour before/after and the 10-minute profiles.
prof = re.findall(r"by 10 min, −2h … \+2h: (.+)", es)
parse = lambda s: [float(x) for x in s.replace("|", "").split()]
surge, plac = parse(prof[0]), parse(prof[1])
nsurge = re.search(r"surges\s+(\d+)", es).group(1)

# Paper trading: per-period lines, in order.
def per(label):
    return [float(v) for v in re.findall(label + r"\s+\d+ trades · win\s+[\d.]+% · avg\s+([+-][\d.]+)%", bt)]


def avg(label):
    vals = per(label)
    return sum(vals) / len(vals)


pdates = re.findall(r"period \d+\s+(\S+ \d+–\S+ \d+)\s+chosen", bt)
ll_dates = re.findall(r"period \d+\s+(\S+ \d+–\S+ \d+)\s+tested", ll)
mvc, hwc = cells(mv), cells(hw)
recent = sum(c["ok"] for c in mvc[-4:])
score = [
    {"q": "Does trading activity predict price moves?", "how": "Hawkes, trades → 2¢ price moves",
     "cells": mvc, "tone": "mixed", "a": "recently, yes" if recent == 4 else "mixed"},
    {"q": "Does trading in one market set off trading in related markets?", "how": "Hawkes, raw trade times",
     "cells": hwc, "tone": "mixed", "a": "some months"},
    {"q": "Do some prices move before related prices?", "how": "Hoffmann–Rosenbaum–Yoshida, linked pairs at 1% vs chance",
     "cells": [{"ok": int(a) > 3 * int(b), "t": f"{d}: {a} pairs vs {b} by chance"} for d, (a, b) in zip(ll_dates, periods)],
     "tone": "yes", "a": "linked, leader flips"},
    {"q": "Is the order-flow signal profitable after costs?", "how": "paper trading, average return per trade, 1¢ per side",
     "cells": [{"ok": v > 0, "t": f"{d}: {v:+.1f}% per trade"} for d, v in zip(pdates, per("out of sample"))],
     "tone": "no", "a": "no"},
    {"q": "Does news, Reddit or HN buzz lead trading?", "how": f"event study, {nsurge} surges (90 days), vs same time on other days",
     "cells": [], "tone": "no", "a": "no"},
]

data = {
    "net": net,
    "score": score,
    "scoreCaption": f"A year of every trade on 1,205 markets, Oct 2025 to Oct 2026. Each test is fit on one 30-day period and scored on the next, which it never saw. The placebo shifts streams by whole days, keeping each market's rhythm and destroying real timing. A month passes when the result beats zero and the placebo. Activity predicted price moves in the last four months, not before: the 90-day run only saw those months.",
    "leadCaption": f"Linked prices move together every month ({min(int(a) for a, _ in periods)}–{max(int(a) for a, _ in periods)} pairs a month, against {min(int(b) for _, b in periods)}–{max(int(b) for _, b in periods)} by chance), but which one moves first holds from one month to the next only about {round(100 * sum(int(x) for x in same) / sum(int(x) for x in nlead))}% of the time, a coin flip. {held} pairs keep a consistent leader across months, mostly the same question at different dates or prices: the busier contract reprices first.",
    "leads": leads[:8],
    "bt": [
        {"label": "Signal, 1¢ spread", "sub": "realistic", "v": avg("out of sample")},
        {"label": "Signal, no costs", "sub": "upper bound", "v": avg("same, no costs"), "dim": True},
        {"label": "Random side, 1¢ spread", "sub": "placebo", "v": avg("placebo: random side"), "dim": True},
    ],
    "esCaption": f"{nsurge} sudden surges in news, Reddit and Hacker News mentions of names in market questions. Trading in those markets, compared with the same clock time on other days, stays flat before and after the surge: no reaction and no lead. Surge days are a few percent busier overall, about the same as at random times.",
    "es": {"surge": surge, "placebo": plac},
}
html = (root / "template.html").read_text().replace("/*DATA*/null", json.dumps(data, ensure_ascii=False))
(root / "index.html").write_text(html)
print("wrote", root / "index.html", f"({len(html)//1024} KB)")
