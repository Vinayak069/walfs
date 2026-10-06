package crashtest

import (
	"fmt"
	"testing"

	"github.com/vinayak-singh/walfs/internal/engine"
)

func TestDifferential(t *testing.T) {
	seeds := 6
	if testing.Short() {
		seeds = 2
	}
	for _, mode := range []engine.JournalMode{engine.ModeOrdered, engine.ModeData, engine.ModeNone} {
		for seed := int64(1); seed <= int64(seeds); seed++ {
			t.Run(fmt.Sprintf("%s/seed%d", mode, seed), func(t *testing.T) {
				t.Parallel()
				if err := RunDifferential(DefaultDiff(seed, mode)); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
