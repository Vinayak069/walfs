// Package crashtest is the verification harness for walfs. It provides
//
//   - a path-based random workload generator,
//   - deterministic tree snapshots (for comparing the engine against the
//     reference model and against recovered crash images),
//   - a differential runner (engine vs. model, operation by operation), and
//   - a power-failure simulator that enumerates the disk states a real device
//     could be left in (CrashMonkey/ALICE style) and checks that recovery
//     always yields a consistent, prefix-correct filesystem.
package crashtest

import (
	"github.com/vinayak-singh/walfs/internal/engine"
	"github.com/vinayak-singh/walfs/internal/model"
)

// VFS is the operation surface shared by the engine and the model.
type VFS interface {
	Lookup(parent uint64, name string) (engine.Attr, error)
	Getattr(ino uint64) (engine.Attr, error)
	Readlink(ino uint64) ([]byte, error)
	Mknod(ctx engine.Ctx, parent uint64, name string, mode uint32, rdev uint64) (engine.Attr, error)
	Mkdir(ctx engine.Ctx, parent uint64, name string, mode uint32) (engine.Attr, error)
	Symlink(ctx engine.Ctx, parent uint64, name string, target []byte) (engine.Attr, error)
	Link(ino uint64, newparent uint64, name string) (engine.Attr, error)
	Unlink(parent uint64, name string) error
	Rmdir(parent uint64, name string) error
	Rename(oldparent uint64, oldname string, newparent uint64, newname string, flags uint32) error
	Read(ino uint64, off uint64, p []byte) (int, error)
	Write(ctx engine.Ctx, ino uint64, off uint64, data []byte) (int, error)
	Setattr(ctx engine.Ctx, ino uint64, a engine.SetAttrIn) (engine.Attr, error)
	Fallocate(ctx engine.Ctx, ino uint64, mode uint32, off, length uint64) error
	// ReaddirAll lists a directory; pageSize>0 forces cookie-based paging.
	ReaddirAll(ino uint64, pageSize int) ([]engine.DirEntry, error)
	// Forget drops kernel references (a no-op in the model).
	Forget(ino uint64, n uint64)
	Sync() error
}

// EngineVFS adapts *engine.FS.
type EngineVFS struct{ *engine.FS }

func (e EngineVFS) ReaddirAll(ino uint64, pageSize int) ([]engine.DirEntry, error) {
	var out []engine.DirEntry
	cookie := uint64(0)
	for {
		n := 0
		last := cookie
		err := e.FS.Readdir(ino, cookie, func(d engine.DirEntry) bool {
			out = append(out, d)
			last = d.Next
			n++
			return pageSize <= 0 || n < pageSize
		})
		if err != nil {
			return nil, err
		}
		if n == 0 || (pageSize <= 0) || last == cookie {
			return out, nil
		}
		cookie = last
	}
}

// ModelVFS adapts *model.Model.
type ModelVFS struct{ *model.Model }

func (m ModelVFS) ReaddirAll(ino uint64, _ int) ([]engine.DirEntry, error) {
	return m.Model.Readdir(ino)
}
func (m ModelVFS) Forget(uint64, uint64) {}
func (m ModelVFS) Sync() error           { return nil }
