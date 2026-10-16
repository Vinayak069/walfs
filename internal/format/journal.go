package format

import (
	"fmt"

	"github.com/Vinayak069/walfs/internal/blockdev"
)

// ---------------------------------------------------------------------------
// Journal on-disk format
//
// The journal is a circular log of whole-block images ("physical redo
// logging", like ext4's jbd2). A transaction is laid out as
//
//	[REVOKE]* ( [DESCRIPTOR] [data block]{n} )* [COMMIT]
//
// Every block carries the transaction sequence number. The COMMIT block holds
// a CRC32C chained over every preceding block of the transaction, so a
// transaction is valid iff its commit block is present, intact, and matches
// the blocks before it. That single rule makes recovery independent of the
// order in which a disk persists the individual writes of a transaction.
//
// The journal superblock records the logical offset and sequence number of
// the oldest transaction that has not yet been checkpointed (written to its
// home location). Offsets are monotonically increasing logical block counts;
// physical position is offset % capacity, which removes any ambiguity between
// a full and an empty log.

const (
	jMagic      = 0x4A4E4C57 // "WLNJ"
	jTypeDesc   = 1
	jTypeCommit = 2
	jTypeRevoke = 3
	jHdr        = 24

	// DescCapacity is the number of block numbers one descriptor/revoke
	// block can carry.
	DescCapacity = (BlockSize - jHdr) / 8

	jsbMagic = 0x3142534A // "JSB1"
)

// JournalSuper is the journal's own superblock (first block of the journal).
type JournalSuper struct {
	Blocks  uint64 // total journal blocks including this one
	TailOff uint64 // logical offset of the oldest live transaction
	TailSeq uint64 // its sequence number
}

// Capacity is the number of log blocks (excluding the superblock).
func (j *JournalSuper) Capacity() uint64 { return j.Blocks - 1 }

func (j *JournalSuper) Marshal() []byte {
	b := make([]byte, BlockSize)
	le.PutUint32(b[0:], jsbMagic)
	le.PutUint32(b[4:], Version)
	le.PutUint64(b[8:], j.Blocks)
	le.PutUint64(b[16:], j.TailOff)
	le.PutUint64(b[24:], j.TailSeq)
	le.PutUint32(b[sbCRCOff:], CRC32C(0, b[:sbCRCOff]))
	return b
}

func ParseJournalSuper(b []byte) (*JournalSuper, error) {
	if le.Uint32(b[0:]) != jsbMagic {
		return nil, corrupt("bad journal superblock magic")
	}
	if le.Uint32(b[sbCRCOff:]) != CRC32C(0, b[:sbCRCOff]) {
		return nil, corrupt("journal superblock checksum mismatch")
	}
	return &JournalSuper{
		Blocks:  le.Uint64(b[8:]),
		TailOff: le.Uint64(b[16:]),
		TailSeq: le.Uint64(b[24:]),
	}, nil
}

func jHeader(typ uint32, seq uint64, count int) []byte {
	b := make([]byte, BlockSize)
	le.PutUint32(b[0:], jMagic)
	le.PutUint32(b[4:], typ)
	le.PutUint64(b[8:], seq)
	le.PutUint32(b[16:], uint32(count))
	return b
}

// EncodeDescriptor builds a descriptor block naming the home location of each
// of the data blocks that follow it.
func EncodeDescriptor(seq uint64, targets []uint64) []byte {
	b := jHeader(jTypeDesc, seq, len(targets))
	for i, t := range targets {
		le.PutUint64(b[jHdr+8*i:], t)
	}
	return b
}

// EncodeRevoke builds a revoke block: replaying older copies of these blocks
// must be skipped (the blocks were freed and may have been reused).
func EncodeRevoke(seq uint64, targets []uint64) []byte {
	b := jHeader(jTypeRevoke, seq, len(targets))
	for i, t := range targets {
		le.PutUint64(b[jHdr+8*i:], t)
	}
	return b
}

// TxnCRCSeed returns the initial CRC state for a transaction. Mixing in the
// filesystem UUID means a stale but internally valid transaction left over
// from a previous filesystem on the same device can never validate.
func TxnCRCSeed(uuid [16]byte, seq uint64) uint32 {
	var s [8]byte
	le.PutUint64(s[:], seq)
	return CRC32C(CRC32C(0, uuid[:]), s[:])
}

// EncodeCommit builds the commit block. nblocks counts every block of the
// transaction before the commit block; crc is the chained CRC of those blocks.
func EncodeCommit(seq uint64, nblocks int, crc uint32, tsNS int64) []byte {
	b := jHeader(jTypeCommit, seq, 0)
	le.PutUint32(b[16:], uint32(nblocks))
	le.PutUint32(b[20:], crc)
	le.PutUint64(b[24:], uint64(tsNS))
	le.PutUint32(b[32:], CRC32C(0, b[:32]))
	return b
}

// ---------------------------------------------------------------------------
// Scanning and recovery

// LoggedBlock locates one journaled block image.
type LoggedBlock struct {
	Target uint64 // home block number
	JOff   uint64 // logical journal offset of the image
}

// TxnInfo is a validated, committed transaction found in the journal.
type TxnInfo struct {
	Seq      uint64
	StartOff uint64
	EndOff   uint64 // offset just past the commit block
	Blocks   []LoggedBlock
	Revokes  []uint64
}

// Journal gives positional access to the log area of a device.
type Journal struct {
	Dev   blockdev.Device
	SB    *Superblock
	Super JournalSuper
}

// ReadJournal loads the journal superblock.
func ReadJournal(dev blockdev.Device, sb *Superblock) (*Journal, error) {
	buf := make([]byte, BlockSize)
	if _, err := dev.ReadAt(buf, int64(sb.JournalStart)*BlockSize); err != nil {
		return nil, err
	}
	js, err := ParseJournalSuper(buf)
	if err != nil {
		return nil, err
	}
	if js.Blocks != sb.JournalBlocks {
		return nil, corrupt("journal size %d != superblock %d", js.Blocks, sb.JournalBlocks)
	}
	return &Journal{Dev: dev, SB: sb, Super: *js}, nil
}

// PhysBlock maps a logical journal offset to a device block number.
func (j *Journal) PhysBlock(off uint64) uint64 {
	return j.SB.JournalStart + 1 + off%j.Super.Capacity()
}

func (j *Journal) readAt(off uint64, buf []byte) error {
	_, err := j.Dev.ReadAt(buf, int64(j.PhysBlock(off))*BlockSize)
	return err
}

// Scan walks the log from the tail and returns every valid committed
// transaction, in order, together with the offset where the log ends. A
// transaction that is torn or whose commit block is missing terminates the
// scan; this is the normal outcome of a crash and is not an error.
func (j *Journal) Scan() (txns []TxnInfo, endOff uint64, err error) {
	capacity := j.Super.Capacity()
	off := j.Super.TailOff
	seq := j.Super.TailSeq
	buf := make([]byte, BlockSize)
	for {
		start := off
		crc := TxnCRCSeed(j.SB.UUID, seq)
		ti := TxnInfo{Seq: seq, StartOff: start}
		valid := false
	parse:
		for {
			if off-j.Super.TailOff >= capacity {
				break // would overrun the log
			}
			if err = j.readAt(off, buf); err != nil {
				return nil, 0, err
			}
			if le.Uint32(buf[0:]) != jMagic || le.Uint64(buf[8:]) != seq {
				break
			}
			switch le.Uint32(buf[4:]) {
			case jTypeDesc, jTypeRevoke:
				typ := le.Uint32(buf[4:])
				n := int(le.Uint32(buf[16:]))
				if n < 1 || n > DescCapacity {
					break parse
				}
				targets := make([]uint64, n)
				for i := range targets {
					targets[i] = le.Uint64(buf[jHdr+8*i:])
				}
				crc = CRC32C(crc, buf)
				off++
				if typ == jTypeRevoke {
					ti.Revokes = append(ti.Revokes, targets...)
					continue
				}
				for _, t := range targets {
					if off-j.Super.TailOff >= capacity {
						break parse
					}
					if err = j.readAt(off, buf); err != nil {
						return nil, 0, err
					}
					crc = CRC32C(crc, buf)
					ti.Blocks = append(ti.Blocks, LoggedBlock{Target: t, JOff: off})
					off++
				}
			case jTypeCommit:
				if le.Uint32(buf[32:]) != CRC32C(0, buf[:32]) {
					break parse
				}
				if uint64(le.Uint32(buf[16:])) != off-start || le.Uint32(buf[20:]) != crc {
					break parse
				}
				off++
				ti.EndOff = off
				valid = true
				break parse
			default:
				break parse
			}
		}
		if !valid {
			return txns, start, nil
		}
		txns = append(txns, ti)
		seq++
	}
}

// RecoverResult summarises a journal replay.
type RecoverResult struct {
	Txns          int
	BlocksApplied int
	BlocksRevoked int
	FirstSeq      uint64
	NextSeq       uint64 // sequence number the next transaction must use
	NextOff       uint64 // logical offset the next transaction starts at
}

// Recover replays all committed transactions to their home locations and
// resets the log so it is empty. It is idempotent: if it is interrupted by a
// crash it can simply be run again, because replay only ever writes the final
// committed images of blocks.
func (j *Journal) Recover() (*RecoverResult, error) {
	txns, endOff, err := j.Scan()
	if err != nil {
		return nil, err
	}
	res := &RecoverResult{Txns: len(txns), NextSeq: j.Super.TailSeq, NextOff: endOff}
	if len(txns) > 0 {
		res.FirstSeq = txns[0].Seq
		res.NextSeq = txns[len(txns)-1].Seq + 1
	}
	// Pass 1: highest revoking sequence per block.
	revoked := map[uint64]uint64{}
	for _, t := range txns {
		for _, b := range t.Revokes {
			if t.Seq > revoked[b] {
				revoked[b] = t.Seq
			}
		}
	}
	// Pass 2: replay in order, skipping images superseded by a revoke.
	buf := make([]byte, BlockSize)
	for _, t := range txns {
		for _, lb := range t.Blocks {
			if lb.Target >= j.SB.TotalBlocks ||
				(lb.Target >= j.SB.JournalStart && lb.Target < j.SB.JournalStart+j.SB.JournalBlocks) {
				return nil, corrupt("journal txn %d targets illegal block %d", t.Seq, lb.Target)
			}
			if r, ok := revoked[lb.Target]; ok && r >= t.Seq {
				res.BlocksRevoked++
				continue
			}
			if err := j.readAt(lb.JOff, buf); err != nil {
				return nil, err
			}
			if _, err := j.Dev.WriteAt(buf, int64(lb.Target)*BlockSize); err != nil {
				return nil, err
			}
			res.BlocksApplied++
		}
	}
	if len(txns) > 0 {
		if err := j.Dev.Flush(); err != nil {
			return nil, err
		}
	}
	// Kill any stale (torn, uncommitted) transaction image sitting at the new
	// head so that it can never be mistaken for the start of a future one.
	zero := make([]byte, BlockSize)
	if _, err := j.Dev.WriteAt(zero, int64(j.PhysBlock(endOff))*BlockSize); err != nil {
		return nil, err
	}
	if err := j.Dev.Flush(); err != nil {
		return nil, err
	}
	j.Super.TailOff, j.Super.TailSeq = endOff, res.NextSeq
	if _, err := j.Dev.WriteAt(j.Super.Marshal(), int64(j.SB.JournalStart)*BlockSize); err != nil {
		return nil, err
	}
	if err := j.Dev.Flush(); err != nil {
		return nil, err
	}
	return res, nil
}

func (r *RecoverResult) String() string {
	return fmt.Sprintf("replayed %d txns (seq %d..%d), %d blocks applied, %d revoked",
		r.Txns, r.FirstSeq, r.NextSeq-1, r.BlocksApplied, r.BlocksRevoked)
}
