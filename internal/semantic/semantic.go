// Package semantic builds the candidate graph: which entity pairs are worth
// testing for a lead-lag relationship at all.
//
// Testing every pair is O(n²·L) per bar and, worse, multiplies false
// discoveries. So we only test pairs whose descriptions look related, then let
// the price data accept or reject them. The representation is TF-IDF over
// name tokens, tags, subtopic and (when fetched) the Wikipedia extract. It is
// dependency-free; swapping in sentence embeddings only changes Vectors().
package semantic

import (
	"math"
	"sort"
	"strings"
	"unicode"

	"github.com/gauri-sharmaa/attention-flow/internal/core"
)

// Pair is an unordered candidate pair (A < B) with its similarity.
type Pair struct {
	A, B int
	Sim  float64
}

var stop = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`a an and are as at be by for from has have in is it its of on or that the
		to was were which with this also their they he she his her who after into over more most than other such
		company american based founded known one first used use may can been including inc ltd corporation`) {
		stop[w] = true
	}
}

func tokens(s string) []string {
	f := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	out := f[:0]
	for _, w := range f {
		if len(w) > 1 && !stop[w] {
			out = append(out, w)
		}
	}
	return out
}

// docTerms returns weighted terms for one entity. Structured fields (name,
// tags) are weighted above free text so a long extract cannot drown them.
func docTerms(e core.Entity) map[string]float64 {
	tf := map[string]float64{}
	for _, w := range tokens(e.Name) {
		tf[w] += 3
	}
	for _, t := range e.Tags {
		for _, w := range tokens(t) {
			tf[w] += 2
		}
	}
	tf["sub:"+e.Subtopic] += 2
	for _, w := range tokens(e.Text) {
		tf[w] += 0.25
	}
	return tf
}

// Vectors returns L2-normalised TF-IDF vectors as sparse maps.
func Vectors(u *core.Universe) []map[string]float64 {
	docs := make([]map[string]float64, len(u.Entities))
	df := map[string]int{}
	for i, e := range u.Entities {
		docs[i] = docTerms(e)
		for w := range docs[i] {
			df[w]++
		}
	}
	n := float64(len(docs))
	for _, d := range docs {
		norm := 0.0
		for w, tf := range d {
			v := (1 + math.Log(tf+1)) * math.Log((n+1)/float64(df[w]+1))
			d[w] = v
			norm += v * v
		}
		norm = math.Sqrt(norm)
		for w := range d {
			d[w] /= norm
		}
	}
	return docs
}

func cosine(a, b map[string]float64) float64 {
	if len(a) > len(b) {
		a, b = b, a
	}
	s := 0.0
	for w, v := range a {
		s += v * b[w]
	}
	return s
}

// Candidates returns, for each entity, its k most similar entities, merged into
// a symmetric set of unordered pairs with similarity at least minSim.
func Candidates(u *core.Universe, k int, minSim float64) []Pair {
	vec := Vectors(u)
	n := len(vec)
	sims := make([]float64, n*n)
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			s := cosine(vec[i], vec[j])
			sims[i*n+j], sims[j*n+i] = s, s
		}
	}
	keep := map[[2]int]bool{}
	idx := make([]int, 0, n)
	for i := 0; i < n; i++ {
		idx = idx[:0]
		for j := 0; j < n; j++ {
			if j != i && sims[i*n+j] >= minSim {
				idx = append(idx, j)
			}
		}
		sort.Slice(idx, func(a, b int) bool { return sims[i*n+idx[a]] > sims[i*n+idx[b]] })
		if len(idx) > k {
			idx = idx[:k]
		}
		for _, j := range idx {
			a, b := min(i, j), max(i, j)
			keep[[2]int{a, b}] = true
		}
	}
	out := make([]Pair, 0, len(keep))
	for p := range keep {
		out = append(out, Pair{A: p[0], B: p[1], Sim: sims[p[0]*n+p[1]]})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].A != out[j].A {
			return out[i].A < out[j].A
		}
		return out[i].B < out[j].B
	})
	return out
}
