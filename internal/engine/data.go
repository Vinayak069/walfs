package engine

import (
	"sync"
	"syscall"

	"github.com/vinayak-singh/walfs/internal/format"
)

// ---------------------------------------------------------------------------
// Reading

var readScratch = sync.Pool{New: func() any { b := make([]byte, 1<<20+format.BlockSize); return &b }}

// Read copies up to len(p) bytes of file ino starting at off into p. Runs of
// physically adjacent uncached blocks are fetched with a single device read.
func (fs *FS) Read(ino uint64, off uint64, p []byte) (int, error) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	in, err := fs.getInode(uint32(ino))
	if err != nil {
		return 0, err
	}
	if in.Mode == 0 {
		return 0, syscall.ESTALE
	}
	if in.IsDir() {
		return 0, syscall.EISDIR
	}
	if !in.IsReg() {
		return 0, syscall.EINVAL
	}
	if off >= in.Size || len(p) == 0 {
		return 0, nil
	}
	n := min(uint64(len(p)), in.Size-off)
	p = p[:n]
	m := fs.newMapper(&in)
	done := uint64(0)
	for done < n {
		pos := off + done
		fb := pos / format.BlockSize
		bo := pos % format.BlockSize
		// How many blocks does the remaining request span (bounded)?
		nb := min((bo+(n-done)+format.BlockSize-1)/format.BlockSize, 256)
		pbs := make([]uint32, nb)
		for i := range pbs {
			pb, err := m.lookup(fb + uint64(i))
			if err != nil {
				return int(done), errnoOrNil(err)
			}
			pbs[i] = pb
		}
		i := uint64(0)
		for i < nb && done < n {
			bo = (off + done) % format.BlockSize
			take := min(format.BlockSize-bo, n-done)
			pb := pbs[i]
			switch {
			case pb == 0:
				clear(p[done : done+take])
			case fs.c.peek(uint64(pb)) != nil:
				copy(p[done:done+take], fs.c.peek(uint64(pb)).data[bo:])
			default:
				// Gather the run of uncached, physically contiguous blocks.
				j := i + 1
				for j < nb && pbs[j] == pbs[j-1]+1 && fs.c.peek(uint64(pbs[j])) == nil {
					j++
				}
				sp := readScratch.Get().(*[]byte)
				scratch := (*sp)[:(j-i)*format.BlockSize]
				if _, err := fs.dev.ReadAt(scratch, int64(pb)*format.BlockSize); err != nil {
					readScratch.Put(sp)
					return int(done), syscall.EIO
				}
				for k := i; k < j && done < n; k++ {
					bo = (off + done) % format.BlockSize
					take = min(format.BlockSize-bo, n-done)
					copy(p[done:done+take], scratch[(k-i)*format.BlockSize+bo:])
					done += take
				}
				readScratch.Put(sp)
				i = j
				continue
			}
			done += take
			i++
		}
	}
	return int(n), nil
}

// ---------------------------------------------------------------------------
// Writing

// Write stores data at off, extending the file if needed. A write that fits
// in one chunk (fs.chunk blocks) is atomic with respect to crashes; larger
// writes are split into chunks that each commit atomically, which POSIX
// permits.
func (fs *FS) Write(ctx Ctx, ino uint64, off uint64, data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	i := uint32(ino)
	in, err := fs.getInode(i)
	if err != nil {
		return 0, err
	}
	if in.Mode == 0 {
		return 0, syscall.ESTALE
	}
	if in.IsDir() {
		return 0, syscall.EISDIR
	}
	if !in.IsReg() {
		return 0, syscall.EINVAL
	}
	if off+uint64(len(data)) > MaxFileSize || off+uint64(len(data)) < off {
		return 0, syscall.EFBIG
	}
	done := 0
	var werr error
	for done < len(data) {
		if err := fs.beginOp(reserveMeta+fs.chunk, fs.chunk+4); err != nil {
			werr = err
			break
		}
		n, err := fs.writeChunk(i, &in, ctx, off+uint64(done), data[done:])
		done += n
		if err != nil {
			werr = err
			break
		}
	}
	if done > 0 {
		return done, nil
	}
	return 0, werr
}

// writeChunk writes at most fs.chunk blocks and persists the inode.
func (fs *FS) writeChunk(ino uint32, in *format.Inode, ctx Ctx, off uint64, data []byte) (int, error) {
	done := 0
	var werr error
	m := fs.newMapper(in)
	firstFB := off / format.BlockSize
	goal := uint64(0)
	if firstFB > 0 {
		if prev, _ := m.lookup(firstFB - 1); prev != 0 {
			goal = uint64(prev) + 1
		}
	}
	for done < len(data) {
		pos := off + uint64(done)
		fb := pos / format.BlockSize
		if fb-firstFB >= uint64(fs.chunk) {
			break
		}
		bo := int(pos % format.BlockSize)
		take := min(format.BlockSize-bo, len(data)-done)
		pb, created, err := fs.bmapAlloc(in, fb, goal)
		if err != nil {
			werr = err
			break
		}
		goal = uint64(pb) + 1
		var b *buf
		switch {
		case created:
			b = fs.c.getNew(uint64(pb), take < format.BlockSize)
		case take == format.BlockSize:
			b = fs.c.getNew(uint64(pb), false)
		default:
			if b, err = fs.c.get(uint64(pb)); err != nil {
				werr = syscall.EIO
			}
		}
		if werr != nil {
			break
		}
		copy(b.data[bo:], data[done:done+take])
		fs.dirtyData(b)
		done += take
	}
	if done > 0 || in.Blocks != 0 {
		now := fs.now()
		if end := off + uint64(done); end > in.Size {
			in.Size = end
		}
		in.Mtime, in.Ctime = now, now
		if ctx.UID != 0 {
			in.Mode &^= sISUID
			if in.Mode&sIXGRP != 0 {
				in.Mode &^= sISGID
			}
		}
		if err := fs.putInode(ino, in); err != nil {
			return done, err
		}
	}
	return done, errnoOrNil(werr)
}

// ---------------------------------------------------------------------------
// Truncation and attribute changes

// zeroTail clears the bytes beyond newSize in the last retained block so that
// a later extension of the file reads zeros, not stale data.
func (fs *FS) zeroTail(in *format.Inode, newSize uint64) error {
	off := int(newSize % format.BlockSize)
	if off == 0 {
		return nil
	}
	pb, err := fs.newMapper(in).lookup(newSize / format.BlockSize)
	if err != nil || pb == 0 {
		return err
	}
	b, err := fs.c.get(uint64(pb))
	if err != nil {
		return syscall.EIO
	}
	clear(b.data[off:])
	fs.dirtyData(b)
	return nil
}

// truncate resizes a regular file. Shrinking frees blocks from the end in
// bounded steps (each its own transaction), keeping every intermediate state
// a valid file.
func (fs *FS) truncate(ino uint32, in *format.Inode, newSize uint64) error {
	if newSize > MaxFileSize {
		return syscall.EFBIG
	}
	now := fs.now()
	if newSize >= in.Size {
		in.Size = newSize
		in.Mtime, in.Ctime = now, now
		return fs.putInode(ino, in)
	}
	if err := fs.zeroTail(in, newSize); err != nil {
		return errnoOrNil(err)
	}
	from := (newSize + format.BlockSize - 1) / format.BlockSize
	for {
		st := freeState{budget: truncBudget, leaf: classData}
		if err := fs.freeFrom(in, from, &st); err != nil {
			_ = fs.putInode(ino, in)
			return errnoOrNil(err)
		}
		if st.done {
			in.Size = newSize
		} else {
			in.Size = max(newSize, min(in.Size, st.low*format.BlockSize))
		}
		in.Mtime, in.Ctime = now, now
		if err := fs.putInode(ino, in); err != nil {
			return err
		}
		if st.done {
			return nil
		}
		if err := fs.beginOp(reserveMeta, 0); err != nil {
			return err
		}
	}
}

// SetAttr field selectors.
const (
	SetMode = 1 << iota
	SetUID
	SetGID
	SetSize
	SetAtime
	SetMtime
)

// SetAttrIn describes an attribute change.
type SetAttrIn struct {
	Valid              uint32
	Mode, UID, GID     uint32
	Size               uint64
	Atime, Mtime       int64
	AtimeNow, MtimeNow bool
}

// Setattr applies attribute changes (chmod, chown, truncate, utimens).
func (fs *FS) Setattr(ctx Ctx, ino uint64, a SetAttrIn) (Attr, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if err := fs.beginOp(reserveMeta, 0); err != nil {
		return Attr{}, err
	}
	i := uint32(ino)
	in, err := fs.getInode(i)
	if err != nil {
		return Attr{}, err
	}
	if in.Mode == 0 {
		return Attr{}, syscall.ESTALE
	}
	now := fs.now()
	if a.Valid&SetSize != 0 {
		switch {
		case in.IsDir():
			return Attr{}, syscall.EISDIR
		case !in.IsReg():
			return Attr{}, syscall.EINVAL
		}
		if err := fs.truncate(i, &in, a.Size); err != nil {
			return Attr{}, err
		}
		// truncate may have committed; the inode is persisted, continue.
		if err := fs.beginOp(reserveMeta, 0); err != nil {
			return Attr{}, err
		}
	}
	if a.Valid&SetMode != 0 {
		in.Mode = in.Mode&format.SIFMT | a.Mode&0o7777
		in.Ctime = now
	}
	if a.Valid&(SetUID|SetGID) != 0 {
		if a.Valid&SetUID != 0 {
			in.UID = a.UID
		}
		if a.Valid&SetGID != 0 {
			in.GID = a.GID
		}
		if in.IsReg() && ctx.UID != 0 {
			in.Mode &^= sISUID
			if in.Mode&sIXGRP != 0 {
				in.Mode &^= sISGID
			}
		}
		in.Ctime = now
	}
	if a.Valid&SetAtime != 0 {
		in.Atime = a.Atime
		if a.AtimeNow {
			in.Atime = now
		}
		in.Ctime = now
	}
	if a.Valid&SetMtime != 0 {
		in.Mtime = a.Mtime
		if a.MtimeNow {
			in.Mtime = now
		}
		in.Ctime = now
	}
	if err := fs.putInode(i, &in); err != nil {
		return Attr{}, err
	}
	return toAttr(i, &in), nil
}

// ---------------------------------------------------------------------------
// fallocate and lseek

// Fallocate modes (FALLOC_FL_*).
const (
	FallocKeepSize  = 0x01
	FallocPunchHole = 0x02
	FallocZeroRange = 0x10
)

// punchBlock frees one data block of the file (making it a hole).
func (fs *FS) punchBlock(in *format.Inode, fb uint64) error {
	depth, idx, ok := blockPath(fb)
	if !ok {
		return syscall.EFBIG
	}
	if depth == 0 {
		p := in.Direct[idx[0]]
		if p == 0 {
			return nil
		}
		if err := fs.freeBlock(p, classData); err != nil {
			return err
		}
		in.Direct[idx[0]] = 0
		in.Blocks--
		return nil
	}
	ptr := *rootPtr(in, depth)
	for lvl := 0; lvl < depth; lvl++ {
		if ptr == 0 {
			return nil
		}
		if err := fs.validBlk(ptr); err != nil {
			return err
		}
		b, err := fs.c.get(uint64(ptr))
		if err != nil {
			return syscall.EIO
		}
		next := le.Uint32(b.data[idx[lvl]*4:])
		if lvl == depth-1 {
			if next == 0 {
				return nil
			}
			if b, err = fs.metaBuf(uint64(ptr)); err != nil {
				return syscall.EIO
			}
			if err := fs.freeBlock(next, classData); err != nil {
				return err
			}
			le.PutUint32(b.data[idx[lvl]*4:], 0)
			in.Blocks--
			return nil
		}
		ptr = next
	}
	return nil
}

// Fallocate preallocates or deallocates a byte range.
func (fs *FS) Fallocate(ctx Ctx, ino uint64, mode uint32, off, length uint64) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	i := uint32(ino)
	in, err := fs.getInode(i)
	if err != nil {
		return err
	}
	if in.Mode == 0 {
		return syscall.ESTALE
	}
	if in.IsDir() {
		return syscall.EISDIR
	}
	if !in.IsReg() {
		return syscall.ENODEV
	}
	if length == 0 || off+length < off || off+length > MaxFileSize {
		return syscall.EINVAL
	}
	end := off + length
	keep := mode&FallocKeepSize != 0
	switch mode &^ FallocKeepSize {
	case 0: // allocate
		firstFB, lastFB := off/format.BlockSize, (end-1)/format.BlockSize
		for fb := firstFB; fb <= lastFB; {
			if err := fs.beginOp(reserveMeta+fs.chunk, fs.chunk+4); err != nil {
				return err
			}
			stop := min(lastFB, fb+uint64(fs.chunk)-1)
			m := fs.newMapper(&in)
			var aerr error
			for ; fb <= stop; fb++ {
				if pb, _ := m.lookup(fb); pb != 0 {
					continue
				}
				pb, created, err := fs.bmapAlloc(&in, fb, 0)
				if err != nil {
					aerr = err
					break
				}
				if created {
					b := fs.c.getNew(uint64(pb), true)
					fs.dirtyData(b)
				}
			}
			if !keep && aerr == nil && end > in.Size && fb > lastFB {
				in.Size = end
			}
			in.Ctime = fs.now()
			if err := fs.putInode(i, &in); err != nil {
				return err
			}
			if aerr != nil {
				return errnoOrNil(aerr)
			}
		}
		if !keep && end > in.Size {
			in.Size = end
			return fs.putInode(i, &in)
		}
		return nil
	case FallocPunchHole:
		if !keep {
			return syscall.EOPNOTSUPP
		}
		if off >= in.Size {
			return nil
		}
		end = min(end, in.Size)
		return fs.punch(i, &in, off, end)
	}
	return syscall.EOPNOTSUPP
}

// punch deallocates [off,end): whole blocks become holes, partial edge blocks
// are zeroed in place.
func (fs *FS) punch(ino uint32, in *format.Inode, off, end uint64) error {
	zero := func(pos, stop uint64) error {
		pb, err := fs.newMapper(in).lookup(pos / format.BlockSize)
		if err != nil || pb == 0 {
			return err
		}
		b, err := fs.c.get(uint64(pb))
		if err != nil {
			return syscall.EIO
		}
		clear(b.data[pos%format.BlockSize : stop-pos/format.BlockSize*format.BlockSize])
		fs.dirtyData(b)
		return nil
	}
	firstFull := (off + format.BlockSize - 1) / format.BlockSize
	lastFull := end / format.BlockSize // exclusive
	if err := fs.beginOp(reserveMeta+8, 0); err != nil {
		return err
	}
	if off/format.BlockSize == (end-1)/format.BlockSize {
		// The whole range lies inside a single block.
		if err := zero(off, end); err != nil {
			return errnoOrNil(err)
		}
	} else {
		if off%format.BlockSize != 0 {
			if err := zero(off, firstFull*format.BlockSize); err != nil {
				return errnoOrNil(err)
			}
		}
		if end%format.BlockSize != 0 {
			if err := zero(lastFull*format.BlockSize, end); err != nil {
				return errnoOrNil(err)
			}
		}
		for fb := firstFull; fb < lastFull; fb++ {
			if fb != firstFull && (fb-firstFull)%uint64(truncBudget) == 0 {
				if err := fs.putInode(ino, in); err != nil {
					return err
				}
				if err := fs.beginOp(reserveMeta, 0); err != nil {
					return err
				}
			}
			if err := fs.punchBlock(in, fb); err != nil {
				_ = fs.putInode(ino, in)
				return errnoOrNil(err)
			}
		}
	}
	in.Mtime = fs.now()
	in.Ctime = in.Mtime
	return fs.putInode(ino, in)
}

// Lseek implements SEEK_DATA (3) and SEEK_HOLE (4).
func (fs *FS) Lseek(ino uint64, off uint64, whence int) (uint64, error) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	in, err := fs.getInode(uint32(ino))
	if err != nil {
		return 0, err
	}
	if in.Mode == 0 {
		return 0, syscall.ESTALE
	}
	if off >= in.Size {
		return 0, syscall.ENXIO
	}
	m := fs.newMapper(&in)
	endFB := (in.Size + format.BlockSize - 1) / format.BlockSize
	for fb := off / format.BlockSize; fb < endFB; fb++ {
		pb, err := m.lookup(fb)
		if err != nil {
			return 0, errnoOrNil(err)
		}
		isData := pb != 0
		if (whence == 3 && isData) || (whence == 4 && !isData) {
			return max(off, fb*format.BlockSize), nil
		}
	}
	if whence == 4 {
		return in.Size, nil // an implicit hole at EOF
	}
	return 0, syscall.ENXIO
}
