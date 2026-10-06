package engine

import (
	"syscall"

	"github.com/vinayak-singh/walfs/internal/format"
)

// Ctx carries the caller's credentials.
type Ctx struct{ UID, GID uint32 }

// Attr is the stat(2)-like view of an inode.
type Attr struct {
	Ino                 uint64
	Mode                uint32
	Nlink               uint32
	UID, GID            uint32
	Size                uint64
	Blocks              uint64 // 512-byte units, as st_blocks
	Atime, Mtime, Ctime int64  // nanoseconds since the epoch
	Rdev                uint64
	Gen                 uint32
}

func toAttr(ino uint32, in *format.Inode) Attr {
	return Attr{
		Ino: uint64(ino), Mode: in.Mode, Nlink: in.Nlink, UID: in.UID, GID: in.GID,
		Size: in.Size, Blocks: in.Blocks * (format.BlockSize / 512),
		Atime: in.Atime, Mtime: in.Mtime, Ctime: in.Ctime, Rdev: in.Rdev, Gen: in.Gen,
	}
}

// Statfs reports capacity.
type Statfs struct {
	Blocks, Bfree, Bavail uint64
	Files, Ffree          uint64
	Bsize, Namelen        uint32
}

const (
	sISUID = 0o4000
	sISGID = 0o2000
	sIXGRP = 0o010

	// truncBudget bounds how many blocks one transaction frees; larger
	// truncates and unlinks proceed in several transactions.
	truncBudget = 1024
	// reserveMeta is the worst-case number of journaled blocks of one
	// metadata operation (create, unlink, rename, ...).
	reserveMeta = 24
)

func checkName(name string) error {
	switch {
	case name == "":
		return syscall.ENOENT
	case len(name) > format.NameMax:
		return syscall.ENAMETOOLONG
	}
	for i := 0; i < len(name); i++ {
		if name[i] == '/' || name[i] == 0 {
			return syscall.EINVAL
		}
	}
	return nil
}

func (fs *FS) now() int64 { return fs.opts.Clock() }

func (fs *FS) loadDirInode(ino uint32) (format.Inode, error) {
	in, err := fs.getInode(ino)
	if err != nil {
		return in, err
	}
	if in.Mode == 0 {
		return in, syscall.ESTALE
	}
	if !in.IsDir() {
		return in, syscall.ENOTDIR
	}
	return in, nil
}

// loadLiveDir additionally rejects directories that have been removed.
func (fs *FS) loadLiveDir(ino uint32) (format.Inode, error) {
	in, err := fs.loadDirInode(ino)
	if err == nil && in.Nlink == 0 {
		return in, syscall.ENOENT
	}
	return in, err
}

// ---------------------------------------------------------------------------
// Lookup counts (the kernel's reference to an inode). An inode whose last
// link is removed lives on, on the orphan list, until the count drops to zero.

func (fs *FS) addRef(ino uint32) {
	fs.refMu.Lock()
	fs.refs[ino]++
	fs.refMu.Unlock()
}

func (fs *FS) refCount(ino uint32) uint64 {
	fs.refMu.Lock()
	defer fs.refMu.Unlock()
	return fs.refs[ino]
}

// Ref records an extra kernel reference (used by readdirplus).
func (fs *FS) Ref(ino uint64) { fs.addRef(uint32(ino)) }

// Forget drops n kernel references to ino, freeing it if it was unlinked.
func (fs *FS) Forget(ino uint64, n uint64) {
	i := uint32(ino)
	fs.refMu.Lock()
	c := fs.refs[i]
	if n >= c {
		delete(fs.refs, i)
		c = 0
	} else {
		fs.refs[i] = c - n
		c -= n
	}
	fs.refMu.Unlock()
	if c != 0 {
		return
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.aborted != nil || fs.opts.ReadOnly || fs.refCount(i) != 0 {
		return
	}
	in, err := fs.getInode(i)
	if err != nil || in.Mode == 0 || in.Nlink != 0 {
		return
	}
	if fs.beginOp(reserveMeta, 0) != nil {
		return
	}
	_ = fs.destroyInode(i, &in, true)
}

// ---------------------------------------------------------------------------
// Orphan list: inodes with nlink == 0 that are still open. Persisting the
// list lets mount-time recovery finish the job after a crash.

func (fs *FS) setOrphanHead(v uint32) error {
	b, err := fs.metaBuf(0)
	if err != nil {
		return syscall.EIO
	}
	fs.sb.OrphanHead = v
	copy(b.data, fs.sb.Marshal())
	return nil
}

func (fs *FS) addOrphan(ino uint32, in *format.Inode) error {
	in.OrphanNext = fs.sb.OrphanHead
	if err := fs.putInode(ino, in); err != nil {
		return err
	}
	return fs.setOrphanHead(ino)
}

func (fs *FS) removeOrphan(ino uint32, in *format.Inode) error {
	if fs.sb.OrphanHead == ino {
		if err := fs.setOrphanHead(in.OrphanNext); err != nil {
			return err
		}
	} else {
		prev := fs.sb.OrphanHead
		for steps := uint32(0); prev != 0 && steps <= fs.sb.InodeCount; steps++ {
			pin, err := fs.getInode(prev)
			if err != nil {
				return err
			}
			if pin.OrphanNext == ino {
				pin.OrphanNext = in.OrphanNext
				if err := fs.putInode(prev, &pin); err != nil {
					return err
				}
				break
			}
			prev = pin.OrphanNext
		}
	}
	in.OrphanNext = 0
	return nil
}

// releaseInode is called once an operation has removed the last link of ino.
func (fs *FS) releaseInode(ino uint32, in *format.Inode) error {
	if fs.refCount(ino) > 0 {
		return fs.addOrphan(ino, in)
	}
	return fs.destroyInode(ino, in, false)
}

// destroyInode frees all blocks and finally the inode itself. Big files are
// freed in several transactions; to stay crash-safe in between, the inode is
// first put on the persistent orphan list, so mount-time recovery resumes the
// job. Every intermediate state is a consistent filesystem.
func (fs *FS) destroyInode(ino uint32, in *format.Inode, onOrphan bool) error {
	if in.Blocks > truncBudget/2 && !onOrphan {
		if err := fs.addOrphan(ino, in); err != nil {
			return err
		}
		onOrphan = true
	}
	for in.Blocks > 0 {
		st := freeState{budget: truncBudget}
		if err := fs.freeFrom(in, 0, &st); err != nil {
			_ = fs.putInode(ino, in)
			return err
		}
		if !st.done {
			in.Size = min(in.Size, st.low*format.BlockSize)
		}
		if err := fs.putInode(ino, in); err != nil {
			return err
		}
		if st.done {
			break
		}
		if err := fs.beginOp(reserveMeta, 0); err != nil {
			return err
		}
	}
	if in.IsDir() {
		fs.dropDirIdx(ino)
	}
	if onOrphan {
		if err := fs.removeOrphan(ino, in); err != nil {
			return err
		}
	}
	if err := fs.freeInodeBit(ino); err != nil {
		return err
	}
	dead := format.Inode{Gen: in.Gen + 1}
	return fs.putInode(ino, &dead)
}

// newInode allocates and initialises an inode (not yet linked anywhere).
func (fs *FS) newInode(ctx Ctx, parent *format.Inode, mode uint32) (uint32, format.Inode, error) {
	ino, err := fs.allocInode()
	if err != nil {
		return 0, format.Inode{}, err
	}
	old, _ := fs.getInode(ino) // preserves the generation counter across reuse
	now := fs.now()
	gid := ctx.GID
	if parent.Mode&sISGID != 0 {
		gid = parent.GID
		if mode&format.SIFMT == format.SIFDIR {
			mode |= sISGID
		}
	}
	in := format.Inode{
		Mode: mode, UID: ctx.UID, GID: gid, Nlink: 1,
		Atime: now, Mtime: now, Ctime: now, Crtime: now, Gen: old.Gen + 1,
	}
	return ino, in, nil
}

// undoNewInode returns an inode allocated by an operation that then failed.
func (fs *FS) undoNewInode(ino uint32, gen uint32) {
	_ = fs.freeInodeBit(ino)
	dead := format.Inode{Gen: gen}
	_ = fs.putInode(ino, &dead)
}

func (fs *FS) touchDir(din *format.Inode, now int64) { din.Mtime, din.Ctime = now, now }
