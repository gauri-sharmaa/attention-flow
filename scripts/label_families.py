"""Label every follower in families.json: related to its leader or not, which
way it should move when the leader's YES price rises, topic, insider risk and
scheduled release.

These rules were written after reading all 389 families that have lag data
(event title, leader question, follower questions).
They encode the same judgments the `attnflow famllm` command asks a model to
make, so the training script can't tell the difference: the output has the
same shape (families_llm.json, keyed by follower condition ID).

Kinds of family seen, and the call made for each:
  - one winner among many (elections, champions, "best AI model", "next PM"):
    the other outcomes move opposite to the leader
  - buckets of one number (GDP, inflation, tweet counts, exact scores,
    valuation ranges, "released on <day>"): mutually exclusive, opposite
  - date ladders ("X by June", "X by December"): same direction
  - price thresholds ("reach $X", "dip to $Y", "FDV above $Z"): same if both
    point the same way, opposite if one is a rise and the other a fall
  - one match, many markets: the same side's handicap or set/map/game winner
    moves with the leader, the other side's against it; totals (O/U), both
    teams to score, coin tosses have no clear direction
  - lists of separate events that only share a theme ("Will Trump say X",
    "Who will Trump talk to", "Who will attend", Netflix earnings words,
    "IPOs before 2027", "What will happen before GTA VI"): not related
"""

import json
import os
import re
import sys

DIR = sys.argv[1] if len(sys.argv) > 1 else "data/pmt365"
fams = json.load(open(os.path.join(DIR, "families.json")))

UNRELATED = re.compile(
    r"\bsay\b|be said|talk to|speak to|meet with|\battend\b|named in|\bdie in\b|perform during|vote \"yea\"|"
    r"make the .* playoffs|before gta vi|trump today|ipos before|enter iran by june 30|demands will trump|"
    r"which cities will russia enter", re.I)
UNRELATED_EVENT = re.compile(r"trump today|what will .* say|who will trump|who will attend|who will enter|"
                             r"who will die|who will perform|which senators|which teams will make|ipos before|"
                             r"before gta vi|named in newly|iranian demands|which cities", re.I)
EXACT_DAY = re.compile(r"released on|\bon (january|february|march|april|may|june|july|august|september|october|"
                       r"november|december) \d|airdrop on|shutdown end|occur in|next strike", re.I)
BUCKET = re.compile(r"between|\d+\s*[-–]\s*\d+|or (below|more|above|greater)|less than|at least|exact score|"
                    r"\d+ fed rate|\d+ more millennium|be \d", re.I)
LADDER_EVENT = re.compile(r"by\.\.\.|by \.\.\.|by___|by ___|by \?|continues through|when will .* hit|"
                          r"out by|seen in public|by\.\.|\bby\b.*\?$", re.I)
UP = re.compile(r"\breach\b|\(high\)|above|over \$|\bhit\b(?!.*\(low\))|or higher|or more|last \d+ days or more|odds hit", re.I)
DOWN = re.compile(r"\bdip\b|\(low\)|or below|less than|odds dip", re.I)
TOTALS = re.compile(r"o/u|over/under|both teams to score|toss|completed match|penalty shootout|total sets|"
                    r"total kills|total rounds|games total", re.I)


def norm(s):
    return re.sub(r"[^a-z0-9 ]", "", s.lower()).strip()


def match_side(leader, follower):
    """For one-match families: +1 if the follower is about the leader's first-named side, -1 if the other, 0 if neither."""
    m = re.search(r"([^:]+?)\s+vs\.?\s+(.+?)(\s*\(bo\d\)|\s*-\s|$)", leader.split(": ", 1)[-1], re.I)
    if not m:
        return 0
    a, b = norm(m.group(1)), norm(m.group(2))
    f = follower.split(": ", 1)[-1] if ":" in follower else follower
    fm = re.search(r"(.+?)\s*(\([+-][\d.]+\))?\s+vs\.?\s+(.+?)\s*(\([+-][\d.]+\))?(\s*-\s.*)?$", f, re.I)
    if not fm:
        return 0
    x, y = norm(fm.group(1)), norm(fm.group(3))

    def same(p, q):
        p, q = p.split(), q.split()
        return bool(p and q) and (p[-1] in q or q[-1] in p or p[0] in q or q[0] in p)
    if same(y, b) or same(x, a):
        return 1
    if same(y, a) or same(x, b):
        return -1
    return 0


def direction(event, L, F):
    ev, l, f = event.lower(), L.lower(), F.lower()
    if UNRELATED_EVENT.search(event) or (UNRELATED.search(F) and UNRELATED.search(L)):
        return False, "unclear"
    # exact scores of one match are mutually exclusive
    if "exact score" in f and "exact score" in l:
        return True, "opposite"
    # one match, many markets
    if re.search(r"\bvs\.?\b", event, re.I) and not re.search(r"win on \d", l):
        if TOTALS.search(F):
            return True, "unclear"
        if l.startswith("spread:") and f.startswith(("spread:", "1h spread:")):
            lt, ft = norm(L.split(":")[1].split("(")[0]), norm(F.split(":")[1].split("(")[0])
            return True, "same" if lt == ft else "opposite"
        s = match_side(L, F)
        return True, {1: "same", -1: "opposite"}.get(s, "unclear")
    # football 1X2: the other team and the draw both move against the leader's team
    if re.search(r"win on \d", l):
        return True, "opposite"
    # combos share some legs with the leader but not all
    if "combo" in ev:
        return True, "unclear"
    # exact-day and month buckets are mutually exclusive
    if EXACT_DAY.search(F) or EXACT_DAY.search(L) or "in which month" in ev:
        return True, "opposite"
    # "no release / not IPO" against an outcome that requires one
    if re.search(r"\bno\b.*release|not ipo|\bno one\b|no next|no listed|not meet", l) and not re.search(r"\bno\b|not ipo|no one", f):
        return True, "opposite"
    # price thresholds
    if UP.search(F) or DOWN.search(F):
        lu, ld = bool(UP.search(L)) and not DOWN.search(L), bool(DOWN.search(L))
        fu, fd = bool(UP.search(F)) and not DOWN.search(F), bool(DOWN.search(F))
        if (lu and fd) or (ld and fu):
            return True, "opposite"
        if (lu and fu) or (ld and fd):
            if BUCKET.search(F) and not re.search(r"or (more|higher|above)|at least|over \$|above \$|reach|dip|hit", f):
                return True, "opposite"
            return True, "same"
    # buckets of one number
    if BUCKET.search(F) and BUCKET.search(L) and not re.search(r"\bby\b (jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)", f):
        return True, "opposite"
    # date ladders: the same thing happening by different dates
    if LADDER_EVENT.search(event) or re.search(r"\b(by|before|through|in 20\d\d)\b", f) and norm(re.sub(r"(by|before|through|in) .*", "", f)) == norm(re.sub(r"(by|before|through|in) .*", "", l)):
        return True, "same"
    # everything else in one event: one winner among many
    return True, "opposite"


TOPIC_RULES = [
    ("sports", r"\bvs\.?\b|win on \d|champion|nba|nhl|mlb|mls|wnba|epl|laliga|ligue 1|premier league|world cup|ballon|masters|open\b|"
               r"wimbledon|roland|counter-strike|dota|lol\b|valorant|esl|worlds 20|golden boot|goalscorer|afc|nfc|super bowl|lebron|mvp"),
    ("crypto", r"bitcoin|ethereum|solana|xrp|fdv|airdrop|token|megaeth|microstrategy|public sale|lighter|monad|opensea|ink fdv|predict.fun|base launch|metamask"),
    ("economy", r"gdp|inflation|fed\b|fed |ecb|jobs|treasury|crude|wti|oil|rials|shutdown|tariff|interest rate"),
    ("tech_ai", r"gemini|openai|anthropic|fable|grok|kimi|ai model|millennium|tesla|ipo|discord|largest company|third-largest|data center|spacex"),
    ("elections", r"election|nominee|nomination|mayor|governor|senate|house seat|ballot|primary|prime minister|parliament|seats|tx-36"),
    ("geopolitics", r"iran|israel|russia|ukraine|venezuela|cuba|hezbollah|houthi|syria|hormuz|ceasefire|strike|military|lebanon|maduro|netanyahu|starmer|leader|putin|airspace|perim|cartel|khamenei|pahlavi|machado"),
    ("entertainment", r"oscar|movie|film|bond|spotify|song|stranger things|gross|opening week|time.*person|nobel|google this year|resident evil"),
    ("weather_science", r"earthquake|precipitation|el niño|el nino|roni|aliens"),
    ("politics", r"trump|epstein|alito|clarity act|fed chair|minister of justice|deport"),
]


def topic(text):
    t = text.lower()
    for name, rx in TOPIC_RULES:
        if re.search(rx, t):
            return name
    return "other"


HIGH_INSIDER = re.compile(r"\bsay\b|be said|tweets|released on|release by|be released|airdrop|launch a token|trump today|"
                          r"die in|perform during|announced as next james bond|frontier model|ipo valuation|net(flix)?", re.I)
MEDIUM_INSIDER = re.compile(r"ipo|nominate|confirmed as|next prime minister|minister of justice|fed chair|agreement|"
                            r"meeting|deal|sale announced|retirement|sells any bitcoin|data center|fsd|blockade", re.I)
RELEASES = [
    (r"gdp", "official GDP release"), (r"inflation", "official CPI release"), (r"jobs added|add at least|lose between", "BLS jobs report, first Friday of the month"),
    (r"ecb", "ECB rate decision, scheduled meeting day"), (r"fed decision|fed rate hike|fed hike", "FOMC decision, scheduled meeting day, 2pm ET"),
    (r"precipitation", "monthly rainfall total, published after month end"), (r"tweets", "tweet count at month end"),
    (r"domestic gross|opening week", "weekend box office reports"), (r"top song", "Spotify year-end chart"),
    (r"oscar nominations", "Oscar nominations announcement"), (r"earnings call", "company earnings call"),
]

out = {}
counts = {}
for fam in fams:
    for m in fam["followers"]:
        rel, d = direction(fam["event"], fam["leader"]["Question"], m["Question"])
        text = fam["event"] + " " + m["Question"]
        ins = "high" if HIGH_INSIDER.search(text) else "medium" if MEDIUM_INSIDER.search(text) else "low"
        rel_s = next((r for rx, r in RELEASES if re.search(rx, text, re.I)), "")
        out[m["ConditionID"]] = {"id": m["ConditionID"], "related": rel, "direction": d, "topic": topic(text),
                                 "insider_risk": ins, "release": rel_s}
        counts[(rel, d)] = counts.get((rel, d), 0) + 1

json.dump(out, open(os.path.join(DIR, "families_llm.json"), "w"), indent=1)
print(f"labelled {len(out)} followers")
for k, v in sorted(counts.items(), key=lambda kv: -kv[1]):
    print(f"   related={k[0]!s:5s} direction={k[1]:9s} {v}")
