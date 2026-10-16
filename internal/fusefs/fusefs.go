// Package fusefs exposes an engine.FS through the Linux FUSE kernel
// interface using go-fuse's low-level (inode-number based) API. It is a thin
// translation layer: all filesystem semantics live in the engine.
package fusefs

import (
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/Vinayak069/walfs/internal/engine"
	"github.com/Vinayak069/walfs/internal/format"
	"github.com/Vinayak069/walfs/internal/metrics"
)

// Options tunes the adapter.
type Options struct {
	AttrTimeout  time.Duration // how long the kernel may cache attributes
	EntryTimeout time.Duration // how long the kernel may cache name lookups
}

// FS implements fuse.RawFileSystem.
type FS struct {
	fuse.RawFileSystem
	e    *engine.FS
	opts Options
	ops  *metrics.Ops // optional
}

// New wraps e.
func New(e *engine.FS, o Options) *FS {
	return &FS{RawFileSystem: fuse.NewDefaultRawFileSystem(), e: e, opts: o}
}

// WithMetrics attaches request counters.
func (f *FS) WithMetrics(m *metrics.Ops) *FS { f.ops = m; return f }

func (f *FS) String() string { return "walfs" }

func ctxOf(h *fuse.InHeader) engine.Ctx { return engine.Ctx{UID: h.Uid, GID: h.Gid} }

func st(err error) fuse.Status {
	if err == nil {
		return fuse.OK
	}
	return fuse.ToStatus(err)
}

// fail records a failed request.
func (f *FS) fail(err error) fuse.Status {
	if err != nil && f.ops != nil {
		f.ops.Errors.Add(1)
	}
	return st(err)
}

func split(ns int64) (sec uint64, nsec uint32) {
	if ns < 0 {
		return 0, 0
	}
	return uint64(ns / 1e9), uint32(ns % 1e9)
}

func fillAttr(o *fuse.Attr, a engine.Attr) {
	o.Ino = a.Ino
	o.Size = a.Size
	o.Blocks = a.Blocks
	o.Atime, o.Atimensec = split(a.Atime)
	o.Mtime, o.Mtimensec = split(a.Mtime)
	o.Ctime, o.Ctimensec = split(a.Ctime)
	o.Mode = a.Mode
	o.Nlink = a.Nlink
	o.Uid, o.Gid = a.UID, a.GID
	o.Rdev = uint32(a.Rdev)
	o.Blksize = format.BlockSize
}

func (f *FS) fillEntry(o *fuse.EntryOut, a engine.Attr) {
	o.NodeId = a.Ino
	o.Generation = uint64(a.Gen)
	o.SetEntryTimeout(f.opts.EntryTimeout)
	o.SetAttrTimeout(f.opts.AttrTimeout)
	fillAttr(&o.Attr, a)
}

func (f *FS) Lookup(_ <-chan struct{}, h *fuse.InHeader, name string, out *fuse.EntryOut) fuse.Status {
	if f.ops != nil {
		f.ops.Lookups.Add(1)
	}
	a, err := f.e.Lookup(h.NodeId, name)
	if err != nil {
		if err != syscall.ENOENT {
			return f.fail(err)
		}
		return st(err)
	}
	f.fillEntry(out, a)
	return fuse.OK
}

func (f *FS) Forget(nodeid, nlookup uint64) { f.e.Forget(nodeid, nlookup) }

func (f *FS) GetAttr(_ <-chan struct{}, in *fuse.GetAttrIn, out *fuse.AttrOut) fuse.Status {
	a, err := f.e.Getattr(in.NodeId)
	if err != nil {
		return st(err)
	}
	out.SetTimeout(f.opts.AttrTimeout)
	fillAttr(&out.Attr, a)
	return fuse.OK
}

func (f *FS) SetAttr(_ <-chan struct{}, in *fuse.SetAttrIn, out *fuse.AttrOut) fuse.Status {
	var a engine.SetAttrIn
	if v, ok := in.GetMode(); ok {
		a.Valid |= engine.SetMode
		a.Mode = v
	}
	if v, ok := in.GetUID(); ok {
		a.Valid |= engine.SetUID
		a.UID = v
	}
	if v, ok := in.GetGID(); ok {
		a.Valid |= engine.SetGID
		a.GID = v
	}
	if v, ok := in.GetSize(); ok {
		a.Valid |= engine.SetSize
		a.Size = v
	}
	if t, ok := in.GetATime(); ok {
		a.Valid |= engine.SetAtime
		a.Atime = t.UnixNano()
		a.AtimeNow = in.Valid&fuse.FATTR_ATIME_NOW != 0
	}
	if t, ok := in.GetMTime(); ok {
		a.Valid |= engine.SetMtime
		a.Mtime = t.UnixNano()
		a.MtimeNow = in.Valid&fuse.FATTR_MTIME_NOW != 0
	}
	at, err := f.e.Setattr(ctxOf(&in.InHeader), in.NodeId, a)
	if err != nil {
		return st(err)
	}
	out.SetTimeout(f.opts.AttrTimeout)
	fillAttr(&out.Attr, at)
	return fuse.OK
}

func (f *FS) Mknod(_ <-chan struct{}, in *fuse.MknodIn, name string, out *fuse.EntryOut) fuse.Status {
	if f.ops != nil {
		f.ops.Creates.Add(1)
	}
	a, err := f.e.Mknod(ctxOf(&in.InHeader), in.NodeId, name, in.Mode, uint64(in.Rdev))
	if err != nil {
		return f.fail(err)
	}
	f.fillEntry(out, a)
	return fuse.OK
}

func (f *FS) Mkdir(_ <-chan struct{}, in *fuse.MkdirIn, name string, out *fuse.EntryOut) fuse.Status {
	a, err := f.e.Mkdir(ctxOf(&in.InHeader), in.NodeId, name, in.Mode)
	if err != nil {
		return st(err)
	}
	f.fillEntry(out, a)
	return fuse.OK
}

func (f *FS) Symlink(_ <-chan struct{}, h *fuse.InHeader, target string, name string, out *fuse.EntryOut) fuse.Status {
	a, err := f.e.Symlink(ctxOf(h), h.NodeId, name, []byte(target))
	if err != nil {
		return st(err)
	}
	f.fillEntry(out, a)
	return fuse.OK
}

func (f *FS) Readlink(_ <-chan struct{}, h *fuse.InHeader) ([]byte, fuse.Status) {
	b, err := f.e.Readlink(h.NodeId)
	return b, st(err)
}

func (f *FS) Link(_ <-chan struct{}, in *fuse.LinkIn, name string, out *fuse.EntryOut) fuse.Status {
	a, err := f.e.Link(in.Oldnodeid, in.NodeId, name)
	if err != nil {
		return st(err)
	}
	f.fillEntry(out, a)
	return fuse.OK
}

func (f *FS) Unlink(_ <-chan struct{}, h *fuse.InHeader, name string) fuse.Status {
	if f.ops != nil {
		f.ops.Unlinks.Add(1)
	}
	return f.fail(f.e.Unlink(h.NodeId, name))
}

func (f *FS) Rmdir(_ <-chan struct{}, h *fuse.InHeader, name string) fuse.Status {
	if f.ops != nil {
		f.ops.Unlinks.Add(1)
	}
	return f.fail(f.e.Rmdir(h.NodeId, name))
}

func (f *FS) Rename(_ <-chan struct{}, in *fuse.RenameIn, oldName, newName string) fuse.Status {
	if f.ops != nil {
		f.ops.Renames.Add(1)
	}
	return f.fail(f.e.Rename(in.NodeId, oldName, in.Newdir, newName, in.Flags))
}

// Access is only reached when the kernel is not enforcing permissions itself.
func (f *FS) Access(_ <-chan struct{}, in *fuse.AccessIn) fuse.Status { return fuse.OK }

func (f *FS) Create(_ <-chan struct{}, in *fuse.CreateIn, name string, out *fuse.CreateOut) fuse.Status {
	if f.ops != nil {
		f.ops.Creates.Add(1)
	}
	a, err := f.e.Mknod(ctxOf(&in.InHeader), in.NodeId, name, format.SIFREG|(in.Mode&0o7777), 0)
	if err != nil {
		return f.fail(err)
	}
	f.fillEntry(&out.EntryOut, a)
	return fuse.OK
}

func (f *FS) Open(_ <-chan struct{}, in *fuse.OpenIn, out *fuse.OpenOut) fuse.Status {
	if in.Flags&syscall.O_TRUNC != 0 {
		if _, err := f.e.Setattr(ctxOf(&in.InHeader), in.NodeId, engine.SetAttrIn{Valid: engine.SetSize, Size: 0}); err != nil {
			return st(err)
		}
	}
	// Every change to the filesystem goes through this kernel mount, so the
	// page cache can never be stale: keep it across open() calls.
	out.OpenFlags |= fuse.FOPEN_KEEP_CACHE
	return fuse.OK
}

func (f *FS) Read(_ <-chan struct{}, in *fuse.ReadIn, buf []byte) (fuse.ReadResult, fuse.Status) {
	t0 := time.Now()
	n, err := f.e.Read(in.NodeId, in.Offset, buf[:min(len(buf), int(in.Size))])
	if f.ops != nil {
		f.ops.Observe(&f.ops.Reads, &f.ops.ReadNanos, t0)
		f.ops.ReadBytes.Add(uint64(max(n, 0)))
	}
	if err != nil {
		return nil, f.fail(err)
	}
	return fuse.ReadResultData(buf[:n]), fuse.OK
}

func (f *FS) Write(_ <-chan struct{}, in *fuse.WriteIn, data []byte) (uint32, fuse.Status) {
	t0 := time.Now()
	n, err := f.e.Write(ctxOf(&in.InHeader), in.NodeId, in.Offset, data)
	if f.ops != nil {
		f.ops.Observe(&f.ops.Writes, &f.ops.WriteNanos, t0)
		f.ops.WrittenBytes.Add(uint64(max(n, 0)))
	}
	if err != nil {
		return uint32(n), f.fail(err)
	}
	return uint32(n), fuse.OK
}

func (f *FS) Lseek(_ <-chan struct{}, in *fuse.LseekIn, out *fuse.LseekOut) fuse.Status {
	switch in.Whence {
	case 3, 4: // SEEK_DATA, SEEK_HOLE
		off, err := f.e.Lseek(in.NodeId, in.Offset, int(in.Whence))
		if err != nil {
			return st(err)
		}
		out.Offset = off
		return fuse.OK
	}
	return fuse.ENOSYS
}

func (f *FS) Fallocate(_ <-chan struct{}, in *fuse.FallocateIn) fuse.Status {
	return st(f.e.Fallocate(ctxOf(&in.InHeader), in.NodeId, in.Mode, in.Offset, in.Length))
}

// Flush is called on every close(2) of a descriptor. POSIX does not promise
// durability there, so it is deliberately a no-op.
func (f *FS) Flush(_ <-chan struct{}, in *fuse.FlushIn) fuse.Status { return fuse.OK }

func (f *FS) Release(_ <-chan struct{}, in *fuse.ReleaseIn) {}

// Fsync commits the running journal transaction, which makes every operation
// issued so far durable (the journal is a single total order).
func (f *FS) Fsync(_ <-chan struct{}, in *fuse.FsyncIn) fuse.Status {
	t0 := time.Now()
	err := f.e.Sync()
	if f.ops != nil {
		f.ops.Observe(&f.ops.Fsyncs, &f.ops.FsyncNanos, t0)
	}
	return f.fail(err)
}

func (f *FS) FsyncDir(c <-chan struct{}, in *fuse.FsyncIn) fuse.Status { return f.Fsync(c, in) }

func (f *FS) OpenDir(_ <-chan struct{}, in *fuse.OpenIn, out *fuse.OpenOut) fuse.Status {
	return fuse.OK
}

func (f *FS) ReleaseDir(in *fuse.ReleaseIn) {}

func (f *FS) ReadDir(_ <-chan struct{}, in *fuse.ReadIn, out *fuse.DirEntryList) fuse.Status {
	err := f.e.Readdir(in.NodeId, in.Offset, func(d engine.DirEntry) bool {
		return out.AddDirEntry(fuse.DirEntry{
			Name: d.Name, Ino: d.Ino, Mode: uint32(d.Type) << 12, Off: d.Next,
		})
	})
	return st(err)
}

// ReadDirPlus returns entries together with their attributes, saving the
// kernel a LOOKUP per file (this is what makes `ls -l` and `find` fast).
// Every entry with a non-zero node id counts as a kernel lookup, so the engine
// takes a reference for each; "." and ".." are returned with a zero node id,
// which tells the kernel to skip them.
func (f *FS) ReadDirPlus(_ <-chan struct{}, in *fuse.ReadIn, out *fuse.DirEntryList) fuse.Status {
	type slot struct {
		name string
		eo   *fuse.EntryOut
	}
	var slots []slot
	err := f.e.Readdir(in.NodeId, in.Offset, func(d engine.DirEntry) bool {
		eo := out.AddDirLookupEntry(fuse.DirEntry{
			Name: d.Name, Ino: d.Ino, Mode: uint32(d.Type) << 12, Off: d.Next,
		})
		if eo == nil {
			return false
		}
		if d.Name != "." && d.Name != ".." {
			slots = append(slots, slot{d.Name, eo})
		}
		return true
	})
	if err != nil {
		return st(err)
	}
	// Fill in attributes outside the readdir critical section (Lookup takes
	// the same lock). An entry removed in the meantime is left zeroed.
	for _, sl := range slots {
		if a, err := f.e.Lookup(in.NodeId, sl.name); err == nil {
			f.fillEntry(sl.eo, a)
		}
	}
	return fuse.OK
}

func (f *FS) StatFs(_ <-chan struct{}, in *fuse.InHeader, out *fuse.StatfsOut) fuse.Status {
	s := f.e.StatFS()
	*out = fuse.StatfsOut{
		Blocks: s.Blocks, Bfree: s.Bfree, Bavail: s.Bavail,
		Files: s.Files, Ffree: s.Ffree,
		Bsize: s.Bsize, NameLen: s.Namelen, Frsize: s.Bsize,
	}
	return fuse.OK
}
