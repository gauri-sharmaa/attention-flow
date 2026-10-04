package semantic

import (
	"testing"

	"github.com/gauri-sharmaa/attention-flow/internal/core"
)

func TestCandidatesLinkRelatedEntities(t *testing.T) {
	u, err := core.LoadUniverse("../../data/universe.txt")
	if err != nil {
		t.Fatal(err)
	}
	id := map[string]int{}
	for _, e := range u.Entities {
		id[e.Name] = e.ID
	}
	cand := Candidates(u, 12, 0.05)
	has := map[[2]int]bool{}
	for _, p := range cand {
		if p.A >= p.B {
			t.Fatalf("pair not ordered: %+v", p)
		}
		has[[2]int{p.A, p.B}] = true
	}
	pair := func(a, b string) bool {
		x, y := id[a], id[b]
		return has[[2]int{min(x, y), max(x, y)}]
	}
	for _, want := range [][2]string{
		{"OpenAI", "ChatGPT"}, {"Nvidia", "Jensen Huang"}, {"Coinbase", "Brian Armstrong"},
		{"Bitcoin", "Bitcoin ETF"}, {"Apple", "iPhone"}, {"FTX", "Sam Bankman-Fried"},
	} {
		if !pair(want[0], want[1]) {
			t.Errorf("missing candidate %s ↔ %s", want[0], want[1])
		}
	}
	if pair("Netflix", "Aave") {
		t.Error("unrelated pair Netflix ↔ Aave is a candidate")
	}
	n := len(u.Entities)
	if len(cand) > n*(n-1)/2/5 {
		t.Errorf("%d candidates is more than a fifth of all %d pairs: pruning is not pruning", len(cand), n*(n-1)/2)
	}
}
