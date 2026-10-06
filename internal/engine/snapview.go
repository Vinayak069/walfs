package engine

import (
	"errors"
	"syscall"

	"github.com/vinayak-singh/walfs/internal/format"
)

// snapDev is the block device a snapshot view reads from. Reading logical
// block X for snapshot s returns the preserved copy if a record says s sees
// one, and the live block otherwise (it has not changed since the snapshot).
type snapDev struct {
	parent *FS
	slot   int
	st     *snapState // the snapshot this view belongs to; a reused slot is a different snapshot
	size   int64
}

var errSnapRO = syscall.EROFS

func (d *snapDev) Size() int64  { return d.size }
func (d *snapDev) Flush() error { return nil }
func (d *snapDev) Close() error { return nil }
func (d *snapDev) WriteAt(p []byte, off int64) (int, error) {
	return 0, errSnapRO
}

// resolve returns the physical block holding snapshot content for logical blk.
// The caller holds parent.mu (read).
func (d *snapDev) resolve(blk uint32) uint32 {
	for _, r := range d.parent.cowIdx[blk] {
		if r.Mask&(1<<d.slot) != 0 {
			return r.P
		}
	}
	return blk
}

func (d *snapDev) ReadAt(p []byte, off int64) (int, error) {
	if off%format.BlockSize != 0 || len(p)%format.BlockSize != 0 {
		return 0, errors.New("snapshot view: unaligned read")
	}
	par := d.parent
	par.mu.RLock()
	defer par.mu.RUnlock()
	if par.snaps[d.slot] != d.st {
		return 0, syscall.ESTALE
	}
	for done := 0; done < len(p); done += format.BlockSize {
		blk := uint32((off + int64(done)) / format.BlockSize)
		phys := uint64(d.resolve(blk))
		out := p[done : done+format.BlockSize]
		// The committed contents of a block may exist only in the cache (a
		// metadata block awaiting its checkpoint, or a data block of the
		// running transaction), so look there first.
		if b := par.c.peek(phys); b != nil {
			copy(out, b.data)
			continue
		}
		if _, err := par.dev.ReadAt(out, int64(phys)*format.BlockSize); err != nil {
			return done, err
		}
	}
	return len(p), nil
}

// SnapshotView returns a read-only filesystem showing the named snapshot. The
// result has the same API as the live filesystem; every mutating call fails
// with EROFS.
func (fs *FS) SnapshotView(name string) (*FS, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.snapParent != nil {
		return nil, syscall.EINVAL
	}
	s := fs.snapByName(name)
	if s == nil || s.state != format.SnapActive {
		return nil, syscall.ENOENT
	}
	return fs.viewLocked(s.slot), nil
}

// SnapshotViewByID is SnapshotView addressed by slot and unique id; it fails
// with ESTALE if the slot has since been reused by a different snapshot.
func (fs *FS) SnapshotViewByID(slot int, uid uint32) (*FS, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if slot < 0 || slot >= format.MaxSnapshots {
		return nil, syscall.ESTALE
	}
	s := fs.snaps[slot]
	if s == nil || s.state != format.SnapActive || s.uid&0xFFFFF != uid&0xFFFFF {
		return nil, syscall.ESTALE
	}
	return fs.viewLocked(slot), nil
}

func (fs *FS) viewLocked(slot int) *FS {
	if v := fs.snapViews[slot]; v != nil {
		return v
	}
	d := &snapDev{parent: fs, slot: slot, st: fs.snaps[slot], size: fs.dev.Size()}
	sb := *fs.sb
	sb.OrphanHead = 0
	v := &FS{
		dev: d, sb: &sb,
		opts:         Options{ReadOnly: true, Clock: fs.opts.Clock, CacheBlocks: 4096, DirtyLimitBlocks: 1},
		c:            newCache(d, 4096),
		txMeta:       map[uint64]*buf{},
		txRevoke:     map[uint64]struct{}{},
		freedPending: map[uint64]struct{}{},
		ckptImg:      map[uint64][]byte{},
		refs:         map[uint32]uint64{},
		dirs:         map[uint32]*dirIndex{},
		cowIdx:       map[uint32][]*cowRec{},
		snapSys:      map[uint32]struct{}{},
		snapTable:    &format.SnapTable{},
		stop:         make(chan struct{}),
		snapParent:   fs,
		snapSlot:     slot,
	}
	fs.snapViews[slot] = v
	return v
}
