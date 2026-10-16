package crashtest

import (
	"bytes"
	"math/rand"
	"testing"

	"github.com/Vinayak069/walfs/internal/blockdev"
	"github.com/Vinayak069/walfs/internal/engine"
	"github.com/Vinayak069/walfs/internal/format"
	"github.com/Vinayak069/walfs/internal/fsck"
)

// Deleting a snapshot that owns more than one chunk of preserved blocks takes
// several transactions. Power loss at any point must leave either the intact
// snapshot or no snapshot at all (the half-deleted state is invisible and is
// finished at the next mount), with no leaked or doubly owned block.
func TestSnapshotChunkedDeleteCrash(t *testing.T) {
	for _, mode := range []engine.JournalMode{engine.ModeOrdered, engine.ModeData} {
		t.Run(mode.String(), func(t *testing.T) {
			dev := blockdev.NewMem(8192 * format.BlockSize)
			if _, err := format.Mkfs(dev, format.MkfsOptions{AssumeZero: true, JournalBlocks: 128, Inodes: 256, Now: 1000}); err != nil {
				t.Fatal(err)
			}
			opts := engine.Options{Mode: mode, CacheBlocks: 128, DirtyLimitBlocks: 64, Clock: fixedClock}
			fs, _, err := engine.Open(dev, opts)
			if err != nil {
				t.Fatal(err)
			}
			ctx := engine.Ctx{}
			f, err := fs.Mknod(ctx, 1, "big", 0o644, 0)
			if err != nil {
				t.Fatal(err)
			}
			orig := make([]byte, 6<<20) // 1536 blocks
			rand.New(rand.NewSource(1)).Read(orig)
			for off := 0; off < len(orig); off += 64 << 10 {
				if _, err := fs.Write(ctx, f.Ino, uint64(off), orig[off:off+64<<10]); err != nil {
					t.Fatal(err)
				}
			}
			if err := fs.CreateSnapshot("s"); err != nil {
				t.Fatal(err)
			}
			// Overwrite everything: every block is preserved for the snapshot.
			noise := make([]byte, 64<<10)
			rand.New(rand.NewSource(2)).Read(noise)
			for off := 0; off < len(orig); off += 64 << 10 {
				if _, err := fs.Write(ctx, f.Ino, uint64(off), noise); err != nil {
					t.Fatal(err)
				}
			}
			if st := fs.Stats(); st.SnapPreserved < 1500 {
				t.Fatalf("only %d blocks preserved; test needs a multi-chunk deletion", st.SnapPreserved)
			}
			if err := fs.Close(); err != nil {
				t.Fatal(err)
			}

			rec := blockdev.NewRecording(blockdev.NewMemFrom(dev.Image()))
			fs2, _, err := engine.Open(rec, opts)
			if err != nil {
				t.Fatal(err)
			}
			if err := fs2.DeleteSnapshot("s"); err != nil {
				t.Fatal(err)
			}
			log := rec.Log()
			flushes := 0
			for _, e := range log {
				if e.Kind == blockdev.OpFlush {
					flushes++
				}
			}
			if flushes < 8 {
				t.Fatalf("deletion used only %d flushes; expected a multi-transaction deletion", flushes)
			}

			durable := blockdev.NewMemFrom(rec.Base())
			var pending []blockdev.LogEntry
			rng := rand.New(rand.NewSource(7))
			sawSnap, sawGone := 0, 0
			for p := 0; p <= len(log); p++ {
				for _, v := range []string{"none", "all", "random", "random"} {
					img := blockdev.NewMemFrom(durable.Image())
					applyPending(img, pending, v, rng)
					fsA, _, err := engine.Open(img, opts)
					if err != nil {
						t.Fatalf("event %d (%s): mount: %v", p, v, err)
					}
					snaps := fsA.ListSnapshots()
					switch len(snaps) {
					case 0:
						sawGone++
					case 1:
						sawSnap++
						view, err := fsA.SnapshotView("s")
						if err != nil {
							t.Fatalf("event %d: %v", p, err)
						}
						a, err := view.Lookup(1, "big")
						if err != nil {
							t.Fatalf("event %d: snapshot lost the file: %v", p, err)
						}
						got := make([]byte, len(orig))
						if n, err := view.Read(a.Ino, 0, got); err != nil || n != len(orig) || !bytes.Equal(got, orig) {
							t.Fatalf("event %d (%s): surviving snapshot has wrong contents (n=%d err=%v)", p, v, n, err)
						}
					default:
						t.Fatalf("event %d: %d snapshots", p, len(snaps))
					}
					if err := fsA.Close(); err != nil {
						t.Fatalf("event %d: close: %v", p, err)
					}
					if rep, err := fsck.Check(img, fsck.Options{}); err != nil || !rep.OK() {
						t.Fatalf("event %d (%s): fsck: %v %v", p, v, err, rep)
					}
				}
				if p == len(log) {
					break
				}
				if e := log[p]; e.Kind == blockdev.OpFlush {
					for _, w := range pending {
						_, _ = durable.WriteAt(w.Data, w.Off)
					}
					pending = pending[:0]
				} else {
					pending = append(pending, e)
				}
			}
			if sawSnap == 0 || sawGone == 0 {
				t.Fatalf("crash points only produced one outcome (snapshot present %d, gone %d)", sawSnap, sawGone)
			}
			t.Logf("%d device events, %d flushes; recovered with snapshot intact %d times, deleted %d times", len(log), flushes, sawSnap, sawGone)
		})
	}
}
