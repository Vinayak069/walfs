package crashtest

import (
	"fmt"
	"os"
	"strconv"
	"testing"

	"github.com/vinayak-singh/walfs/internal/engine"
)

// TestDifferentialLong is a soak test: WALFS_SOAK=<n> runs n seeds per mode.
func TestDifferentialLong(t *testing.T) {
	n, _ := strconv.Atoi(os.Getenv("WALFS_SOAK"))
	if n == 0 {
		t.Skip("set WALFS_SOAK=<seeds> to run")
	}
	for _, mode := range []engine.JournalMode{engine.ModeOrdered, engine.ModeData} {
		for seed := int64(100); seed < int64(100+n); seed++ {
			t.Run(fmt.Sprintf("%s/seed%d", mode, seed), func(t *testing.T) {
				t.Parallel()
				cfg := DefaultDiff(seed, mode)
				cfg.Ops = 6000
				cfg.Gen.MaxWrite = 100 << 10
				cfg.Gen.MaxFile = 3 << 20
				cfg.Gen.MaxData = 6 << 20
				cfg.Gen.Names = 5 + int(seed%6)
				if err := RunDifferential(cfg); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
