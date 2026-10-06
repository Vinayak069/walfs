package engine

import (
	"syscall"

	"github.com/vinayak-singh/walfs/internal/format"
)

// ---------------------------------------------------------------------------
// Read-only namespace operations

// Lookup resolves name in directory parent and takes a kernel reference.
func (fs *FS) Lookup(parent uint64, name string) (Attr, error) {
	if err := checkName(name); err != nil {
		return Attr{}, err
	}
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	pin, err := fs.loadDirInode(uint32(parent))
	if err != nil {
		return Attr{}, err
	}
	e, ok, err := fs.dirLookup(uint32(parent), &pin, name)
	if err != nil {
		return Attr{}, errno(err)
	}
	if !ok {
		return Attr{}, syscall.ENOENT
	}
	in, err := fs.getInode(e.ino)
	if err != nil {
		return Attr{}, err
	}
	if in.Mode == 0 {
		return Attr{}, syscall.EUCLEAN
	}
	fs.addRef(e.ino)
	return toAttr(e.ino, &in), nil
}

// Getattr returns the attributes of an inode.
func (fs *FS) Getattr(ino uint64) (Attr, error) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	in, err := fs.getInode(uint32(ino))
	if err != nil {
		return Attr{}, err
	}
	if in.Mode == 0 {
		return Attr{}, syscall.ESTALE
	}
	return toAttr(uint32(ino), &in), nil
}

// Readlink returns a symlink's target.
func (fs *FS) Readlink(ino uint64) ([]byte, error) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	in, err := fs.getInode(uint32(ino))
	if err != nil {
		return nil, err
	}
	if in.Mode == 0 {
		return nil, syscall.ESTALE
	}
	if !in.IsLnk() {
		return nil, syscall.EINVAL
	}
	if in.InlineSymlink() {
		return append([]byte(nil), in.Inline...), nil
	}
	pb, err := fs.newMapper(&in).lookup(0)
	if err != nil || pb == 0 {
		return nil, syscall.EUCLEAN
	}
	b, err := fs.c.get(uint64(pb))
	if err != nil {
		return nil, syscall.EIO
	}
	if in.Size > format.BlockSize {
		return nil, syscall.EUCLEAN
	}
	return append([]byte(nil), b.data[:in.Size]...), nil
}

// Readdir lists directory ino starting at the resume cookie.
func (fs *FS) Readdir(ino uint64, cookie uint64, fn func(DirEntry) bool) error {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	in, err := fs.loadDirInode(uint32(ino))
	if err != nil {
		return err
	}
	return errnoOrNil(fs.readDirEntries(&in, cookie, fn))
}

func errnoOrNil(err error) error {
	if err == nil {
		return nil
	}
	if IsCorrupt(err) {
		return syscall.EUCLEAN
	}
	return errno(err)
}

// StatFS reports capacity and usage.
func (fs *FS) StatFS() Statfs {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	avail := uint64(0)
	if fs.freeBlocks > fs.reservedBlks {
		avail = fs.freeBlocks - fs.reservedBlks
	}
	return Statfs{
		Blocks: fs.sb.TotalBlocks - fs.sb.DataStart,
		Bfree:  fs.freeBlocks, Bavail: avail,
		Files: uint64(fs.sb.InodeCount) - 1, Ffree: uint64(fs.freeInodes),
		Bsize: format.BlockSize, Namelen: format.NameMax,
	}
}

// ---------------------------------------------------------------------------
// Creation

// Mknod creates a regular file, FIFO, socket or device node.
func (fs *FS) Mknod(ctx Ctx, parent uint64, name string, mode uint32, rdev uint64) (Attr, error) {
	switch mode & format.SIFMT {
	case format.SIFREG, format.SIFIFO, format.SIFSOCK, format.SIFCHR, format.SIFBLK:
	case 0:
		mode |= format.SIFREG
	default:
		return Attr{}, syscall.EINVAL
	}
	return fs.create(ctx, parent, name, mode, rdev, nil)
}

// Mkdir creates a directory.
func (fs *FS) Mkdir(ctx Ctx, parent uint64, name string, mode uint32) (Attr, error) {
	return fs.create(ctx, parent, name, format.SIFDIR|(mode&0o7777), 0, nil)
}

// Symlink creates a symbolic link.
func (fs *FS) Symlink(ctx Ctx, parent uint64, name string, target []byte) (Attr, error) {
	if len(target) == 0 {
		return Attr{}, syscall.ENOENT
	}
	if len(target) >= format.BlockSize {
		return Attr{}, syscall.ENAMETOOLONG
	}
	return fs.create(ctx, parent, name, format.SIFLNK|0o777, 0, target)
}

func (fs *FS) create(ctx Ctx, parent uint64, name string, mode uint32, rdev uint64, target []byte) (Attr, error) {
	if err := checkName(name); err != nil {
		return Attr{}, err
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if err := fs.beginOp(reserveMeta, 4); err != nil {
		return Attr{}, err
	}
	if fs.freeBlocks < 4 {
		return Attr{}, syscall.ENOSPC
	}
	pino := uint32(parent)
	pin, err := fs.loadLiveDir(pino)
	if err != nil {
		return Attr{}, err
	}
	if name == "." || name == ".." {
		return Attr{}, syscall.EEXIST
	}
	if _, ok, err := fs.dirLookup(pino, &pin, name); err != nil {
		return Attr{}, errnoOrNil(err)
	} else if ok {
		return Attr{}, syscall.EEXIST
	}
	isDir := mode&format.SIFMT == format.SIFDIR
	if isDir && pin.Nlink >= format.MaxLinks {
		return Attr{}, syscall.EMLINK
	}
	ino, in, err := fs.newInode(ctx, &pin, mode)
	if err != nil {
		return Attr{}, err
	}
	in.Rdev = rdev
	fail := func(err error) (Attr, error) {
		fs.undoNewInode(ino, in.Gen)
		return Attr{}, errnoOrNil(err)
	}
	switch {
	case isDir:
		pb, err := fs.allocBlock(0, true)
		if err != nil {
			return fail(err)
		}
		b := fs.newMeta(uint64(pb))
		format.PutDirent(b.data, 0, ino, format.DirentLen(1), format.FTDir, ".")
		format.PutDirent(b.data, format.DirentLen(1), pino, format.BlockSize-format.DirentLen(1), format.FTDir, "..")
		in.Direct[0] = pb
		in.Size, in.Blocks, in.Nlink = format.BlockSize, 1, 2
	case target != nil:
		in.Size = uint64(len(target))
		if len(target) <= format.InlineSymlinkMax {
			in.Inline = append([]byte(nil), target...)
		} else {
			pb, err := fs.allocBlock(0, true)
			if err != nil {
				return fail(err)
			}
			b := fs.newMeta(uint64(pb))
			copy(b.data, target)
			in.Direct[0] = pb
			in.Blocks = 1
		}
	}
	if err := fs.dirInsert(pino, &pin, name, ino, format.FTypeOf(mode)); err != nil {
		if isDir {
			_ = fs.freeBlock(in.Direct[0], classMeta)
		}
		return fail(err)
	}
	if isDir {
		pin.Nlink++
	}
	fs.touchDir(&pin, in.Ctime)
	if err := fs.putInode(ino, &in); err != nil {
		return Attr{}, err
	}
	if err := fs.putInode(pino, &pin); err != nil {
		return Attr{}, err
	}
	fs.addRef(ino)
	return toAttr(ino, &in), nil
}

// Link adds a hard link to ino at newparent/name.
func (fs *FS) Link(ino uint64, newparent uint64, name string) (Attr, error) {
	if err := checkName(name); err != nil {
		return Attr{}, err
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if err := fs.beginOp(reserveMeta, 4); err != nil {
		return Attr{}, err
	}
	if fs.freeBlocks < 4 {
		return Attr{}, syscall.ENOSPC
	}
	in, err := fs.getInode(uint32(ino))
	if err != nil {
		return Attr{}, err
	}
	if in.Mode == 0 || in.Nlink == 0 {
		return Attr{}, syscall.ENOENT
	}
	if in.IsDir() {
		return Attr{}, syscall.EPERM
	}
	if in.Nlink >= format.MaxLinks {
		return Attr{}, syscall.EMLINK
	}
	pino := uint32(newparent)
	pin, err := fs.loadLiveDir(pino)
	if err != nil {
		return Attr{}, err
	}
	if name == "." || name == ".." {
		return Attr{}, syscall.EEXIST
	}
	if _, ok, err := fs.dirLookup(pino, &pin, name); err != nil {
		return Attr{}, errnoOrNil(err)
	} else if ok {
		return Attr{}, syscall.EEXIST
	}
	if err := fs.dirInsert(pino, &pin, name, uint32(ino), format.FTypeOf(in.Mode)); err != nil {
		return Attr{}, errnoOrNil(err)
	}
	now := fs.now()
	in.Nlink++
	in.Ctime = now
	fs.touchDir(&pin, now)
	if err := fs.putInode(uint32(ino), &in); err != nil {
		return Attr{}, err
	}
	if err := fs.putInode(pino, &pin); err != nil {
		return Attr{}, err
	}
	fs.addRef(uint32(ino))
	return toAttr(uint32(ino), &in), nil
}

// ---------------------------------------------------------------------------
// Removal

// Unlink removes a non-directory entry.
func (fs *FS) Unlink(parent uint64, name string) error {
	return fs.remove(parent, name, false)
}

// Rmdir removes an empty directory.
func (fs *FS) Rmdir(parent uint64, name string) error {
	return fs.remove(parent, name, true)
}

func (fs *FS) remove(parent uint64, name string, wantDir bool) error {
	if err := checkName(name); err != nil {
		return err
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if err := fs.beginOp(reserveMeta, 0); err != nil {
		return err
	}
	pino := uint32(parent)
	pin, err := fs.loadDirInode(pino)
	if err != nil {
		return err
	}
	if wantDir && name == "." {
		return syscall.EINVAL
	}
	if name == "." || name == ".." {
		if wantDir {
			return syscall.ENOTEMPTY
		}
		return syscall.EISDIR
	}
	e, ok, err := fs.dirLookup(pino, &pin, name)
	if err != nil {
		return errnoOrNil(err)
	}
	if !ok {
		return syscall.ENOENT
	}
	in, err := fs.getInode(e.ino)
	if err != nil {
		return err
	}
	if in.Mode == 0 {
		return syscall.EUCLEAN
	}
	if wantDir {
		if !in.IsDir() {
			return syscall.ENOTDIR
		}
		empty, err := fs.dirIsEmpty(e.ino, &in)
		if err != nil {
			return errnoOrNil(err)
		}
		if !empty {
			return syscall.ENOTEMPTY
		}
	} else if in.IsDir() {
		return syscall.EISDIR
	}
	if _, err := fs.dirRemove(pino, &pin, name); err != nil {
		return errnoOrNil(err)
	}
	now := fs.now()
	if wantDir {
		in.Nlink = 0
		pin.Nlink--
	} else {
		in.Nlink--
	}
	in.Ctime = now
	fs.touchDir(&pin, now)
	if err := fs.putInode(pino, &pin); err != nil {
		return err
	}
	if in.Nlink == 0 {
		return errnoOrNil(fs.releaseInode(e.ino, &in))
	}
	return fs.putInode(e.ino, &in)
}

// ---------------------------------------------------------------------------
// Rename

// Rename flags (RENAME_* from <linux/fs.h>).
const (
	RenameNoReplace = 1
	RenameExchange  = 2
)

// isAncestor reports whether directory anc is dir itself or one of its
// ancestors, following ".." entries to the root.
func (fs *FS) isAncestor(anc, dir uint32) (bool, error) {
	cur := dir
	for steps := 0; steps < 1<<20; steps++ {
		if cur == anc {
			return true, nil
		}
		if cur == format.RootIno {
			return false, nil
		}
		in, err := fs.getInode(cur)
		if err != nil {
			return false, err
		}
		e, ok, err := fs.dirLookup(cur, &in, "..")
		if err != nil {
			return false, errnoOrNil(err)
		}
		if !ok || e.ino == cur {
			return false, nil
		}
		cur = e.ino
	}
	return false, syscall.ELOOP
}

// Rename moves oldparent/oldname to newparent/newname atomically.
func (fs *FS) Rename(oldparent uint64, oldname string, newparent uint64, newname string, flags uint32) error {
	if err := checkName(oldname); err != nil {
		return err
	}
	if err := checkName(newname); err != nil {
		return err
	}
	if flags&^(RenameNoReplace|RenameExchange) != 0 || flags == RenameNoReplace|RenameExchange {
		return syscall.EINVAL
	}
	if oldname == "." || oldname == ".." || newname == "." || newname == ".." {
		return syscall.EBUSY
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if err := fs.beginOp(reserveMeta+8, 4); err != nil {
		return err
	}
	if fs.freeBlocks < 4 {
		return syscall.ENOSPC
	}
	op, np := uint32(oldparent), uint32(newparent)
	opIn, err := fs.loadDirInode(op)
	if err != nil {
		return err
	}
	var npIn format.Inode
	npP := &npIn
	if op == np {
		npP = &opIn
	} else if npIn, err = fs.loadDirInode(np); err != nil {
		return err
	}
	if npP.Nlink == 0 { // destination directory was removed
		return syscall.ENOENT
	}
	se, ok, err := fs.dirLookup(op, &opIn, oldname)
	if err != nil {
		return errnoOrNil(err)
	}
	if !ok {
		return syscall.ENOENT
	}
	de, dstExists, err := fs.dirLookup(np, npP, newname)
	if err != nil {
		return errnoOrNil(err)
	}
	exchange := flags&RenameExchange != 0
	if exchange && !dstExists {
		return syscall.ENOENT
	}
	if dstExists && flags&RenameNoReplace != 0 {
		return syscall.EEXIST
	}
	if dstExists && de.ino == se.ino && !exchange {
		return nil // both names are the same file: POSIX says do nothing
	}
	srcIn, err := fs.getInode(se.ino)
	if err != nil {
		return err
	}
	srcDir := srcIn.IsDir()
	if srcDir {
		if anc, err := fs.isAncestor(se.ino, np); err != nil {
			return errnoOrNil(err)
		} else if anc {
			return syscall.EINVAL
		}
	}
	var dstIn format.Inode
	dstDir := false
	if dstExists {
		if dstIn, err = fs.getInode(de.ino); err != nil {
			return err
		}
		dstDir = dstIn.IsDir()
		if exchange {
			if dstDir {
				if anc, err := fs.isAncestor(de.ino, op); err != nil {
					return errnoOrNil(err)
				} else if anc {
					return syscall.EINVAL
				}
			}
		} else {
			if srcDir && !dstDir {
				return syscall.ENOTDIR
			}
			if !srcDir && dstDir {
				return syscall.EISDIR
			}
			if dstDir {
				empty, err := fs.dirIsEmpty(de.ino, &dstIn)
				if err != nil {
					return errnoOrNil(err)
				}
				if !empty {
					return syscall.ENOTEMPTY
				}
			}
		}
	}
	cross := op != np
	if srcDir && cross && !exchange && !(dstExists && dstDir) && npP.Nlink >= format.MaxLinks {
		return syscall.EMLINK
	}

	now := fs.now()
	srcFT := format.FTypeOf(srcIn.Mode)
	var fsErr error
	switch {
	case exchange:
		dstFT := format.FTypeOf(dstIn.Mode)
		if fsErr = fs.dirSetIno(op, &opIn, oldname, de.ino, dstFT); fsErr == nil {
			fsErr = fs.dirSetIno(np, npP, newname, se.ino, srcFT)
		}
		if fsErr == nil && cross {
			if srcDir {
				fsErr = fs.dirSetIno(se.ino, &srcIn, "..", np, format.FTDir)
			}
			if fsErr == nil && dstDir {
				fsErr = fs.dirSetIno(de.ino, &dstIn, "..", op, format.FTDir)
			}
			switch {
			case srcDir && !dstDir:
				opIn.Nlink--
				npP.Nlink++
			case dstDir && !srcDir:
				npP.Nlink--
				opIn.Nlink++
			}
		}
		dstIn.Ctime = now
	default:
		if dstExists {
			fsErr = fs.dirSetIno(np, npP, newname, se.ino, srcFT)
		} else {
			fsErr = fs.dirInsert(np, npP, newname, se.ino, srcFT)
		}
		if fsErr == nil {
			_, fsErr = fs.dirRemove(op, &opIn, oldname)
		}
		if fsErr == nil && srcDir && cross {
			fsErr = fs.dirSetIno(se.ino, &srcIn, "..", np, format.FTDir)
			opIn.Nlink--
			npP.Nlink++
		}
		if dstExists {
			if dstDir {
				dstIn.Nlink = 0
				npP.Nlink--
			} else {
				dstIn.Nlink--
			}
			dstIn.Ctime = now
		}
	}
	if fsErr != nil {
		// A multi-step rename failed half-way (I/O error or corruption).
		// Latch the filesystem read-only so the half-applied transaction is
		// never committed: the disk keeps the last consistent state.
		fs.aborted = fsErr
		return syscall.EIO
	}
	srcIn.Ctime = now
	fs.touchDir(&opIn, now)
	if cross {
		fs.touchDir(npP, now)
	}
	if err := fs.putInode(se.ino, &srcIn); err != nil {
		return err
	}
	if err := fs.putInode(op, &opIn); err != nil {
		return err
	}
	if cross {
		if err := fs.putInode(np, npP); err != nil {
			return err
		}
	}
	if dstExists {
		if !exchange && dstIn.Nlink == 0 {
			return errnoOrNil(fs.releaseInode(de.ino, &dstIn))
		}
		return fs.putInode(de.ino, &dstIn)
	}
	return nil
}
