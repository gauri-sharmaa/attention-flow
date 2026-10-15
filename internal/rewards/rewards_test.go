package rewards

import (
	"math"
	"testing"

	"github.com/gauri-sharmaa/attention-flow/internal/source"
)

func TestScore(t *testing.T) {
	if got := Score(3, 1, 100); math.Abs(got-100*4.0/9) > 1e-9 {
		t.Errorf("Score(3,1,100) = %v, want 44.4", got)
	}
	if Score(3, 3, 100) != 0 || Score(3, 4, 100) != 0 {
		t.Error("orders at or past the band edge score nothing")
	}
	if Credit(90, 0, 0.5) != 30 {
		t.Error("one-sided maker in the middle gets a third")
	}
	if Credit(90, 0, 0.95) != 0 {
		t.Error("one-sided maker near the edge gets nothing")
	}
	bq, aq := BookScores([]source.Level{{Price: 0.49, Size: 100}, {Price: 0.48, Size: 10}}, []source.Level{{Price: 0.52, Size: 100}}, 0.50, 3, 50)
	if math.Abs(bq-100*4.0/9) > 1e-9 || math.Abs(aq-100*1.0/9) > 1e-9 {
		t.Errorf("BookScores = %v %v", bq, aq)
	}
}
