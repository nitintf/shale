package shale

import (
	"errors"
	"fmt"
	"sync"
)

// frameID identifies a slot in the pool.
//
// It is a distinct type from a page number on purpose. Both are small integers,
// and swapping them is the easiest mistake to make in this file, so the compiler
// should catch it
type frameID int

// maxUsageCount caps how many reprieves a frame can bank.
//
// Higher means hot pages are protected harder, at the cost of a longer sweep:
// the worst case is maxUsageCount laps decrementing everything to zero plus one
// more to find a victim. Postgres uses 5.
const maxUsageCount = 5

// frame is one slot in the buffer pool: one page's worth of bytes, plus the
// bookkeeping needed to decide when that page can be evicted.
type frame struct {
	data       []byte // exactly PageSize, a window into the pool's slab
	pageNum    uint32 // which page these bytes belong to
	pinCount   int    // callers using this frame right now; nonzero means do not evict
	dirty      bool   // modified since read in, so eviction must write it back first
	valid      bool   // false until this frame has been filled for the first time
	usageCount int    // how many sweeps this frame survives; see maxUsageCount
}

var (
	// ErrPoolExhausted means every frame is pinned, so no page can be brought in.
	//
	// It is not a transient condition to retry: the goroutine that would unpin
	// something is the one blocked here. In practice it means a caller fetched a
	// page and never unpinned it
	ErrPoolExhausted = errors.New("shale: buffer pool exhausted, every frame is pinned")

	// ErrNotPinned means the caller tried to unpin a page that is not in the pool.
	ErrNotPinned = errors.New("shale: page is not pinned")
)

// BufferPool keeps a fixed number of pages in memory and decides which to evict
// when it runs out of room. Once it exists, it is the only component that reads
// and writes through the Pager.
//
// The mutex protects the pool's own bookkeeping, not the contents of the pages
// it hands out. Two goroutines that fetch the same page and both modify it will
// still corrupt it.
type BufferPool struct {
	mu        sync.Mutex
	pager     *Pager
	slab      []byte             // one allocation, carved into frames
	frames    []frame            // indexed by frameID
	table     map[uint32]frameID // page number -> the frame holding it
	free      []frameID          // frames that have never been filled
	clockHand int                // where the next eviction sweep starts
}

// NewBufferPool creates a pool of nFrames frames over pg.
//
// It panics if nFrames is less than one: such a pool could serve nothing, and
// would spin looking for a victim frame that cannot exist.
func NewBufferPool(pg *Pager, nFrames int) *BufferPool {
	if nFrames < 1 {
		panic("shale: buffer pool needs at least one frame")
	}

	bp := &BufferPool{
		pager:  pg,
		slab:   make([]byte, nFrames*PageSize),
		frames: make([]frame, nFrames),
		table:  make(map[uint32]frameID, nFrames),
		free:   make([]frameID, 0, nFrames),
	}

	for i := range bp.frames {
		lo, hi := i*PageSize, (i+1)*PageSize
		// The third index caps the capacity at hi. Without it this slice would
		// run to the end of the slab, so an accidental append would silently
		// overwrite the next frame's bytes.
		bp.frames[i].data = bp.slab[lo:hi:hi]
	}

	// Filled in reverse so that popping from the end hands out frame 0 first,
	// which makes test output far easier to read.
	for i := nFrames - 1; i >= 0; i-- {
		bp.free = append(bp.free, frameID(i))
	}

	return bp
}

// findVictim runs the clock sweep and returns a frame that may be reused.
//
// Each access banks a reprieve, up to maxUsageCount, and each sweep past a frame
// spends one. So a page touched five times survives five sweeps while a page
// touched once survives one, and a page nobody wants is taken.
// That approximates LRU for one bit per frame and O(1) work, instead of a linked
// list that has to be reordered on every single read.
//
// Caller must hold bp.mu.
func (bp *BufferPool) findVictim() (frameID, bool) {
	// The bound has to allow maxUsageCount laps spent purely on decrementing,
	// plus one more lap to actually take a victim. Anything shorter reports the
	// pool exhausted while evictable frames are sitting right there.
	limit := (maxUsageCount + 1) * len(bp.frames)

	for range limit {
		id := frameID(bp.clockHand)

		// Advance before deciding. Skipping without advancing looks at the same
		// frame forever.
		bp.clockHand = (bp.clockHand + 1) % len(bp.frames)

		f := &bp.frames[id]

		if f.pinCount > 0 {
			continue // in use, evicting it would corrupt the holder's page
		}
		if f.usageCount > 0 {
			f.usageCount-- // spend one reprieve and move on
			continue
		}

		return id, true
	}

	return 0, false
}

// grabFrame returns a frame ready to hold a new page, evicting one if needed.
//
// Caller must hold bp.mu.
func (bp *BufferPool) grabFrame() (frameID, error) {
	// A frame that has never been filled costs nothing to take.
	if n := len(bp.free); n > 0 {
		id := bp.free[n-1]
		bp.free = bp.free[:n-1]
		return id, nil
	}

	id, ok := bp.findVictim()
	if !ok {
		return 0, ErrPoolExhausted
	}

	f := &bp.frames[id]

	// A clean victim is free to discard: the disk copy is already identical.
	// A dirty one has to go back first or its changes are lost.
	if f.dirty {
		if err := bp.pager.WritePage(f.pageNum, PageFrom(f.data)); err != nil {
			return 0, fmt.Errorf("evicting page %d: %w", f.pageNum, err)
		}
		// Cleared only after the write succeeds. That is the invariant: dirty
		// is set by UnpinPage and cleared by nothing except a completed write.
		f.dirty = false
	}

	// Drop the old mapping before the frame is reused. Miss this and the table
	// still claims the old page lives here, so a later fetch reports a hit and
	// hands back a completely different page with no error anywhere.
	if f.valid {
		delete(bp.table, f.pageNum)
	}

	return id, nil
}

// fetchLocked is FetchPage without the locking, so callers already holding the
// mutex can reuse it.
//
// Caller must hold bp.mu.
func (bp *BufferPool) fetchLocked(n uint32) (*Page, error) {
	// Hit. No disk access at all, and in a healthy pool this is the
	// overwhelming majority of calls.
	if id, ok := bp.table[n]; ok {
		f := &bp.frames[id]
		f.pinCount++
		if f.usageCount < maxUsageCount {
			f.usageCount++
		}
		return PageFrom(f.data), nil
	}

	// Miss. Take a free frame, or evict one and write it back if it is dirty.
	id, err := bp.grabFrame()
	if err != nil {
		return nil, err
	}

	f := &bp.frames[id]

	// TODO: this holds the pool mutex across a disk read, which blocks every
	// other goroutine for the duration. Postgres solves it with a per-buffer
	// io_in_progress flag so others can wait on just this page.
	if err := bp.pager.readInto(n, f.data); err != nil {
		// The frame holds nothing usable and grabFrame already removed its old
		// table entry, so hand it back rather than orphaning it.
		f.valid = false
		f.dirty = false
		f.pageNum = 0
		bp.free = append(bp.free, id)
		return nil, err
	}

	f.pageNum = n
	f.pinCount = 1
	f.dirty = false
	f.valid = true
	f.usageCount = 1 // one reprieve: we just paid a disk read for these bytes
	bp.table[n] = id

	return PageFrom(f.data), nil
}

// FetchPage returns page n, resident in memory and pinned.
//
// Every successful FetchPage must be matched by exactly one UnpinPage. Miss one
// and that frame stays pinned forever; miss enough and the pool can serve
// nothing at all.
//
//	page, err := bp.FetchPage(n)
//	if err != nil { ... }
//	defer bp.UnpinPage(n, false)
//
// The returned Page aliases the frame's memory. It stays valid only while the
// page is pinned: once unpinned the frame can be evicted and refilled with a
// different page, and the pointer would then be showing someone else's bytes.
func (bp *BufferPool) FetchPage(n uint32) (*Page, error) {
	bp.mu.Lock()
	defer bp.mu.Unlock()

	return bp.fetchLocked(n)
}

// UnpinPage releases one pin on page n.
//
// dirty says whether the caller modified the page. The pool cannot know that on
// its own, so the caller has to tell it.
func (bp *BufferPool) UnpinPage(n uint32, dirty bool) error {
	bp.mu.Lock()
	defer bp.mu.Unlock()

	// Not in the table at all: the caller is unpinning something it never
	// fetched. A pinned page can never be evicted, so if it was fetched and is
	// still pinned it must be here.
	id, ok := bp.table[n]
	if !ok {
		return fmt.Errorf("%w: page %d is not in the pool", ErrNotPinned, n)
	}

	f := &bp.frames[id]

	// Already at zero: this is a second unpin for one fetch. Letting the count
	// go negative would make the frame look evictable while someone still holds
	// a pointer into it.
	if f.pinCount <= 0 {
		return fmt.Errorf("%w: page %d is already unpinned", ErrNotPinned, n)
	}

	f.pinCount--

	// OR it in, never assign. Someone else may have dirtied this page and
	// unpinned before us, and assigning false would wipe their flag.
	if dirty {
		f.dirty = true
	}

	return nil
}

// FlushPage writes page n back to disk if it is dirty.
//
// Flushing neither evicts nor unpins: the frame stays where it is, holding the
// same page. A pinned page can be flushed.
//
// A page that is not in the pool is not an error. There is simply nothing to do.
//
// TODO: the pool mutex protects the pool's bookkeeping, not the contents of the
// pages it hands out. If another goroutine is midway through an Insert on this
// page while the write happens, a half-modified page reaches the disk. Postgres
// prevents this with a per-buffer content lock, held while reading or writing
// the page bytes, which is separate from the pin that keeps the frame resident.
// Single-goroutine use cannot hit this today.
func (bp *BufferPool) FlushPage(n uint32) error {
	bp.mu.Lock()
	defer bp.mu.Unlock()

	return bp.flushLocked(n)
}

// flushLocked is FlushPage without the locking, so callers already holding the
// mutex can reuse it. Go mutexes are not reentrant, so a locked method calling
// an exported one deadlocks against itself.
//
// Caller must hold bp.mu.
func (bp *BufferPool) flushLocked(n uint32) error {
	id, ok := bp.table[n]
	if !ok {
		return nil // not in the pool, so nothing to flush
	}

	// The address, not a copy. Indexing a slice of structs yields a copy, so
	// f := bp.frames[id] would make the f.dirty = false below write to a
	// temporary and leave the real frame dirty forever.
	f := &bp.frames[id]

	if !f.dirty {
		return nil // already clean, nothing to do
	}

	if err := bp.pager.WritePage(f.pageNum, PageFrom(f.data)); err != nil {
		return fmt.Errorf("flushing page %d: %w", n, err)
	}

	f.dirty = false // cleared only after the write succeeds

	return nil
}

// FlushAll writes every dirty page in the pool back to disk.
//
// It stops at the first error, so some pages may already have been written.
func (bp *BufferPool) FlushAll() error {
	bp.mu.Lock()
	defer bp.mu.Unlock()

	for n := range bp.table {
		if err := bp.flushLocked(n); err != nil {
			return err
		}
	}

	return nil
}

// NewPage grows the file by one page and returns it, pinned, along with its
// page number.
//
// Like FetchPage, the returned page must be unpinned exactly once.
func (bp *BufferPool) NewPage() (*Page, uint32, error) {
	bp.mu.Lock()
	defer bp.mu.Unlock()

	// The lock has to cover Allocate too. Pager has no mutex of its own, so two
	// concurrent NewPage calls would race on nPages and hand out the same
	// number twice.
	n, err := bp.pager.Allocate()
	if err != nil {
		return nil, 0, err
	}

	page, err := bp.fetchLocked(n)
	if err != nil {
		return nil, 0, err
	}

	return page, n, nil
}

// Close flushes every dirty page, syncs, and closes the file.
//
// This is the only place in the engine where data is guaranteed to have reached
// the disk. The pool must not be used afterwards.
func (bp *BufferPool) Close() error {
	// No lock here. FlushAll takes it, and taking it first would deadlock for
	// the same reason FlushAll could not call FlushPage.
	if err := bp.FlushAll(); err != nil {
		return err
	}

	if err := bp.pager.Sync(); err != nil {
		return err
	}

	return bp.pager.Close()
}
