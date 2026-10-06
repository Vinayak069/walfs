package engine

import (
	"sync"

	"github.com/vinayak-singh/walfs/internal/blockdev"
	"github.com/vinayak-singh/walfs/internal/format"
)

// buf is one cached device block.
//
// Concurrency: the engine serialises all mutation with FS.mu (write lock), so
// the state flags below are only changed while no reader is running. Readers
// (holding FS.mu for reading) only read data and may race with each other
// solely on cache lookups, which are protected by cache.mu.
type buf struct {
	blk   uint64
	data  []byte
	ready chan struct{} // closed once data is valid (or err set)
	err   error
	used  bool // second-chance bit for eviction

	inTxn  bool // journaled block belonging to the running transaction
	isData bool // ordered-mode data block belonging to the running transaction
	ckpt   bool // committed to the journal, home-location write still pending
	// flushing: ordered data whose commit is in flight. The block must stay
	// cached (readers must see the new contents) until it is written home.
	flushing bool
}

func (b *buf) pinned() bool { return b.inTxn || b.isData || b.ckpt || b.flushing }

// cache is a bounded block cache. Pinned buffers (part of the running
// transaction or awaiting checkpoint) are never evicted.
type cache struct {
	dev blockdev.Device
	cap int

	mu sync.Mutex
	m  map[uint64]*buf

	pool sync.Pool
}

func newCache(dev blockdev.Device, capBlocks int) *cache {
	c := &cache{dev: dev, cap: capBlocks, m: make(map[uint64]*buf, capBlocks/2)}
	c.pool.New = func() any { return make([]byte, format.BlockSize) }
	return c
}

func (c *cache) alloc() []byte { return c.pool.Get().([]byte) }

// release returns a buffer's memory to the pool. Only legal when no reader can
// hold the buffer (i.e. under the FS write lock).
func (c *cache) release(b []byte) {
	if len(b) == format.BlockSize {
		c.pool.Put(b)
	}
}

// get returns the buffer for blk, reading it from the device on a miss.
func (c *cache) get(blk uint64) (*buf, error) {
	c.mu.Lock()
	if b, ok := c.m[blk]; ok {
		b.used = true
		c.mu.Unlock()
		<-b.ready
		if b.err != nil {
			return nil, b.err
		}
		return b, nil
	}
	c.evictLocked()
	b := &buf{blk: blk, ready: make(chan struct{}), used: true, data: c.alloc()}
	c.m[blk] = b
	c.mu.Unlock()

	if _, err := c.dev.ReadAt(b.data, int64(blk)*format.BlockSize); err != nil {
		b.err = err
		c.mu.Lock()
		delete(c.m, blk)
		c.mu.Unlock()
		close(b.ready)
		return nil, err
	}
	close(b.ready)
	return b, nil
}

// peek returns the buffer only if it is already cached and valid.
func (c *cache) peek(blk uint64) *buf {
	c.mu.Lock()
	b, ok := c.m[blk]
	c.mu.Unlock()
	if !ok {
		return nil
	}
	select {
	case <-b.ready:
		if b.err != nil {
			return nil
		}
		return b
	default:
		return nil
	}
}

// getNew returns a zero-filled buffer for blk without touching the device: for
// blocks that are about to be completely overwritten or freshly allocated.
// Any existing buffer is reused (its contents are zeroed by the caller if
// needed via zero=true).
func (c *cache) getNew(blk uint64, zero bool) *buf {
	c.mu.Lock()
	if b, ok := c.m[blk]; ok {
		b.used = true
		c.mu.Unlock()
		<-b.ready
		if b.err == nil {
			if zero {
				clear(b.data)
			}
			return b
		}
		c.mu.Lock()
	}
	c.evictLocked()
	b := &buf{blk: blk, ready: make(chan struct{}), used: true}
	b.data = c.alloc()
	if zero {
		clear(b.data)
	}
	close(b.ready)
	c.m[blk] = b
	c.mu.Unlock()
	return b
}

// drop removes blk from the cache entirely (the block was freed).
func (c *cache) drop(blk uint64) *buf {
	c.mu.Lock()
	b := c.m[blk]
	delete(c.m, blk)
	c.mu.Unlock()
	return b
}

// forget removes an unpinned buffer and recycles its memory.
func (c *cache) forget(b *buf) {
	c.mu.Lock()
	if c.m[b.blk] == b {
		delete(c.m, b.blk)
	}
	c.mu.Unlock()
	c.release(b.data)
	b.data = nil
}

// evictLocked trims the cache back under its capacity using a second-chance
// sweep over unpinned buffers. Go's randomised map iteration makes the sweep
// start at a random point, which spreads eviction without an LRU list.
func (c *cache) evictLocked() {
	if len(c.m) < c.cap {
		return
	}
	target := c.cap - c.cap/8
	for pass := 0; pass < 2 && len(c.m) > target; pass++ {
		for blk, b := range c.m {
			if b.pinned() {
				continue
			}
			select {
			case <-b.ready:
			default:
				continue // still loading
			}
			if b.used && pass == 0 {
				b.used = false
				continue
			}
			delete(c.m, blk)
			if len(c.m) <= target {
				return
			}
		}
	}
}

func (c *cache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}
