package critic

import (
	"math/rand"
	"testing"
)

// TestSpreadKeepsConcurrencyBound: however the faults are drawn, no more than maxc are ever open
// at once, including when pushing a fault later reorders it past the faults drawn after it.
func TestSpreadKeepsConcurrencyBound(t *testing.T) {
	for seed := int64(0); seed < 500; seed++ {
		rng := rand.New(rand.NewSource(seed))
		maxc := 1 + rng.Intn(3)
		var fs []GenFault
		for i := 0; i < 2+rng.Intn(8); i++ {
			at := float64(rng.Intn(60))
			fs = append(fs, GenFault{At: at, Clear: at + 10 + float64(rng.Intn(30))})
		}
		n := len(fs)
		fs = spread(fs, maxc)
		if len(fs) != n {
			t.Fatalf("seed %d: %d faults became %d", seed, n, len(fs))
		}
		for _, f := range fs {
			open := 0
			for _, g := range fs {
				if g.At <= f.At && g.Clear > f.At {
					open++
				}
			}
			if open > maxc {
				t.Fatalf("seed %d: %d open at %.1fs, max %d: %+v", seed, open, f.At, maxc, fs)
			}
		}
	}
}
