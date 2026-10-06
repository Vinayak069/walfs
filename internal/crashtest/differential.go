package crashtest

import (
	"fmt"
	"math/rand"

	"github.com/vinayak-singh/walfs/internal/blockdev"
	"github.com/vinayak-singh/walfs/internal/engine"
	"github.com/vinayak-singh/walfs/internal/format"
	"github.com/vinayak-singh/walfs/internal/fsck"
	"github.com/vinayak-singh/walfs/internal/model"
)

// DiffConfig configures a differential (engine vs. model) run.
type DiffConfig struct {
	Gen           GenConfig
	Ops           int
	Mode          engine.JournalMode
	ImageBlocks   int64
	JournalBlocks uint64
	CacheBlocks   int // small values stress buffer eviction
	DirtyLimit    int // small values stress forced commits
	SnapshotEvery int
	RemountEvery  int  // average ops between clean remounts (0: never)
	Async         bool // pipelined background commits
}

// DefaultDiff returns a configuration that is fast and exercises the interesting paths.
func DefaultDiff(seed int64, mode engine.JournalMode) DiffConfig {
	return DiffConfig{
		// Snapshots need a journal.
		Gen:           GenConfig{Seed: seed, Sparse: true, NoSnapshots: mode == engine.ModeNone},
		Ops:           1500,
		Mode:          mode,
		ImageBlocks:   16384,
		JournalBlocks: 256,
		CacheBlocks:   96,
		DirtyLimit:    48,
		SnapshotEvery: 150,
		RemountEvery:  400,
	}
}

type clockVar struct{ v int64 }

func (c *clockVar) now() int64 { return c.v }

// RunDifferential executes the same random operations on the engine and the
// model and returns an error describing the first divergence, fsck failure
// or internal inconsistency.
func RunDifferential(cfg DiffConfig) (err error) {
	dev := blockdev.NewMem(cfg.ImageBlocks * format.BlockSize)
	if _, err := format.Mkfs(dev, format.MkfsOptions{AssumeZero: true, JournalBlocks: cfg.JournalBlocks, Inodes: 2048, Now: 1000}); err != nil {
		return err
	}
	clk := &clockVar{v: 1000}
	open := func() (*engine.FS, error) {
		fs, _, err := engine.Open(dev, engine.Options{
			Mode: cfg.Mode, Clock: clk.now, CacheBlocks: cfg.CacheBlocks, DirtyLimitBlocks: cfg.DirtyLimit,
			AsyncCommit: cfg.Async,
		})
		return fs, err
	}
	fs, err := open()
	if err != nil {
		return err
	}
	m := model.New(clk.now)
	re := &Runner{FS: EngineVFS{fs}}
	rm := &Runner{FS: ModelVFS{m}}
	g := NewGen(cfg.Gen)
	rng := rand.New(rand.NewSource(cfg.Gen.Seed ^ 0x5eed))
	var hist []string
	fail := func(i int, f string, a ...any) error {
		tail := hist
		if len(tail) > 12 {
			tail = tail[len(tail)-12:]
		}
		return fmt.Errorf("seed %d op %d: %s\nrecent ops:\n  %s", cfg.Gen.Seed, i, fmt.Sprintf(f, a...), joinLines(tail))
	}
	snapCmp := func(i int, what string) error {
		sa, e1 := TakeFull(ModelVFS{m}, true)
		sb, e2 := TakeFull(EngineVFS{fs}, true)
		if e1 != nil || e2 != nil {
			return fail(i, "%s: snapshot error model=%v engine=%v", what, e1, e2)
		}
		if d := DiffFull(sa, sb); d != "" {
			return fail(i, "%s: tree mismatch:\n%s", what, d)
		}
		return nil
	}
	for i := 0; i < cfg.Ops; i++ {
		clk.v = 1000 + int64(i)*10
		op := g.Next(m)
		hist = append(hist, fmt.Sprintf("#%d %s", i, op))
		rEng := re.Apply(op)
		rMod := rm.Apply(op)
		if !SameResult(rEng, rMod) {
			return fail(i, "result mismatch for %s\n  engine: %s\n  model:  %s", op, rEng, rMod)
		}
		if a := fs.Aborted(); a != nil {
			return fail(i, "engine aborted: %v", a)
		}
		if cfg.SnapshotEvery > 0 && i%cfg.SnapshotEvery == cfg.SnapshotEvery-1 {
			if err := snapCmp(i, "periodic"); err != nil {
				return err
			}
		}
		if cfg.RemountEvery > 0 && rng.Intn(cfg.RemountEvery) == 0 {
			re.ReleaseAll()
			rm.ReleaseAll()
			if err := fs.Close(); err != nil {
				return fail(i, "close: %v", err)
			}
			if rep, err := fsck.Check(dev, fsck.Options{}); err != nil || !rep.OK() {
				return fail(i, "fsck after unmount: %v %v", err, rep)
			}
			if fs, err = open(); err != nil {
				return fail(i, "remount: %v", err)
			}
			re.FS = EngineVFS{fs}
			if err := snapCmp(i, "after remount"); err != nil {
				return err
			}
		}
	}
	if err := snapCmp(cfg.Ops, "final"); err != nil {
		return err
	}
	re.ReleaseAll()
	if err := fs.Close(); err != nil {
		return err
	}
	rep, err := fsck.Check(dev, fsck.Options{})
	if err != nil {
		return err
	}
	if !rep.OK() {
		return fmt.Errorf("seed %d: final fsck: %s", cfg.Gen.Seed, rep)
	}
	return nil
}

func joinLines(s []string) string {
	out := ""
	for i, l := range s {
		if i > 0 {
			out += "\n  "
		}
		out += l
	}
	return out
}
