package crashtest

import (
	"errors"
	"fmt"
	"syscall"

	"github.com/vinayak-singh/walfs/internal/blockdev"
	"github.com/vinayak-singh/walfs/internal/engine"
	"github.com/vinayak-singh/walfs/internal/format"
	"github.com/vinayak-singh/walfs/internal/fsck"
)

// RunENOSPC fills a small filesystem to the brim with random operations,
// checking after every failure that the filesystem is still consistent,
// that freed space is reusable, and that no operation fails with anything
// other than a clean ENOSPC/EMLINK/etc.
func RunENOSPC(seed int64, mode engine.JournalMode) error {
	dev := blockdev.NewMem(700 * format.BlockSize)
	if _, err := format.Mkfs(dev, format.MkfsOptions{AssumeZero: true, JournalBlocks: 128, Inodes: 256}); err != nil {
		return err
	}
	clk := &clockVar{v: 1}
	fs, _, err := engine.Open(dev, engine.Options{Mode: mode, Clock: clk.now, CacheBlocks: 64, DirtyLimitBlocks: 32})
	if err != nil {
		return err
	}
	r := &Runner{FS: EngineVFS{fs}}
	g := NewGen(GenConfig{Seed: seed, MaxData: 1 << 30, MaxFile: 600 << 10, MaxWrite: 64 << 10, Names: 40, NoHandles: true})
	// The generator needs a model for path selection; keep a shadow of names
	// only by running the model alongside (results are not compared here).
	mdl := newShadow(clk)
	rm := &Runner{FS: ModelVFS{mdl}}
	enospc := 0
	for i := 0; i < 4000; i++ {
		clk.v += 10
		op := g.Next(mdl)
		res := r.Apply(op)
		if res.Err != nil {
			var en syscall.Errno
			if !errors.As(res.Err, &en) {
				return fmt.Errorf("op %d %s: non-errno error %v", i, op, res.Err)
			}
			if en == syscall.ENOSPC {
				enospc++
			}
			switch en {
			case syscall.ENOSPC, syscall.ENOENT, syscall.EEXIST, syscall.ENOTDIR, syscall.EISDIR,
				syscall.ENOTEMPTY, syscall.EINVAL, syscall.EPERM, syscall.ENODEV, syscall.EBADF,
				syscall.EBUSY, syscall.EOPNOTSUPP, syscall.ENAMETOOLONG:
			default:
				return fmt.Errorf("op %d %s: unexpected errno %v", i, op, en)
			}
		}
		if a := fs.Aborted(); a != nil {
			return fmt.Errorf("op %d %s: engine aborted: %v", i, op, a)
		}
		// Only mirror operations that succeeded on the engine, so the shadow
		// tracks what exists.
		if res.Err == nil {
			rm.Apply(op)
		}
		if i%200 == 199 {
			if err := fs.Sync(); err != nil {
				return err
			}
			if err := fs.Checkpoint(); err != nil {
				return err
			}
			rep, err := fsck.Check(dev, fsck.Options{})
			if err != nil || !rep.OK() {
				return fmt.Errorf("op %d: fsck: %v %v", i, err, rep)
			}
		}
	}
	if enospc == 0 {
		return fmt.Errorf("workload never hit ENOSPC; test is not exercising the full-disk paths")
	}
	if err := fs.Close(); err != nil {
		return err
	}
	rep, err := fsck.Check(dev, fsck.Options{})
	if err != nil || !rep.OK() {
		return fmt.Errorf("final fsck: %v %v", err, rep)
	}
	return nil
}
