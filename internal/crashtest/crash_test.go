package crashtest

import (
	"fmt"
	"testing"

	"github.com/Vinayak069/walfs/internal/engine"
)

func seedsFor(t *testing.T, n int) int {
	if testing.Short() {
		return max(1, n/3)
	}
	return n
}

// The central claim: whatever instant power is lost, and whichever of the
// unflushed writes reached the platter (reordered, torn at sector
// granularity), recovery yields a consistent filesystem that is exactly some
// operation-boundary state, never older than the last completed fsync.
func TestCrashConsistency(t *testing.T) {
	for _, mode := range []engine.JournalMode{engine.ModeOrdered, engine.ModeData} {
		for seed := int64(1); seed <= int64(seedsFor(t, 6)); seed++ {
			t.Run(fmt.Sprintf("%s/seed%d", mode, seed), func(t *testing.T) {
				t.Parallel()
				cfg := DefaultCrash(seed, mode)
				cfg.KeepData = true
				if testing.Short() {
					cfg.Ops = 120
					cfg.PointStride = 2
				}
				rep, err := RunCrash(cfg)
				if err != nil {
					t.Fatal(err)
				}
				t.Log(rep)
				if !rep.OK() {
					for _, f := range rep.Failures {
						t.Error(f)
					}
				}
				if rep.ReplayedTxnImages == 0 {
					t.Error("no crash image needed journal replay: workload does not exercise recovery")
				}
				if rep.StatesSeen < 3 {
					t.Errorf("only %d distinct recovered states seen", rep.StatesSeen)
				}
			})
		}
	}
}

// Ordered mode with in-place overwrites: structure and sizes must still be
// exact operation-boundary states (data bytes of overwritten blocks may
// legitimately be old or new, as in ext4 data=ordered).
func TestCrashConsistencyOrderedOverwrites(t *testing.T) {
	for seed := int64(100); seed < 100+int64(seedsFor(t, 4)); seed++ {
		t.Run(fmt.Sprintf("seed%d", seed), func(t *testing.T) {
			t.Parallel()
			cfg := DefaultCrash(seed, engine.ModeOrdered)
			cfg.Gen.NoOverwrite = false
			cfg.ExactData = false
			cfg.Ops = 200
			rep, err := RunCrash(cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Log(rep)
			for _, f := range rep.Failures {
				t.Error(f)
			}
		})
	}
}

// Negative controls: the harness must have teeth. Without a journal, and
// with a device that ignores cache flushes, violations must be found.
func TestCrashHarnessDetectsBrokenDesigns(t *testing.T) {
	t.Run("no-journal", func(t *testing.T) {
		cfg := DefaultCrash(3, engine.ModeNone)
		cfg.ExactData = false
		cfg.Gen.NoOverwrite = false
		cfg.RecoveryEvery = 0
		cfg.MaxFailures = 1
		rep, err := RunCrash(cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Log(rep)
		if rep.OK() {
			t.Fatal("harness failed to detect that ModeNone is not crash consistent")
		}
	})
	t.Run("flushes-ignored", func(t *testing.T) {
		cfg := DefaultCrash(3, engine.ModeData)
		cfg.DropFlushes = true
		cfg.RecoveryEvery = 0
		cfg.MaxFailures = 1
		rep, err := RunCrash(cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Log(rep)
		if rep.OK() {
			t.Fatal("harness failed to detect missing write barriers")
		}
	})
}

// Power loss while commits run in the background and operations continue to
// join the next transaction. The recovered state must still be an exact
// operation-boundary state no older than the last fsync.
func TestCrashConsistencyAsync(t *testing.T) {
	for _, mode := range []engine.JournalMode{engine.ModeOrdered, engine.ModeData} {
		for seed := int64(200); seed < 200+int64(seedsFor(t, 4)); seed++ {
			t.Run(fmt.Sprintf("%s/seed%d", mode, seed), func(t *testing.T) {
				t.Parallel()
				cfg := DefaultCrash(seed, mode)
				cfg.Async = true
				cfg.DirtyLimit = 16
				cfg.Ops = 250
				cfg.KeepData = true
				rep, err := RunCrash(cfg)
				if err != nil {
					t.Fatal(err)
				}
				t.Log(rep)
				for _, f := range rep.Failures {
					t.Error(f)
				}
				if rep.ReplayedTxnImages == 0 {
					t.Error("no crash image needed journal replay")
				}
			})
		}
	}
}
