package source

import (
	"slices"
	"testing"

	"github.com/gauri-sharmaa/attention-flow/internal/core"
)

func TestMatcher(t *testing.T) {
	u := &core.Universe{}
	for i, n := range []string{"OpenAI", "Sam Altman", "Apple", "Base", "GPT-4", "Bitcoin ETF"} {
		u.Entities = append(u.Entities, core.Entity{ID: i, Name: n})
	}
	m := NewMatcher(u)
	cases := []struct {
		text string
		want []int
	}{
		{"OpenAI and Sam Altman, again.", []int{0, 1}},
		{"sam altman lowercase does not count", nil},
		{"apple pie is not Apple.", []int{2}},
		{"the base case", nil},
		{"GPT-4 vs GPT-4o", []int{4}},
		{"spot Bitcoin ETF flows", []int{5}},
		{"ends with Sam", nil},
		{"OpenAI OpenAI OpenAI", []int{0}},
	}
	for _, c := range cases {
		got := m.Match(c.text, nil)
		slices.Sort(got)
		if !slices.Equal(got, c.want) && !(len(got) == 0 && len(c.want) == 0) {
			t.Errorf("Match(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}
