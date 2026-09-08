package shale

import (
	"bytes"
	"errors"
	"path/filepath"
	"sync"
	"testing"
)

// newTestPool opens a pool of nFrames frames over a fresh file.
func newTestPool(t *testing.T, nFrames int) *BufferPool {
	t.Helper()

	pg, err := Open(filepath.Join(t.TempDir(), "pool.db"))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	bp := NewBufferPool(pg, nFrames)
	t.Cleanup(func() { bp.Close() })

	return bp
}

// setFrames puts the pool into a chosen state so the sweep can be tested
// directly, without going through the fetch path.
func setFrames(bp *BufferPool, states []struct {
	pinCount   int
	usageCount int
}) {
	for i, s := range states {
		f := &bp.frames[i]
		f.valid = true
		f.pageNum = uint32(i)
		f.pinCount = s.pinCount
		f.usageCount = s.usageCount
		bp.table[uint32(i)] = frameID(i)
	}
	bp.free = bp.free[:0]
}

type frameState = struct {
	pinCount   int
	usageCount int
}

// With a clear ref bit the frame under the hand is taken straight away, no
// second chance needed.
func TestFindVictimTakesFirstClearFrame(t *testing.T) {
	bp := newTestPool(t, 4)
	setFrames(bp, []frameState{{0, 0}, {0, 0}, {0, 0}, {0, 0}})

	id, ok := bp.findVictim()
	if !ok {
		t.Fatal("findVictim found nothing with four clear unpinned frames")
	}
	if id != 0 {
		t.Errorf("findVictim() = %d, want 0 (the frame under the hand)", id)
	}
	if bp.clockHand != 1 {
		t.Errorf("clockHand = %d, want 1 (it must advance past the victim)", bp.clockHand)
	}
}

// The second chance: a set bit costs the frame one pass, not its life.
func TestFindVictimSecondChance(t *testing.T) {
	bp := newTestPool(t, 4)
	setFrames(bp, []frameState{{0, 1}, {0, 0}, {0, 0}, {0, 0}})

	id, ok := bp.findVictim()
	if !ok {
		t.Fatal("findVictim found nothing")
	}
	if id != 1 {
		t.Errorf("findVictim() = %d, want 1 (frame 0 had its bit set)", id)
	}
	if got := bp.frames[0].usageCount; got != 0 {
		t.Errorf("frame 0 usageCount = %d, want 0: the sweep must spend one on the way past", got)
	}
}

// The worst case, and the reason the bound is (maxUsageCount+1)*len rather than
// 2*len: every frame is at the cap, so several laps go by spending reprieves and
// evicting nothing, then a victim is taken. All inside a single call.
func TestFindVictimEveryFrameAtMaxUsage(t *testing.T) {
	bp := newTestPool(t, 4)
	setFrames(bp, []frameState{
		{0, maxUsageCount},
		{0, maxUsageCount},
		{0, maxUsageCount},
		{0, maxUsageCount},
	})

	id, ok := bp.findVictim()
	if !ok {
		t.Fatal("findVictim gave up with every frame at max usage; the bound is too short")
	}
	if id != 0 {
		t.Errorf("findVictim() = %d, want 0 (the hand comes back round to it)", id)
	}

	// Every other frame should have been decremented once per lap.
	for i := 1; i < 4; i++ {
		if got := bp.frames[i].usageCount; got != 0 {
			t.Errorf("frame %d usageCount = %d, want 0 after the draining laps", i, got)
		}
	}
}

// A frequently used page outlives a rarely used one, which is the entire point
// of counting instead of using a single bit.
func TestFindVictimPrefersColdFrames(t *testing.T) {
	bp := newTestPool(t, 3)
	setFrames(bp, []frameState{
		{0, 5}, // hot
		{0, 1}, // touched once
		{0, 5}, // hot
	})

	id, ok := bp.findVictim()
	if !ok {
		t.Fatal("findVictim found nothing")
	}
	if id != 1 {
		t.Errorf("findVictim() = %d, want 1 (the least used frame)", id)
	}
	// The hand passed frame 0 twice on the way (0, 1, 2, 0, then 1 again), so it
	// spent two of its five reprieves and still survives comfortably.
	if got := bp.frames[0].usageCount; got != 3 {
		t.Errorf("hot frame 0 usageCount = %d, want 3", got)
	}
}

func TestFindVictimSkipsPinned(t *testing.T) {
	bp := newTestPool(t, 4)
	setFrames(bp, []frameState{{1, 0}, {2, 0}, {0, 0}, {0, 0}})

	id, ok := bp.findVictim()
	if !ok {
		t.Fatal("findVictim found nothing with two unpinned frames available")
	}
	if id != 2 {
		t.Errorf("findVictim() = %d, want 2 (frames 0 and 1 are pinned)", id)
	}

	// A pinned frame is skipped before its bit is even looked at, so a pinned
	// frame with a set bit keeps it.
	if bp.frames[0].pinCount != 1 || bp.frames[1].pinCount != 2 {
		t.Error("the sweep modified a pinned frame")
	}
}

// No victim exists. This must return rather than spin, which is what the loop
// bound guarantees.
func TestFindVictimAllPinned(t *testing.T) {
	bp := newTestPool(t, 4)
	setFrames(bp, []frameState{{1, 1}, {1, 1}, {1, 0}, {1, 0}})

	if _, ok := bp.findVictim(); ok {
		t.Error("findVictim returned a victim with every frame pinned")
	}
}

// The hand carries over between calls, so pressure rotates around the frames
// instead of always falling on frame 0.
func TestClockHandPersistsAcrossCalls(t *testing.T) {
	bp := newTestPool(t, 4)
	setFrames(bp, []frameState{{0, 0}, {0, 0}, {0, 0}, {0, 0}})

	for want := frameID(0); want < 4; want++ {
		id, ok := bp.findVictim()
		if !ok {
			t.Fatalf("call %d found nothing", want)
		}
		if id != want {
			t.Errorf("call %d returned frame %d, want %d", want, id, want)
		}
	}

	// And it wraps.
	if bp.clockHand != 0 {
		t.Errorf("clockHand = %d after a full lap, want 0", bp.clockHand)
	}
}

// A hit returns the same frame, so two fetches of one page share memory. This
// is the behaviour TestReadPageHasNoCache pins as absent at the pager level.
func TestFetchPageHitSharesTheFrame(t *testing.T) {
	bp := newTestPool(t, 4)

	first, n, err := bp.NewPage()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Insert([]byte("written through the first pointer")); err != nil {
		t.Fatal(err)
	}

	second, err := bp.FetchPage(n)
	if err != nil {
		t.Fatalf("FetchPage(%d) failed: %v", n, err)
	}

	got, err := second.Read(0)
	if err != nil {
		t.Fatalf("the second fetch cannot see the first fetch's insert: %v", err)
	}
	if !bytes.Equal(got, []byte("written through the first pointer")) {
		t.Errorf("Read(0) = %q, want the record inserted through the other pointer", got)
	}

	bp.UnpinPage(n, true)
	bp.UnpinPage(n, false)
}

// A pool far smaller than the file still serves every page. This is the line in
// docs.md that closes out stage 0.
func TestEvictionWithPoolSmallerThanFile(t *testing.T) {
	const nPages = 8
	bp := newTestPool(t, 2) // two frames, eight pages

	want := make(map[uint32][]byte, nPages)
	for i := 0; i < nPages; i++ {
		page, n, err := bp.NewPage()
		if err != nil {
			t.Fatalf("NewPage %d failed: %v", i, err)
		}
		rec := []byte{byte('a' + i), byte('a' + i), byte('a' + i)}
		if _, err := page.Insert(rec); err != nil {
			t.Fatal(err)
		}
		if err := bp.UnpinPage(n, true); err != nil {
			t.Fatal(err)
		}
		want[n] = rec
	}

	for n, rec := range want {
		page, err := bp.FetchPage(n)
		if err != nil {
			t.Fatalf("FetchPage(%d) failed: %v", n, err)
		}
		got, err := page.Read(0)
		if err != nil {
			t.Fatalf("page %d: %v", n, err)
		}
		if !bytes.Equal(got, rec) {
			t.Errorf("page %d = %q, want %q (dirty page lost across eviction)", n, got, rec)
		}
		if err := bp.UnpinPage(n, false); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPoolExhausted(t *testing.T) {
	bp := newTestPool(t, 2)

	// Create three pages, unpinning each one so the pool starts out free.
	var nums []uint32
	for i := 0; i < 3; i++ {
		_, n, err := bp.NewPage()
		if err != nil {
			t.Fatalf("NewPage %d failed: %v", i, err)
		}
		if err := bp.UnpinPage(n, false); err != nil {
			t.Fatal(err)
		}
		nums = append(nums, n)
	}

	// Pin two pages and hold them, which fills both frames.
	for _, n := range nums[:2] {
		if _, err := bp.FetchPage(n); err != nil {
			t.Fatalf("FetchPage(%d) failed: %v", n, err)
		}
	}

	// Nothing can be evicted for the third, and waiting would not help: the
	// goroutine that would unpin is this one.
	if _, err := bp.FetchPage(nums[2]); !errors.Is(err, ErrPoolExhausted) {
		t.Errorf("FetchPage with every frame pinned = %v, want ErrPoolExhausted", err)
	}

	// Release one pin and the same fetch now succeeds.
	if err := bp.UnpinPage(nums[0], false); err != nil {
		t.Fatal(err)
	}
	if _, err := bp.FetchPage(nums[2]); err != nil {
		t.Errorf("FetchPage after freeing a pin failed: %v", err)
	}
	bp.UnpinPage(nums[1], false)
	bp.UnpinPage(nums[2], false)
}

func TestUnpinErrors(t *testing.T) {
	bp := newTestPool(t, 2)

	if err := bp.UnpinPage(0, false); !errors.Is(err, ErrNotPinned) {
		t.Errorf("unpinning a page that was never fetched = %v, want ErrNotPinned", err)
	}

	_, n, err := bp.NewPage()
	if err != nil {
		t.Fatal(err)
	}
	if err := bp.UnpinPage(n, false); err != nil {
		t.Fatalf("first unpin failed: %v", err)
	}
	if err := bp.UnpinPage(n, false); !errors.Is(err, ErrNotPinned) {
		t.Errorf("double unpin = %v, want ErrNotPinned", err)
	}
}

// Changes made through the pool must reach the disk, so a fresh pool over the
// same file sees them.
func TestChangesSurviveCloseAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reopen.db")

	func() {
		pg, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		bp := NewBufferPool(pg, 2)

		for i := 0; i < 4; i++ {
			page, n, err := bp.NewPage()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := page.Insert([]byte{byte('0' + i)}); err != nil {
				t.Fatal(err)
			}
			if err := bp.UnpinPage(n, true); err != nil {
				t.Fatal(err)
			}
		}

		if err := bp.Close(); err != nil {
			t.Fatalf("Close failed: %v", err)
		}
	}()

	pg, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	bp := NewBufferPool(pg, 2)
	defer bp.Close()

	if got, want := pg.NumPages(), uint32(4); got != want {
		t.Fatalf("NumPages() = %d after reopen, want %d", got, want)
	}

	for i := uint32(0); i < 4; i++ {
		page, err := bp.FetchPage(i)
		if err != nil {
			t.Fatalf("FetchPage(%d) failed: %v", i, err)
		}
		got, err := page.Read(0)
		if err != nil {
			t.Fatalf("page %d: %v", i, err)
		}
		if want := []byte{byte('0' + i)}; !bytes.Equal(got, want) {
			t.Errorf("page %d = %q, want %q", i, got, want)
		}
		bp.UnpinPage(i, false)
	}
}

// Validates the whole mutex decision. Meaningless without -race.
func TestConcurrentFetchUnpin(t *testing.T) {
	const nPages = 20
	bp := newTestPool(t, 4)

	for i := 0; i < nPages; i++ {
		_, n, err := bp.NewPage()
		if err != nil {
			t.Fatal(err)
		}
		bp.UnpinPage(n, false)
	}

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				n := uint32((g*7 + i) % nPages)
				page, err := bp.FetchPage(n)
				if err != nil {
					// A transient exhaustion is legitimate with 4 frames and 8
					// goroutines. Anything else is not.
					if errors.Is(err, ErrPoolExhausted) {
						continue
					}
					t.Errorf("FetchPage(%d): %v", n, err)
					return
				}
				_ = page.NumSlots() // touch the bytes so -race has something to see
				if err := bp.UnpinPage(n, false); err != nil {
					t.Errorf("UnpinPage(%d): %v", n, err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
}
