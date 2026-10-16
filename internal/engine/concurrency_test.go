package engine

import (
	"bytes"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/Vinayak069/walfs/internal/blockdev"
	"github.com/Vinayak069/walfs/internal/format"
)

// Many goroutines hammer the filesystem while commits run in the background.
// Each worker owns a directory and a private model of it, so every outcome is
// checkable exactly; extra goroutines read and list concurrently, and another
// takes snapshots. At the end the tree must match every model, survive a
// remount, and pass fsck. Meant to be run under the race detector.
func TestConcurrentWorkloadAsyncCommit(t *testing.T) {
	for _, mode := range []JournalMode{ModeOrdered, ModeData} {
		t.Run(mode.String(), func(t *testing.T) {
			dev := blockdev.NewMem(32768 * format.BlockSize)
			if _, err := format.Mkfs(dev, format.MkfsOptions{AssumeZero: true, JournalBlocks: 512, Inodes: 4096}); err != nil {
				t.Fatal(err)
			}
			opts := Options{Mode: mode, AsyncCommit: true, CacheBlocks: 512, DirtyLimitBlocks: 96, CommitInterval: 20 * time.Millisecond}
			fs, _, err := Open(dev, opts)
			if err != nil {
				t.Fatal(err)
			}
			const workers = 6
			const opsPer = 400
			models := make([]map[string][]byte, workers)
			var wg sync.WaitGroup
			errs := make(chan error, workers+4)
			for w := 0; w < workers; w++ {
				w := w
				models[w] = map[string][]byte{}
				wg.Add(1)
				go func() {
					defer wg.Done()
					if err := worker(fs, w, opsPer, models[w]); err != nil {
						errs <- fmt.Errorf("worker %d: %w", w, err)
					}
				}()
			}
			stop := make(chan struct{})
			var rg sync.WaitGroup
			for r := 0; r < 3; r++ {
				rg.Add(1)
				go func(r int) { // concurrent readers and listers
					defer rg.Done()
					rng := rand.New(rand.NewSource(int64(r)))
					buf := make([]byte, 20000)
					for {
						select {
						case <-stop:
							return
						default:
						}
						d, err := fs.Lookup(1, fmt.Sprintf("w%d", rng.Intn(workers)))
						if err != nil {
							continue
						}
						// Collect first: the callback runs under the read lock and
						// must not call back into the filesystem (a second RLock
						// deadlocks as soon as a writer is waiting).
						var names []string
						_ = fs.Readdir(d.Ino, 0, func(e DirEntry) bool {
							if e.Name != "." && e.Name != ".." && rng.Intn(3) == 0 {
								names = append(names, e.Name)
							}
							return true
						})
						for _, name := range names {
							if a, err := fs.Lookup(d.Ino, name); err == nil {
								_, _ = fs.Read(a.Ino, 0, buf)
								fs.Forget(a.Ino, 1)
							}
						}
						fs.Forget(d.Ino, 1)
					}
				}(r)
			}
			rg.Add(1)
			go func() { // periodic snapshots (taken at arbitrary points)
				defer rg.Done()
				for i := 0; ; i++ {
					select {
					case <-stop:
						return
					case <-time.After(15 * time.Millisecond):
					}
					name := fmt.Sprintf("s%d", i%3)
					_ = fs.DeleteSnapshot(name)
					_ = fs.CreateSnapshot(name)
				}
			}()
			wg.Wait()
			close(stop)
			rg.Wait()
			close(errs)
			for err := range errs {
				t.Error(err)
			}
			if t.Failed() {
				return
			}
			verify := func(fs *FS, when string) {
				for w := 0; w < workers; w++ {
					d, err := fs.Lookup(1, fmt.Sprintf("w%d", w))
					if err != nil {
						t.Fatalf("%s: %v", when, err)
					}
					ents := map[string]bool{}
					_ = fs.Readdir(d.Ino, 0, func(e DirEntry) bool {
						if e.Name != "." && e.Name != ".." {
							ents[e.Name] = true
						}
						return true
					})
					if len(ents) != len(models[w]) {
						t.Fatalf("%s: worker %d dir has %d entries, model %d", when, w, len(ents), len(models[w]))
					}
					for name, want := range models[w] {
						a, err := fs.Lookup(d.Ino, name)
						if err != nil {
							t.Fatalf("%s: w%d/%s: %v", when, w, name, err)
						}
						got := make([]byte, a.Size)
						if n, err := fs.Read(a.Ino, 0, got); err != nil || uint64(n) != a.Size || !bytes.Equal(got, want) {
							t.Fatalf("%s: w%d/%s content mismatch (size %d want %d, err %v)", when, w, name, a.Size, len(want), err)
						}
						fs.Forget(a.Ino, 1)
					}
				}
			}
			verify(fs, "before unmount")
			if err := fs.Close(); err != nil {
				t.Fatal(err)
			}
			snapFSCK(t, dev)
			fs2, _, err := Open(dev, opts)
			if err != nil {
				t.Fatal(err)
			}
			verify(fs2, "after remount")
			if err := fs2.Close(); err != nil {
				t.Fatal(err)
			}
			snapFSCK(t, dev)
		})
	}
}

func worker(fs *FS, w, ops int, model map[string][]byte) error {
	rng := rand.New(rand.NewSource(int64(w) + 100))
	ctx := Ctx{}
	d, err := fs.Mkdir(ctx, 1, fmt.Sprintf("w%d", w), 0o755)
	if err != nil {
		return err
	}
	names := []string{"a", "b", "c", "d", "e", "f"}
	for i := 0; i < ops; i++ {
		name := names[rng.Intn(len(names))]
		data, exists := model[name]
		switch r := rng.Intn(100); {
		case r < 15 && !exists: // create
			a, err := fs.Mknod(ctx, d.Ino, name, 0o644, 0)
			if err != nil {
				return fmt.Errorf("create %s: %w", name, err)
			}
			fs.Forget(a.Ino, 1)
			model[name] = []byte{}
		case r < 60 && exists: // write (append, overwrite or sparse extend)
			a, err := fs.Lookup(d.Ino, name)
			if err != nil {
				return fmt.Errorf("lookup %s: %w", name, err)
			}
			n := 1 + rng.Intn(30000)
			off := uint64(len(data))
			if rng.Intn(2) == 0 && len(data) > 0 {
				off = uint64(rng.Intn(len(data)))
			} else if rng.Intn(5) == 0 {
				off += uint64(rng.Intn(10000))
			}
			p := make([]byte, n)
			rng.Read(p)
			if wn, err := fs.Write(ctx, a.Ino, off, p); err != nil || wn != n {
				return fmt.Errorf("write %s: %d %v", name, wn, err)
			}
			if end := int(off) + n; end > len(data) {
				data = append(data, make([]byte, end-len(data))...)
			}
			copy(data[off:], p)
			model[name] = data
			fs.Forget(a.Ino, 1)
		case r < 68 && exists: // truncate
			a, err := fs.Lookup(d.Ino, name)
			if err != nil {
				return err
			}
			sz := uint64(rng.Intn(len(data) + 5000))
			if _, err := fs.Setattr(ctx, a.Ino, SetAttrIn{Valid: SetSize, Size: sz}); err != nil {
				return fmt.Errorf("truncate: %w", err)
			}
			if int(sz) > len(data) {
				data = append(data, make([]byte, int(sz)-len(data))...)
			} else {
				data = data[:sz]
			}
			model[name] = data
			fs.Forget(a.Ino, 1)
		case r < 78 && exists: // unlink
			if err := fs.Unlink(d.Ino, name); err != nil {
				return fmt.Errorf("unlink: %w", err)
			}
			delete(model, name)
		case r < 86 && exists: // rename (possibly over an existing file)
			to := names[rng.Intn(len(names))]
			if to == name {
				continue
			}
			if err := fs.Rename(d.Ino, name, d.Ino, to, 0); err != nil {
				return fmt.Errorf("rename: %w", err)
			}
			model[to] = data
			delete(model, name)
		case r < 92: // fsync
			if err := fs.Sync(); err != nil {
				return fmt.Errorf("sync: %w", err)
			}
		}
	}
	fs.Forget(d.Ino, 1)
	return nil
}
