// Package shale is a storage engine built from scratch.
package shale

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
)

// A page is a fixed-size byte array with three regions:
//
//	+--------+------------------+---------+----------------------+
//	| header | slot array ->    |  free   |   <- record data     |
//	+--------+------------------+---------+----------------------+
//	0        8            freeStart    freeEnd                4096
//
// The slot array grows forward from the header, record data grows backward from
// the end, and free space is whatever is left in the middle.
//
// A record is addressed by its slot index, never by a byte offset. That
// indirection is the whole point: a record can move inside its page during
// Compact without anything outside the page needing to change.

const (
	// PageSize is fixed for the life of a database file. Page N begins at byte
	// N*PageSize, which is the only reason locating a page is pure arithmetic.
	PageSize = 4096 // 4 KB

	headerSize = 8 // 8 Bytes
	slotSize   = 4 // 4 Bytes
)

// Header field offsets within a page.
const (
	offNumSlots  = 0 // uint16: slots ever allocated, including dead ones
	offFreeStart = 2 // uint16: first free byte after the slot array
	offFreeEnd   = 4 // uint16: first byte of the record area
	offFlags     = 6 // uint16: reserved (leaf vs internal, once there is a B-tree)
)

var (
	ErrPageFull   = errors.New("shale: page full")
	ErrNoSuchSlot = errors.New("shale: no such slot")
	ErrDeleted    = errors.New("shale: record deleted")
)

// Page is one fixed-size page. It holds exactly the bytes that live on disk:
// nothing is parsed on the way in or out, which is what makes a cached page and
// a stored page the same thing.
type Page struct {
	data []byte
}

func NewPage() *Page {
	p := &Page{
		data: make([]byte, PageSize),
	}
	p.setNumSlots(0)
	p.setFreeStart(headerSize)
	p.setFreeEnd(PageSize)

	return p
}

// PageFrom wraps an existing buffer as a page without copying or initialising it.
//
// NewPage is for making a brand new empty page: it allocates and writes a fresh
// header. PageFrom is for bytes that are already a page, freshly read off disk or
// sitting in a buffer pool frame, so it leaves the header alone.
//
// The buffer is not copied. The returned Page reads and writes the caller's
// bytes directly, which is what makes a cached page byte-for-byte identical to
// the one on disk.
func PageFrom(buf []byte) *Page {
	if len(buf) != PageSize {
		panic("shale: buffer is not exactly one page")
	}
	return &Page{data: buf}
}

func (p *Page) u16(off int) uint16 {
	return binary.LittleEndian.Uint16(p.data[off:])
}

func (p *Page) setU16(off int, v uint16) {
	binary.LittleEndian.PutUint16(p.data[off:], v)
}

func (p *Page) NumSlots() uint16 {
	return p.u16(offNumSlots)
}

func (p *Page) setNumSlots(v uint16) {
	p.setU16(offNumSlots, v)
}

func (p *Page) freeStart() uint16 {
	return p.u16(offFreeStart)
}

func (p *Page) setFreeStart(v uint16) {
	p.setU16(offFreeStart, v)
}

func (p *Page) freeEnd() uint16 {
	return p.u16(offFreeEnd)
}

func (p *Page) setFreeEnd(v uint16) {
	p.setU16(offFreeEnd, v)
}

func (p *Page) FreeSpace() uint16 {
	return p.freeEnd() - p.freeStart()
}

func (p *Page) readSlot(i uint16) (offset, length uint16) {
	base := headerSize + int(i)*slotSize
	return p.u16(base), p.u16(base + 2)
}

func (p *Page) writeSlot(i, offset, length uint16) {
	base := headerSize + int(i)*slotSize
	p.setU16(base, offset)
	p.setU16(base+2, length)
}

// Insert copies rec into the page and returns the slot index that addresses it.
// It returns ErrPageFull when the record and its slot do not fit.
//
// The returned index is stable: it keeps addressing this record even after
// Compact moves the record's bytes to a different offset inside the page.
//
// When the record does not fit in the contiguous free space, Insert compacts the
// page and tries once more before giving up, so a page holding enough total free
// space in fragments will still accept the record.
func (p *Page) Insert(rec []byte) (uint16, error) {
	// A record costs its own bytes plus the slot that points at it. Forgetting
	// the slot is the classic way to overflow a page.
	need := len(rec) + slotSize

	// Compare in int, never in uint16. len(rec) can exceed 65535, and narrowing
	// it before the check would wrap a huge record into a small number that
	// sails through and then writes past the end of the page.
	if need > int(p.FreeSpace()) {
		p.Compact()

		if need > int(p.FreeSpace()) {
			return 0, ErrPageFull
		}
	}

	// The new slot's index is the current count, read before anything moves.
	slot := p.NumSlots()

	// Records grow backward from the end of the page, so the new record begins
	// len(rec) bytes below the current record area.
	newFreeEnd := p.freeEnd() - uint16(len(rec))

	// Safe to narrow now: the check above proved len(rec) fits inside the page.
	if n := copy(p.data[newFreeEnd:], rec); n != len(rec) {
		// copy truncates silently when the destination is short, so a mismatch
		// means the arithmetic above is wrong, not the caller's input.
		panic("shale: short copy during Insert, page arithmetic is broken")
	}

	// Point the slot at the bytes just written. This has to happen before
	// freeStart moves, because the new slot belongs at the current freeStart.
	p.writeSlot(slot, newFreeEnd, uint16(len(rec)))

	// Commit the header. All three fields move together or the page is corrupt.
	p.setFreeEnd(newFreeEnd)
	p.setFreeStart(p.freeStart() + slotSize)
	p.setNumSlots(slot + 1)

	return slot, nil
}

// Read returns the record addressed by slot.
//
// The returned slice aliases the page's buffer, it is not a copy. Treat it as
// read-only, and copy it if it needs to outlive the next modification of this
// page. It returns ErrNoSuchSlot when slot is out of range, and ErrDeleted when
// the slot points at a deleted record.
func (p *Page) Read(slot uint16) ([]byte, error) {
	if slot >= p.NumSlots() {
		return nil, ErrNoSuchSlot
	}

	offset, length := p.readSlot(slot)

	if offset == 0 {
		return nil, ErrDeleted
	}

	return p.data[int(offset) : int(offset)+int(length)], nil
}

func (p *Page) Delete(slot uint16) error {
	if slot >= p.NumSlots() {
		return ErrNoSuchSlot
	}

	offset, length := p.readSlot(slot)

	if offset == 0 {
		return ErrDeleted
	}

	p.writeSlot(slot, 0, length)

	return nil
}

// Compact slides the live records together, reclaiming the space left behind by
// deleted ones.
//
// Deleting a record only marks its slot dead, so the bytes it occupied stay put
// and the page ends up with two kinds of free space: the contiguous middle
// region, which an insert can use, and holes scattered through the record area,
// which it cannot. Compact turns the second kind into the first.
//
// Slot indices survive unchanged. Only the offsets stored in the slots move,
// which is the entire reason records are addressed by slot rather than by byte
// offset: an index elsewhere can point into this page and stay valid across a
// compaction it never hears about.
//
// Real engines all have this routine. Postgres calls it PageRepairFragmentation
// and runs it from VACUUM and from HOT pruning, which happens opportunistically
// whenever a query touches a page with dead tuples on it. InnoDB calls it
// btr_page_reorganize, SQLite calls it defragmentPage. In a B-tree page it earns
// its keep by avoiding a page split, which costs far more than a compaction.
func (p *Page) Compact() {
	// Scratch space for the rebuilt record area, page-sized so an offset means
	// the same thing in both buffers and the copy back needs no translation.
	// Compacting in place would overwrite records that have not moved yet.
	newData := make([]byte, PageSize)

	n := p.NumSlots()
	newFreeEnd := uint16(PageSize)

	for i := range n {
		offset, length := p.readSlot(i)
		if offset == 0 {
			continue // skip deleted records
		}

		newFreeEnd -= length
		copy(newData[newFreeEnd:], p.data[offset:offset+length])
		p.writeSlot(i, newFreeEnd, length)
	}

	copy(p.data[newFreeEnd:], newData[newFreeEnd:])

	p.setFreeEnd(newFreeEnd)
}

// Reset empties the page, discarding every record and every tombstone at once.
//
// Deleted slots normally live forever, because an index elsewhere may still
// address them by index and renumbering would silently repoint it at the wrong
// record. Once nothing outside the page references any of these slots, that
// constraint is gone and the whole page can be reused from scratch. This is a
// header rewrite rather than a scan, so it costs nothing.
//
// The caller is responsible for knowing that no outside reference survives.
// Calling this while an index still points here corrupts that index.
func (p *Page) Reset() {
	p.setNumSlots(0)
	p.setFreeStart(headerSize)
	p.setFreeEnd(PageSize)
}

// check validates every invariant the page layout depends on.
// It is a debugging aid, not input validation. Callers cannot break these from
// outside, so a failure means the page code itself is wrong. Call it at the end
// of every mutation in tests: it reports corruption at the operation that caused
// it, instead of three operations later when a read comes back as garbage.
func (p *Page) check() error {
	numSlots := p.NumSlots()
	freeStart := p.freeStart()
	freeEnd := p.freeEnd()

	if freeStart < headerSize {
		return fmt.Errorf("freeStart %d is inside the header (< %d)", freeStart, headerSize)
	}
	if freeEnd > PageSize {
		return fmt.Errorf("freeEnd %d is past the end of the page (> %d)", freeEnd, PageSize)
	}
	// uint16 subtraction wraps silently, so FreeSpace would report ~65000 here
	// rather than anything obviously wrong. This is the check that catches it.
	if freeStart > freeEnd {
		return fmt.Errorf("freeStart %d > freeEnd %d, free space has wrapped", freeStart, freeEnd)
	}
	if want := uint16(headerSize + int(numSlots)*slotSize); freeStart != want {
		return fmt.Errorf("freeStart %d disagrees with numSlots %d, want %d", freeStart, numSlots, want)
	}

	// Collect the live records so they can be checked for overlap.
	type span struct{ start, end uint16 }
	live := make([]span, 0, numSlots)

	for i := uint16(0); i < numSlots; i++ {
		offset, length := p.readSlot(i)
		if offset == 0 {
			continue // tombstone, it points at nothing by design
		}
		if offset < freeEnd {
			return fmt.Errorf("slot %d offset %d is inside free space (freeEnd %d)", i, offset, freeEnd)
		}
		end := int(offset) + int(length)
		if end > PageSize {
			return fmt.Errorf("slot %d spans %d..%d, past the end of the page", i, offset, end)
		}
		live = append(live, span{offset, uint16(end)})
	}

	sort.Slice(live, func(a, b int) bool { return live[a].start < live[b].start })

	for i := 1; i < len(live); i++ {
		if live[i].start < live[i-1].end {
			return fmt.Errorf("records overlap: %d..%d and %d..%d",
				live[i-1].start, live[i-1].end, live[i].start, live[i].end)
		}
	}

	return nil
}
