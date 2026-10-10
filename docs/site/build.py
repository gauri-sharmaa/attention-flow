"""Fill docs/site/template.html with the real results and write index.html.

Inputs (all produced by attnflow commands, see README):
  docs/site/links.json                          hawkes -json
  docs/results/real-90d-hawkes-walkforward.txt  hawkes -folds 2
  docs/results/real-90d-activity-to-price.txt   hawkes -moves 0.02 -folds 2
  docs/results/real-90d-price-leadlag.txt       leadlag -periods 3
  docs/results/real-90d-paper-trading.txt       backtest -periods 3
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
    return [(float(g), float(p)) for g, p in re.findall(r"gain ([+-][\d.]+)\s+placebo ([+-][\d.]+)", text)]


net = json.loads((root / "links.json").read_text())
hw = folds(read("real-90d-hawkes-walkforward.txt"))
mv = folds(read("real-90d-activity-to-price.txt"))
ll = read("real-90d-price-leadlag.txt")
bt = read("real-90d-paper-trading.txt")
es = read("real-outside-attention-eventstudy.txt")


def fmt(x, d=3):
    return f"{x:+.{d}f}"


def placebo(fs):
    return f"{min(p for _, p in fs):+.3f} to {max(p for _, p in fs):+.3f}"


# Price lead-lag: significant pairs per period vs chance, and stability.
periods = re.findall(r"p≤1%\s+(\d+) \(chance ≈ (\d+)\)", ll)
stab = re.findall(r"same leader next period \d+ \((\d+)%\)", ll)
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

# Paper trading: average return per trade lines for the combined test set.
def avg(label):
    vals = [float(v) for v in re.findall(label + r"\s+\d+ trades · win\s+[\d.]+% · avg\s+([+-][\d.]+)%", bt)]
    return sum(vals) / len(vals)

score = [
    {"q": "Does trading in one market set off trading in related markets?", "how": "Hawkes, raw trade times, 382 markets",
     "f1": fmt(hw[0][0]), "f2": fmt(hw[1][0]), "pl": placebo(hw), "ok": all(g > 0 and g > p for g, p in hw), "a": "yes" if all(g > 0 and g > p for g, p in hw) else "no"},
    {"q": "Does trading activity predict price moves?", "how": "Hawkes, trades → 2¢ price moves",
     "f1": fmt(mv[0][0]), "f2": fmt(mv[1][0]), "pl": placebo(mv), "ok": all(g > 0 and g > p for g, p in mv), "a": "yes" if all(g > 0 and g > p for g, p in mv) else "no"},
    {"q": "Do some prices move before related prices?", "how": "Hoffmann–Rosenbaum–Yoshida, significant pairs at 1% vs chance",
     "f1": f"{periods[1][0]} vs {periods[1][1]}", "f2": f"{periods[2][0]} vs {periods[2][1]}", "pl": "–", "ok": True, "a": "linked, leader unstable"},
    {"q": "Is the order-flow signal profitable after costs?", "how": "paper trading, 1¢ per side, walk-forward",
     "f1": f"{avg('out of sample'):+.1f}%", "f2": "", "pl": f"{avg('placebo: random side'):+.1f}%", "ok": False, "a": "no"},
    {"q": "Does news, Reddit or HN buzz lead trading?", "how": f"event study, {nsurge} surges, vs same time on other days",
     "f1": "flat", "f2": "", "pl": "flat", "ok": False, "a": "no"},
]
score[3]["f2"] = "per trade"
score[4]["f2"] = "before & after"

data = {
    "net": net,
    "score": score,
    "leadCaption": f"Linked prices move together every month ({', '.join(f'{a} pairs vs {b} by chance' for a, b in periods)}), but which one moves first holds from one month to the next only about {stab[0]}% of the time, a coin flip. {held} pairs keep a consistent leader across months, mostly the same question at different dates or prices: the busier contract reprices first.",
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
