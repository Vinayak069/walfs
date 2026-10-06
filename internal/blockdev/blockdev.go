// Package blockdev defines the minimal block-device abstraction walfs is
// built on, plus the implementations used in production (a file or raw
// device), in unit tests (memory), and in crash-consistency testing (a
// recording device that logs every write and flush).
package blockdev

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
)

// Device is a flat byte-addressable store with an explicit durability barrier.
//
// The contract mirrors a real disk with a volatile write cache:
//   - WriteAt may be buffered, reordered and (at sector granularity) torn by a
//     power loss until a Flush returns.
//   - Flush returns only after every write that completed before it is durable.
type Device interface {
	ReadAt(p []byte, off int64) (int, error)
	WriteAt(p []byte, off int64) (int, error)
	Flush() error
	Size() int64
	Close() error
}

// ---------------------------------------------------------------------------
// FileDevice: a regular file or a raw block device.

// FileDevice stores the filesystem in a regular file or block device.
type FileDevice struct {
	f    *os.File
	size int64
}

// OpenFile opens path as a device. If the path is a regular file and size>0
// and the file is smaller, it is extended (sparsely) to size.
func OpenFile(path string, size int64, direct bool) (*FileDevice, error) {
	flags := os.O_RDWR
	if direct {
		flags |= syscall.O_DIRECT
	}
	f, err := os.OpenFile(path, flags, 0)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	sz := st.Size()
	if st.Mode().IsRegular() {
		if size > sz {
			if err := f.Truncate(size); err != nil {
				f.Close()
				return nil, err
			}
			sz = size
		}
	} else if sz == 0 { // block device: discover size by seeking
		if sz, err = f.Seek(0, io.SeekEnd); err != nil {
			f.Close()
			return nil, err
		}
	}
	return &FileDevice{f: f, size: sz}, nil
}

// CreateFile creates (or truncates) a regular file of the given size.
func CreateFile(path string, size int64) (*FileDevice, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, err
	}
	if err := f.Truncate(size); err != nil {
		f.Close()
		return nil, err
	}
	return &FileDevice{f: f, size: size}, nil
}

func (d *FileDevice) ReadAt(p []byte, off int64) (int, error)  { return d.f.ReadAt(p, off) }
func (d *FileDevice) WriteAt(p []byte, off int64) (int, error) { return d.f.WriteAt(p, off) }
func (d *FileDevice) Flush() error                             { return syscall.Fdatasync(int(d.f.Fd())) }
func (d *FileDevice) Size() int64                              { return d.size }
func (d *FileDevice) Close() error                             { return d.f.Close() }

// ---------------------------------------------------------------------------
// MemDevice: RAM backed, used by tests.

type MemDevice struct {
	mu   sync.RWMutex
	data []byte
}

func NewMem(size int64) *MemDevice { return &MemDevice{data: make([]byte, size)} }

// NewMemFrom wraps (a copy of) img.
func NewMemFrom(img []byte) *MemDevice {
	return &MemDevice{data: append([]byte(nil), img...)}
}

func (d *MemDevice) ReadAt(p []byte, off int64) (int, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if off < 0 || off >= int64(len(d.data)) {
		return 0, io.EOF
	}
	n := copy(p, d.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (d *MemDevice) WriteAt(p []byte, off int64) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if off < 0 || off+int64(len(p)) > int64(len(d.data)) {
		return 0, fmt.Errorf("memdev: write [%d,%d) out of range %d", off, off+int64(len(p)), len(d.data))
	}
	return copy(d.data[off:], p), nil
}

func (d *MemDevice) Flush() error { return nil }
func (d *MemDevice) Size() int64  { return int64(len(d.data)) }
func (d *MemDevice) Close() error { return nil }

// Image returns a copy of the current contents.
func (d *MemDevice) Image() []byte {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return append([]byte(nil), d.data...)
}

// ---------------------------------------------------------------------------
// FaultDevice: injects I/O errors for error-path testing.

// ErrInjected is returned by a FaultDevice once its budget is exhausted.
var ErrInjected = errors.New("blockdev: injected I/O error")

type FaultDevice struct {
	Device
	mu          sync.Mutex
	writesLeft  int // <0: unlimited
	flushesLeft int // <0: unlimited
}

// NewFault wraps d. Negative budgets mean "never fail".
func NewFault(d Device, writeBudget, flushBudget int) *FaultDevice {
	return &FaultDevice{Device: d, writesLeft: writeBudget, flushesLeft: flushBudget}
}

func (d *FaultDevice) WriteAt(p []byte, off int64) (int, error) {
	d.mu.Lock()
	if d.writesLeft == 0 {
		d.mu.Unlock()
		return 0, ErrInjected
	}
	if d.writesLeft > 0 {
		d.writesLeft--
	}
	d.mu.Unlock()
	return d.Device.WriteAt(p, off)
}

func (d *FaultDevice) Flush() error {
	d.mu.Lock()
	if d.flushesLeft == 0 {
		d.mu.Unlock()
		return ErrInjected
	}
	if d.flushesLeft > 0 {
		d.flushesLeft--
	}
	d.mu.Unlock()
	return d.Device.Flush()
}
