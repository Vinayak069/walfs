package crashtest

import (
	"fmt"
	"testing"

	"github.com/vinayak-singh/walfs/internal/engine"
)

func TestMany(t *testing.T) {
	for _, mode := range []engine.JournalMode{engine.ModeOrdered, engine.ModeData, engine.ModeNone} {
		for seed := int64(100); seed < 160; seed++ {
			seed, mode := seed, mode
			t.Run(fmt.Sprintf("%s/%d", mode, seed), func(t *testing.T) {
				t.Parallel()
				c := DefaultDiff(seed, mode)
				c.Ops = 4000
				if err := RunDifferential(c); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
