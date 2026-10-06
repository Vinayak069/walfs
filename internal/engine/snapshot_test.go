package engine

import (
	"bytes"
	"math/rand"
	"syscall"
	"testing"

	"github.com/vinayak-singh/walfs/internal/blockdev"
	"github.com/vinayak-singh/walfs/internal/format"
	"github.com/vinayak-singh/walfs/internal/fsck"
)

func randBytes(seed int64, n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

func readAll(t *testing.T, fs *FS, ino uint64) []byte {
	t.Helper()
	a, err := fs.Getattr(ino)
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, a.Size)
	if n, err := fs.Read(ino, 0, b); err != nil || uint64(n) != a.Size {
		t.Fatalf("read: %d %v", n, err)
	}
	return b
}

func lookupPath(t *testing.T, fs *FS, comps ...string) (Attr, error) {
	t.Helper()
	cur := uint64(1)
	var a Attr
	var err error
	for _, c := range comps {
		if a, err = fs.Lookup(cur, c); err != nil {
			return a, err
		}
		cur = a.Ino
	}
	return a, nil
}

func snapFSCK(t *testing.T, dev blockdev.Device) {
	t.Helper()
	r, err := fsck.Check(dev, fsck.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !r.OK() {
		t.Fatalf("fsck: %s", r)
	}
}

func TestSnapshotFreezesState(t *testing.T) {
	for _, mode := range []JournalMode{ModeOrdered, ModeData} {
		t.Run(mode.String(), func(t *testing.T) {
			fs, dev := newTestFS(t, 8192, mode)
			ctx := Ctx{}
			d, _ := fs.Mkdir(ctx, 1, "d", 0o755)
			f, _ := fs.Mknod(ctx, d.Ino, "f", 0o644, 0)
			g, _ := fs.Mknod(ctx, d.Ino, "gone", 0o644, 0)
			orig := randBytes(1, 100000)
			origG := randBytes(2, 50000)
			fs.Write(ctx, f.Ino, 0, orig)
			fs.Write(ctx, g.Ino, 0, origG)
			if _, err := fs.Symlink(ctx, d.Ino, "lnk", []byte("f")); err != nil {
				t.Fatal(err)
			}

			if err := fs.CreateSnapshot("s1"); err != nil {
				t.Fatal(err)
			}
			if err := fs.Checkpoint(); err != nil { // fsck wants an empty journal
				t.Fatal(err)
			}
			snapFSCK(t, dev)

			// Mutate everything the snapshot can see.
			fs.Write(ctx, f.Ino, 5000, randBytes(3, 30000))             // overwrite in place
			fs.Write(ctx, f.Ino, uint64(len(orig)), randBytes(4, 9000)) // append
			fs.Setattr(ctx, f.Ino, SetAttrIn{Valid: SetMode, Mode: 0o600})
			if err := fs.Unlink(d.Ino, "gone"); err != nil { // frees blocks the snapshot still needs
				t.Fatal(err)
			}
			fs.Forget(g.Ino, 1)
			if err := fs.Rename(d.Ino, "f", 1, "moved", 0); err != nil {
				t.Fatal(err)
			}
			if _, err := fs.Mkdir(ctx, 1, "newdir", 0o755); err != nil {
				t.Fatal(err)
			}

			v, err := fs.SnapshotView("s1")
			if err != nil {
				t.Fatal(err)
			}
			vd, err := lookupPath(t, v, "d")
			if err != nil {
				t.Fatal(err)
			}
			vf, err := lookupPath(t, v, "d", "f")
			if err != nil {
				t.Fatalf("snapshot lost file d/f: %v", err)
			}
			if vf.Mode&0o7777 != 0o644 {
				t.Errorf("snapshot sees mode %o, want 644", vf.Mode&0o7777)
			}
			if got := readAll(t, v, vf.Ino); !bytes.Equal(got, orig) {
				t.Fatal("snapshot file contents changed after overwrite/append")
			}
			vg, err := lookupPath(t, v, "d", "gone")
			if err != nil {
				t.Fatalf("snapshot lost unlinked file: %v", err)
			}
			if got := readAll(t, v, vg.Ino); !bytes.Equal(got, origG) {
				t.Fatal("snapshot lost contents of a file deleted after the snapshot")
			}
			if l, err := lookupPath(t, v, "d", "lnk"); err != nil {
				t.Fatal(err)
			} else if tgt, _ := v.Readlink(l.Ino); string(tgt) != "f" {
				t.Fatalf("symlink target %q", tgt)
			}
			if _, err := lookupPath(t, v, "newdir"); err != syscall.ENOENT {
				t.Errorf("snapshot sees a directory created later: %v", err)
			}
			if _, err := lookupPath(t, v, "moved"); err != syscall.ENOENT {
				t.Errorf("snapshot sees a rename done later: %v", err)
			}
			// Views are read-only.
			if _, err := v.Mknod(ctx, vd.Ino, "x", 0o644, 0); err != syscall.EROFS {
				t.Errorf("create in a snapshot: %v, want EROFS", err)
			}
			if _, err := v.Write(ctx, vf.Ino, 0, []byte("x")); err != syscall.EROFS {
				t.Errorf("write to a snapshot: %v, want EROFS", err)
			}
			// The live filesystem is unaffected by preservation.
			live, _ := lookupPath(t, fs, "moved")
			if got := readAll(t, fs, live.Ino); !bytes.Equal(got[:5000], orig[:5000]) {
				t.Error("live file corrupted")
			}

			if err := fs.Close(); err != nil {
				t.Fatal(err)
			}
			snapFSCK(t, dev)

			// Everything survives a remount.
			fs2, _, err := Open(dev, Options{Mode: mode})
			if err != nil {
				t.Fatal(err)
			}
			v2, err := fs2.SnapshotView("s1")
			if err != nil {
				t.Fatal(err)
			}
			vf2, err := lookupPath(t, v2, "d", "f")
			if err != nil || !bytes.Equal(readAll(t, v2, vf2.Ino), orig) {
				t.Fatalf("snapshot content lost across remount: %v", err)
			}
			if err := fs2.DeleteSnapshot("s1"); err != nil {
				t.Fatal(err)
			}
			if _, err := fs2.SnapshotView("s1"); err != syscall.ENOENT {
				t.Errorf("deleted snapshot still opens: %v", err)
			}
			if err := fs2.Close(); err != nil {
				t.Fatal(err)
			}
			snapFSCK(t, dev) // no leaked or double-owned blocks after deletion
		})
	}
}

func TestSnapshotsOverlap(t *testing.T) {
	fs, dev := newTestFS(t, 8192, ModeOrdered)
	ctx := Ctx{}
	f, _ := fs.Mknod(ctx, 1, "f", 0o644, 0)
	versions := [][]byte{randBytes(10, 40000), randBytes(11, 40000), randBytes(12, 40000), randBytes(13, 40000)}
	names := []string{"a", "b", "c"}
	for i, v := range versions[:3] {
		fs.Write(ctx, f.Ino, 0, v)
		if err := fs.CreateSnapshot(names[i]); err != nil {
			t.Fatal(err)
		}
	}
	fs.Write(ctx, f.Ino, 0, versions[3])
	check := func(name string, want []byte) {
		t.Helper()
		v, err := fs.SnapshotView(name)
		if err != nil {
			t.Fatal(err)
		}
		a, err := v.Lookup(1, "f")
		if err != nil {
			t.Fatal(err)
		}
		if got := readAll(t, v, a.Ino); !bytes.Equal(got, want) {
			t.Fatalf("snapshot %s shows the wrong version", name)
		}
	}
	for i, n := range names {
		check(n, versions[i])
	}
	// Deleting the middle snapshot must not disturb the others.
	if err := fs.DeleteSnapshot("b"); err != nil {
		t.Fatal(err)
	}
	check("a", versions[0])
	check("c", versions[2])
	fs.Sync()
	if err := fs.DeleteSnapshot("a"); err != nil {
		t.Fatal(err)
	}
	check("c", versions[2])
	// A new snapshot reuses a slot; stale views must not leak old data.
	if err := fs.CreateSnapshot("d"); err != nil {
		t.Fatal(err)
	}
	check("d", versions[3])
	if err := fs.CreateSnapshot("c"); err != syscall.EEXIST {
		t.Errorf("duplicate name: %v", err)
	}
	if err := fs.Close(); err != nil {
		t.Fatal(err)
	}
	snapFSCK(t, dev)
}

// Deleting a file that a snapshot still sees must not give its space back
// until the snapshot goes away, and must give it all back afterwards.
func TestSnapshotSpaceAccounting(t *testing.T) {
	fs, dev := newTestFS(t, 8192, ModeOrdered)
	ctx := Ctx{}
	f, _ := fs.Mknod(ctx, 1, "big", 0o644, 0)
	fs.Write(ctx, f.Ino, 0, randBytes(5, 2<<20)) // 512 blocks
	fs.Sync()
	if err := fs.CreateSnapshot("s"); err != nil {
		t.Fatal(err)
	}
	afterSnap := fs.StatFS().Bfree
	fs.Unlink(1, "big")
	fs.Forget(f.Ino, 1)
	fs.Sync()
	if free := fs.StatFS().Bfree; free > afterSnap+8 {
		t.Fatalf("deleting a snapshotted file released %d blocks", free-afterSnap)
	}
	if err := fs.DeleteSnapshot("s"); err != nil {
		t.Fatal(err)
	}
	fs.Sync()
	if free := fs.StatFS().Bfree; free < afterSnap+500 {
		t.Fatalf("deleting the snapshot returned only %d of ~512 blocks", free-afterSnap)
	}
	if err := fs.Close(); err != nil {
		t.Fatal(err)
	}
	snapFSCK(t, dev)
}

func TestSnapshotLimitsAndErrors(t *testing.T) {
	fs, _ := newTestFS(t, 8192, ModeOrdered)
	for _, bad := range []string{"", ".", "..", "a/b", string(bytes.Repeat([]byte("x"), 200))} {
		if err := fs.CreateSnapshot(bad); err == nil {
			t.Errorf("snapshot name %q accepted", bad)
		}
	}
	for i := 0; i < format.MaxSnapshots; i++ {
		if err := fs.CreateSnapshot(string(rune('A'+i)) + "x"); err != nil {
			t.Fatalf("snapshot %d: %v", i, err)
		}
	}
	if err := fs.CreateSnapshot("one-too-many"); err != syscall.ENOSPC {
		t.Errorf("33rd snapshot: %v, want ENOSPC", err)
	}
	if err := fs.DeleteSnapshot("nope"); err != syscall.ENOENT {
		t.Errorf("delete missing: %v", err)
	}
	fs0, _ := newTestFS(t, 2048, ModeNone)
	if err := fs0.CreateSnapshot("s"); err != syscall.EOPNOTSUPP {
		t.Errorf("snapshot without a journal: %v", err)
	}
}
