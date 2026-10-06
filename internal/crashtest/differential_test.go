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

// Big directories: thousands of names spill directories over many blocks and
// exercise rec_len merging, in-block splitting and the dir index.
func TestDifferentialBigDirs(t *testing.T) {
	for _, mode := range []engine.JournalMode{engine.ModeOrdered, engine.ModeData} {
		t.Run(mode.String(), func(t *testing.T) {
			t.Parallel()
			cfg := DefaultDiff(77, mode)
			cfg.Gen.Names = 600
			cfg.Gen.MaxData = 1 << 20
			cfg.Ops = 6000
			cfg.SnapshotEvery = 500
			if err := RunDifferential(cfg); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Tiny device: ENOSPC must be reported identically to... itself, i.e. leave
// the filesystem consistent and usable. (The model has no capacity limit, so
// this test drives the engine only and relies on fsck.)
func TestENOSPCConsistency(t *testing.T) {
	for _, mode := range []engine.JournalMode{engine.ModeOrdered, engine.ModeData} {
		t.Run(mode.String(), func(t *testing.T) {
			t.Parallel()
			if err := RunENOSPC(5, mode); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Compare the live tree and every snapshot after *every* operation, so a
// divergence is pinned to the exact operation that caused it.
func TestDifferentialSnapshotsTight(t *testing.T) {
	for _, mode := range []engine.JournalMode{engine.ModeOrdered, engine.ModeData} {
		for seed := int64(1); seed <= 4; seed++ {
			t.Run(fmt.Sprintf("%s/seed%d", mode, seed), func(t *testing.T) {
				t.Parallel()
				cfg := DefaultDiff(seed, mode)
				cfg.Ops = 500
				cfg.SnapshotEvery = 1
				cfg.RemountEvery = 120
				if err := RunDifferential(cfg); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

// The same random workloads with commits running in the background: results
// and trees must still match the model exactly. Run with -race.
func TestDifferentialAsync(t *testing.T) {
	for _, mode := range []engine.JournalMode{engine.ModeOrdered, engine.ModeData} {
		for seed := int64(1); seed <= 4; seed++ {
			t.Run(fmt.Sprintf("%s/seed%d", mode, seed), func(t *testing.T) {
				t.Parallel()
				cfg := DefaultDiff(seed, mode)
				cfg.Async = true
				cfg.Ops = 1200
				cfg.DirtyLimit = 16 // soft threshold 4: background commits start constantly
				if err := RunDifferential(cfg); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
