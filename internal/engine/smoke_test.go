package engine

import (
	"bytes"
	"math/rand"
	"testing"

	"github.com/Vinayak069/walfs/internal/blockdev"
	"github.com/Vinayak069/walfs/internal/format"
	"github.com/Vinayak069/walfs/internal/fsck"
)

func newTestFS(t testing.TB, blocks int64, mode JournalMode) (*FS, *blockdev.MemDevice) {
	t.Helper()
	dev := blockdev.NewMem(blocks * format.BlockSize)
	if _, err := format.Mkfs(dev, format.MkfsOptions{AssumeZero: true, JournalBlocks: 256}); err != nil {
		t.Fatal(err)
	}
	fs, _, err := Open(dev, Options{Mode: mode})
	if err != nil {
		t.Fatal(err)
	}
	return fs, dev
}

func TestSmoke(t *testing.T) {
	for _, mode := range []JournalMode{ModeOrdered, ModeData, ModeNone} {
		t.Run(mode.String(), func(t *testing.T) {
			fs, dev := newTestFS(t, 4096, mode)
			ctx := Ctx{}
			a, err := fs.Mkdir(ctx, 1, "d", 0o755)
			if err != nil {
				t.Fatal(err)
			}
			f, err := fs.Mknod(ctx, a.Ino, "f", 0o644, 0)
			if err != nil {
				t.Fatal(err)
			}
			data := make([]byte, 300000)
			rand.New(rand.NewSource(1)).Read(data)
			if n, err := fs.Write(ctx, f.Ino, 100, data); err != nil || n != len(data) {
				t.Fatalf("write: %d %v", n, err)
			}
			got := make([]byte, len(data))
			if n, err := fs.Read(f.Ino, 100, got); err != nil || n != len(data) || !bytes.Equal(got, data) {
				t.Fatalf("read mismatch: %d %v", n, err)
			}
			if err := fs.Close(); err != nil {
				t.Fatal(err)
			}
			mustFsck(t, dev)
			fs2, info, err := Open(dev, Options{Mode: mode})
			if err != nil {
				t.Fatal(err)
			}
			t.Log(info.Journal)
			d2, err := fs2.Lookup(1, "d")
			if err != nil {
				t.Fatal(err)
			}
			f2, err := fs2.Lookup(d2.Ino, "f")
			if err != nil {
				t.Fatal(err)
			}
			if f2.Size != 100+uint64(len(data)) {
				t.Fatalf("size %d", f2.Size)
			}
			if n, err := fs2.Read(f2.Ino, 100, got); err != nil || n != len(data) || !bytes.Equal(got, data) {
				t.Fatalf("read after remount mismatch: %d %v", n, err)
			}
			if err := fs2.Unlink(d2.Ino, "f"); err != nil {
				t.Fatal(err)
			}
			fs2.Forget(f2.Ino, 1)
			if err := fs2.Rmdir(1, "d"); err != nil {
				t.Fatal(err)
			}
			if err := fs2.Close(); err != nil {
				t.Fatal(err)
			}
			mustFsck(t, dev)
		})
	}
}

func mustFsck(t testing.TB, dev blockdev.Device) *fsck.Report {
	t.Helper()
	r, err := fsck.Check(dev, fsck.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !r.OK() {
		t.Fatalf("fsck: %s", r)
	}
	return r
}
