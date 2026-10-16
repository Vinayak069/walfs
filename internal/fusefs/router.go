package fusefs

import (
	"syscall"

	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/Vinayak069/walfs/internal/engine"
	"github.com/Vinayak069/walfs/internal/format"
)

// SnapDirName is the hidden directory under the root through which snapshots
// are created (mkdir), deleted (rmdir), listed and browsed (read-only).
const SnapDirName = ".snapshots"

// Node ids. Live inodes are plain inode numbers (< 2^32). Snapshot content is
//
//	uid(20 bits) << 40 | (slot+1)(8 bits) << 32 | inode
//
// so a kernel handle to a deleted snapshot can never resolve to a different
// snapshot that later reuses its slot. The .snapshots directory itself is
// snapDirID.
const (
	viewBase  = uint64(1) << 40
	snapDirID = uint64(1) << 62
)

func viewID(slot int, uid uint32, ino uint64) uint64 {
	return uint64(uid&0xFFFFF)<<40 | uint64(slot+1)<<32 | ino
}

func splitView(id uint64) (slot int, uid uint32, ino uint64) {
	return int(id>>32&0xFF) - 1, uint32(id >> 40 & 0xFFFFF), id & 0xFFFFFFFF
}

func isSnapID(id uint64) bool { return id >= viewBase }

// Router sends requests for live inodes to the main adapter and requests for
// snapshot nodes to a read-only handler.
type Router struct {
	fuse.RawFileSystem
	m *FS
	e *engine.FS
}

// NewRouter wraps main with snapshot browsing support.
func NewRouter(main *FS) *Router {
	return &Router{RawFileSystem: fuse.NewDefaultRawFileSystem(), m: main, e: main.e}
}

func (r *Router) String() string { return "walfs" }

func roAttr(a engine.Attr) engine.Attr {
	a.Mode &^= 0o222
	return a
}

// snapDirAttr is the synthetic attribute set of /.snapshots: owned like the
// root so whoever may write the root may create snapshots.
func (r *Router) snapDirAttr() (engine.Attr, error) {
	a, err := r.e.Getattr(format.RootIno)
	if err != nil {
		return a, err
	}
	a.Ino = snapDirID
	a.Mode = format.SIFDIR | a.Mode&0o7777
	a.Nlink = 2 + uint32(len(r.e.ListSnapshots()))
	a.Size = 4096
	return a, nil
}

func (r *Router) view(id uint64) (*engine.FS, uint64, int, uint32, fuse.Status) {
	slot, uid, ino := splitView(id)
	v, err := r.e.SnapshotViewByID(slot, uid)
	if err != nil {
		return nil, 0, 0, 0, fuse.Status(syscall.ESTALE)
	}
	return v, ino, slot, uid, fuse.OK
}

func (r *Router) snapEntry(o *fuse.EntryOut, info engine.SnapInfo, v *engine.FS) fuse.Status {
	a, err := v.Getattr(format.RootIno)
	if err != nil {
		return st(err)
	}
	a = roAttr(a)
	a.Ino = viewID(info.Slot, info.UID, format.RootIno)
	r.m.fillEntry(o, a)
	o.Generation = uint64(info.UID)
	return fuse.OK
}

func (r *Router) infoByName(name string) (engine.SnapInfo, bool) {
	for _, s := range r.e.ListSnapshots() {
		if s.Name == name {
			return s, true
		}
	}
	return engine.SnapInfo{}, false
}

// ---------------------------------------------------------------------------
// Lookup / attributes

func (r *Router) Lookup(c <-chan struct{}, h *fuse.InHeader, name string, out *fuse.EntryOut) fuse.Status {
	switch {
	case h.NodeId == format.RootIno && name == SnapDirName:
		a, err := r.snapDirAttr()
		if err != nil {
			return st(err)
		}
		r.m.fillEntry(out, a)
		return fuse.OK
	case h.NodeId == snapDirID:
		info, ok := r.infoByName(name)
		if !ok {
			return fuse.ENOENT
		}
		v, err := r.e.SnapshotViewByID(info.Slot, info.UID)
		if err != nil {
			return fuse.ENOENT
		}
		return r.snapEntry(out, info, v)
	case isSnapID(h.NodeId):
		v, ino, slot, uid, s := r.view(h.NodeId)
		if s != fuse.OK {
			return s
		}
		a, err := v.Lookup(ino, name)
		if err != nil {
			return st(err)
		}
		a = roAttr(a)
		a.Ino = viewID(slot, uid, a.Ino)
		r.m.fillEntry(out, a)
		out.Generation = uint64(uid)
		return fuse.OK
	}
	return r.m.Lookup(c, h, name, out)
}

func (r *Router) Forget(nodeid, nlookup uint64) {
	switch {
	case nodeid == snapDirID:
	case isSnapID(nodeid):
		if v, ino, _, _, s := r.view(nodeid); s == fuse.OK {
			v.Forget(ino, nlookup)
		}
	default:
		r.m.Forget(nodeid, nlookup)
	}
}

func (r *Router) GetAttr(c <-chan struct{}, in *fuse.GetAttrIn, out *fuse.AttrOut) fuse.Status {
	switch {
	case in.NodeId == snapDirID:
		a, err := r.snapDirAttr()
		if err != nil {
			return st(err)
		}
		out.SetTimeout(r.m.opts.AttrTimeout)
		fillAttr(&out.Attr, a)
		return fuse.OK
	case isSnapID(in.NodeId):
		v, ino, slot, uid, s := r.view(in.NodeId)
		if s != fuse.OK {
			return s
		}
		a, err := v.Getattr(ino)
		if err != nil {
			return st(err)
		}
		a = roAttr(a)
		a.Ino = viewID(slot, uid, a.Ino)
		out.SetTimeout(r.m.opts.AttrTimeout)
		fillAttr(&out.Attr, a)
		return fuse.OK
	}
	return r.m.GetAttr(c, in, out)
}

func (r *Router) SetAttr(c <-chan struct{}, in *fuse.SetAttrIn, out *fuse.AttrOut) fuse.Status {
	if isSnapID(in.NodeId) {
		return fuse.EROFS
	}
	return r.m.SetAttr(c, in, out)
}

func (r *Router) Access(c <-chan struct{}, in *fuse.AccessIn) fuse.Status {
	if isSnapID(in.NodeId) && in.Mask&2 != 0 { // W_OK
		return fuse.EROFS
	}
	return fuse.OK
}

// ---------------------------------------------------------------------------
// Namespace changes: only /.snapshots accepts them (mkdir = snapshot, rmdir =
// delete); everything inside a snapshot is read-only.

func (r *Router) Mkdir(c <-chan struct{}, in *fuse.MkdirIn, name string, out *fuse.EntryOut) fuse.Status {
	switch {
	case in.NodeId == snapDirID:
		if err := r.e.CreateSnapshot(name); err != nil {
			return r.m.fail(err)
		}
		info, ok := r.infoByName(name)
		if !ok {
			return fuse.EIO
		}
		v, err := r.e.SnapshotViewByID(info.Slot, info.UID)
		if err != nil {
			return st(err)
		}
		return r.snapEntry(out, info, v)
	case isSnapID(in.NodeId):
		return fuse.EROFS
	}
	return r.m.Mkdir(c, in, name, out)
}

func (r *Router) Rmdir(c <-chan struct{}, h *fuse.InHeader, name string) fuse.Status {
	switch {
	case h.NodeId == snapDirID:
		return r.m.fail(r.e.DeleteSnapshot(name))
	case isSnapID(h.NodeId):
		return fuse.EROFS
	}
	return r.m.Rmdir(c, h, name)
}

func (r *Router) Mknod(c <-chan struct{}, in *fuse.MknodIn, name string, out *fuse.EntryOut) fuse.Status {
	if isSnapID(in.NodeId) {
		return fuse.EROFS
	}
	return r.m.Mknod(c, in, name, out)
}

func (r *Router) Create(c <-chan struct{}, in *fuse.CreateIn, name string, out *fuse.CreateOut) fuse.Status {
	if isSnapID(in.NodeId) {
		return fuse.EROFS
	}
	return r.m.Create(c, in, name, out)
}

func (r *Router) Symlink(c <-chan struct{}, h *fuse.InHeader, target, name string, out *fuse.EntryOut) fuse.Status {
	if isSnapID(h.NodeId) {
		return fuse.EROFS
	}
	return r.m.Symlink(c, h, target, name, out)
}

func (r *Router) Link(c <-chan struct{}, in *fuse.LinkIn, name string, out *fuse.EntryOut) fuse.Status {
	if isSnapID(in.NodeId) {
		return fuse.EROFS
	}
	if isSnapID(in.Oldnodeid) {
		return fuse.Status(syscall.EXDEV)
	}
	return r.m.Link(c, in, name, out)
}

func (r *Router) Unlink(c <-chan struct{}, h *fuse.InHeader, name string) fuse.Status {
	if isSnapID(h.NodeId) {
		return fuse.EROFS
	}
	return r.m.Unlink(c, h, name)
}

func (r *Router) Rename(c <-chan struct{}, in *fuse.RenameIn, oldName, newName string) fuse.Status {
	if isSnapID(in.NodeId) || isSnapID(in.Newdir) {
		return fuse.EROFS
	}
	return r.m.Rename(c, in, oldName, newName)
}

// ---------------------------------------------------------------------------
// File I/O

func (r *Router) Readlink(c <-chan struct{}, h *fuse.InHeader) ([]byte, fuse.Status) {
	if isSnapID(h.NodeId) {
		v, ino, _, _, s := r.view(h.NodeId)
		if s != fuse.OK {
			return nil, s
		}
		b, err := v.Readlink(ino)
		return b, st(err)
	}
	return r.m.Readlink(c, h)
}

func (r *Router) Open(c <-chan struct{}, in *fuse.OpenIn, out *fuse.OpenOut) fuse.Status {
	if isSnapID(in.NodeId) {
		if in.Flags&(syscall.O_WRONLY|syscall.O_RDWR|syscall.O_TRUNC|syscall.O_APPEND) != 0 {
			return fuse.EROFS
		}
		out.OpenFlags |= fuse.FOPEN_KEEP_CACHE // snapshots are immutable
		return fuse.OK
	}
	return r.m.Open(c, in, out)
}

func (r *Router) Read(c <-chan struct{}, in *fuse.ReadIn, buf []byte) (fuse.ReadResult, fuse.Status) {
	if isSnapID(in.NodeId) {
		v, ino, _, _, s := r.view(in.NodeId)
		if s != fuse.OK {
			return nil, s
		}
		n, err := v.Read(ino, in.Offset, buf[:min(len(buf), int(in.Size))])
		if err != nil {
			return nil, st(err)
		}
		return fuse.ReadResultData(buf[:n]), fuse.OK
	}
	return r.m.Read(c, in, buf)
}

func (r *Router) Write(c <-chan struct{}, in *fuse.WriteIn, data []byte) (uint32, fuse.Status) {
	if isSnapID(in.NodeId) {
		return 0, fuse.EROFS
	}
	return r.m.Write(c, in, data)
}

func (r *Router) Lseek(c <-chan struct{}, in *fuse.LseekIn, out *fuse.LseekOut) fuse.Status {
	if isSnapID(in.NodeId) {
		v, ino, _, _, s := r.view(in.NodeId)
		if s != fuse.OK {
			return s
		}
		if in.Whence != 3 && in.Whence != 4 {
			return fuse.ENOSYS
		}
		off, err := v.Lseek(ino, in.Offset, int(in.Whence))
		if err != nil {
			return st(err)
		}
		out.Offset = off
		return fuse.OK
	}
	return r.m.Lseek(c, in, out)
}

func (r *Router) Fallocate(c <-chan struct{}, in *fuse.FallocateIn) fuse.Status {
	if isSnapID(in.NodeId) {
		return fuse.EROFS
	}
	return r.m.Fallocate(c, in)
}

func (r *Router) Flush(c <-chan struct{}, in *fuse.FlushIn) fuse.Status { return fuse.OK }
func (r *Router) Release(c <-chan struct{}, in *fuse.ReleaseIn)         {}
func (r *Router) ReleaseDir(in *fuse.ReleaseIn)                         {}

func (r *Router) Fsync(c <-chan struct{}, in *fuse.FsyncIn) fuse.Status {
	if isSnapID(in.NodeId) {
		return fuse.OK
	}
	return r.m.Fsync(c, in)
}

func (r *Router) FsyncDir(c <-chan struct{}, in *fuse.FsyncIn) fuse.Status {
	if isSnapID(in.NodeId) {
		return fuse.OK
	}
	return r.m.FsyncDir(c, in)
}

func (r *Router) StatFs(c <-chan struct{}, h *fuse.InHeader, out *fuse.StatfsOut) fuse.Status {
	return r.m.StatFs(c, h, out)
}

// ---------------------------------------------------------------------------
// Directories

func (r *Router) OpenDir(c <-chan struct{}, in *fuse.OpenIn, out *fuse.OpenOut) fuse.Status {
	return fuse.OK
}

type listing struct {
	name string
	ino  uint64
	typ  uint8
	info *engine.SnapInfo
}

// snapDirListing returns ".", ".." and one entry per snapshot.
func (r *Router) snapDirListing() []listing {
	out := []listing{{".", snapDirID, format.FTDir, nil}, {"..", format.RootIno, format.FTDir, nil}}
	for _, s := range r.e.ListSnapshots() {
		s := s
		out = append(out, listing{s.Name, viewID(s.Slot, s.UID, format.RootIno), format.FTDir, &s})
	}
	return out
}

func (r *Router) ReadDir(c <-chan struct{}, in *fuse.ReadIn, out *fuse.DirEntryList) fuse.Status {
	switch {
	case in.NodeId == snapDirID:
		for i, l := range r.snapDirListing() {
			if uint64(i) < in.Offset {
				continue
			}
			if !out.AddDirEntry(fuse.DirEntry{Name: l.name, Ino: l.ino, Mode: uint32(l.typ) << 12, Off: uint64(i) + 1}) {
				break
			}
		}
		return fuse.OK
	case isSnapID(in.NodeId):
		v, ino, _, _, s := r.view(in.NodeId)
		if s != fuse.OK {
			return s
		}
		return st(v.Readdir(ino, in.Offset, func(d engine.DirEntry) bool {
			return out.AddDirEntry(fuse.DirEntry{Name: d.Name, Ino: d.Ino, Mode: uint32(d.Type) << 12, Off: d.Next})
		}))
	}
	return r.m.ReadDir(c, in, out)
}

func (r *Router) ReadDirPlus(c <-chan struct{}, in *fuse.ReadIn, out *fuse.DirEntryList) fuse.Status {
	switch {
	case in.NodeId == snapDirID:
		// Entries without a node id are skipped by the kernel and looked up on
		// demand, which keeps reference counting simple for views.
		for i, l := range r.snapDirListing() {
			if uint64(i) < in.Offset {
				continue
			}
			if out.AddDirLookupEntry(fuse.DirEntry{Name: l.name, Ino: l.ino, Mode: uint32(l.typ) << 12, Off: uint64(i) + 1}) == nil {
				break
			}
		}
		return fuse.OK
	case isSnapID(in.NodeId):
		v, ino, slot, uid, s := r.view(in.NodeId)
		if s != fuse.OK {
			return s
		}
		type slotT struct {
			name string
			eo   *fuse.EntryOut
		}
		var slots []slotT
		err := v.Readdir(ino, in.Offset, func(d engine.DirEntry) bool {
			eo := out.AddDirLookupEntry(fuse.DirEntry{Name: d.Name, Ino: d.Ino, Mode: uint32(d.Type) << 12, Off: d.Next})
			if eo == nil {
				return false
			}
			if d.Name != "." && d.Name != ".." {
				slots = append(slots, slotT{d.Name, eo})
			}
			return true
		})
		if err != nil {
			return st(err)
		}
		for _, sl := range slots {
			if a, err := v.Lookup(ino, sl.name); err == nil {
				a = roAttr(a)
				a.Ino = viewID(slot, uid, a.Ino)
				r.m.fillEntry(sl.eo, a)
				sl.eo.Generation = uint64(uid)
			}
		}
		return fuse.OK
	}
	return r.m.ReadDirPlus(c, in, out)
}
