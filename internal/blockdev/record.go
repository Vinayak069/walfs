package blockdev

import "sync"

// OpKind distinguishes entries in a RecordingDevice log.
type OpKind uint8

const (
	OpWrite OpKind = iota
	OpFlush
)

// LogEntry is one device-level event, in issue order.
type LogEntry struct {
	Kind OpKind
	Off  int64
	Data []byte // private copy; nil for flushes
	Tag  int    // caller-defined marker (the harness stores the op index)
}

// RecordingDevice wraps a MemDevice and logs every write and flush so that a
// crash harness can later reconstruct any state a real disk could have been in
// at power loss: all writes before the last completed flush are durable; any
// subset of the later writes (each possibly torn at sector granularity) may
// or may not have reached the platter.
type RecordingDevice struct {
	mem  *MemDevice
	mu   sync.Mutex
	base []byte // image at the time recording started
	log  []LogEntry
	tag  int

	// DropFlushes makes Flush a silent no-op that is not logged: the device
	// then behaves as if it ignored cache-flush commands. Used as a negative
	// control to prove that the crash harness detects missing barriers.
	DropFlushes bool
}

// NewRecording starts recording on top of mem. The current contents of mem are
// remembered as the durable base image.
func NewRecording(mem *MemDevice) *RecordingDevice {
	return &RecordingDevice{mem: mem, base: mem.Image()}
}

func (d *RecordingDevice) SetTag(t int) { d.mu.Lock(); d.tag = t; d.mu.Unlock() }

func (d *RecordingDevice) ReadAt(p []byte, off int64) (int, error) { return d.mem.ReadAt(p, off) }
func (d *RecordingDevice) Size() int64                             { return d.mem.Size() }
func (d *RecordingDevice) Close() error                            { return nil }

func (d *RecordingDevice) WriteAt(p []byte, off int64) (int, error) {
	n, err := d.mem.WriteAt(p, off)
	if err != nil {
		return n, err
	}
	d.mu.Lock()
	d.log = append(d.log, LogEntry{Kind: OpWrite, Off: off, Data: append([]byte(nil), p...), Tag: d.tag})
	d.mu.Unlock()
	return n, nil
}

func (d *RecordingDevice) Flush() error {
	if d.DropFlushes {
		return nil
	}
	d.mu.Lock()
	d.log = append(d.log, LogEntry{Kind: OpFlush, Tag: d.tag})
	d.mu.Unlock()
	return nil
}

// Base returns the durable image from when recording started.
func (d *RecordingDevice) Base() []byte { return d.base }

// Log returns the recorded events. The slice must not be modified.
func (d *RecordingDevice) Log() []LogEntry {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.log
}
