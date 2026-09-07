package shale

import (
	"bytes"
	"testing"
)

// The precise assertion: compaction reclaims exactly the bytes the deleted
// records occupied, no more and no less. Only possible to check because Delete
// keeps the length.
func TestCompactReclaimsExactSpace(t *testing.T) {
	p := NewPage()

	records := [][]byte{
		bytes.Repeat([]byte("a"), 100),
		bytes.Repeat([]byte("b"), 250),
		bytes.Repeat([]byte("c"), 40),
		bytes.Repeat([]byte("d"), 700),
	}

	slots := make([]uint16, len(records))
	for i, rec := range records {
		s, err := p.Insert(rec)
		if err != nil {
			t.Fatal(err)
		}
		slots[i] = s
	}

	// Delete the 250-byte and 700-byte records.
	deleted := 0
	for _, i := range []int{1, 3} {
		if err := p.Delete(slots[i]); err != nil {
			t.Fatal(err)
		}
		deleted += len(records[i])
	}

	before := p.FreeSpace()
	p.Compact()
	mustCheck(t, p)

	if got, want := p.FreeSpace(), before+uint16(deleted); got != want {
		t.Errorf("FreeSpace() = %d after Compact, want %d (reclaimed %d, expected %d)",
			got, want, got-before, deleted)
	}
}

// Records move, slot indices do not. This is the whole reason the slot
// indirection exists, so it gets its own test.
func TestCompactPreservesSlotIndices(t *testing.T) {
	p := NewPage()

	keepA, _ := p.Insert([]byte("first survivor"))
	dead, _ := p.Insert(bytes.Repeat([]byte("x"), 500))
	keepB, _ := p.Insert([]byte("second survivor"))

	offsetBefore, _ := p.readSlot(keepB)

	if err := p.Delete(dead); err != nil {
		t.Fatal(err)
	}
	p.Compact()
	mustCheck(t, p)

	// Same slot index, same bytes.
	for _, tc := range []struct {
		slot uint16
		want string
	}{
		{keepA, "first survivor"},
		{keepB, "second survivor"},
	} {
		got, err := p.Read(tc.slot)
		if err != nil {
			t.Fatalf("Read(%d) after Compact failed: %v", tc.slot, err)
		}
		if !bytes.Equal(got, []byte(tc.want)) {
			t.Errorf("Read(%d) = %q, want %q", tc.slot, got, tc.want)
		}
	}

	// The record really did move, otherwise this test proves nothing.
	if offsetAfter, _ := p.readSlot(keepB); offsetAfter == offsetBefore {
		t.Errorf("slot %d offset unchanged at %d, expected Compact to move the record",
			keepB, offsetBefore)
	}
}

func TestCompactKeepsTombstones(t *testing.T) {
	p := NewPage()

	a, _ := p.Insert([]byte("live"))
	dead, _ := p.Insert([]byte("dead"))
	slotsBefore := p.NumSlots()
	startBefore := p.freeStart()

	if err := p.Delete(dead); err != nil {
		t.Fatal(err)
	}
	p.Compact()
	mustCheck(t, p)

	if _, err := p.Read(dead); err != ErrDeleted {
		t.Errorf("Read of a compacted-away slot = %v, want ErrDeleted", err)
	}
	if got := p.NumSlots(); got != slotsBefore {
		t.Errorf("NumSlots() = %d after Compact, want %d unchanged", got, slotsBefore)
	}
	if got := p.freeStart(); got != startBefore {
		t.Errorf("freeStart() = %d after Compact, want %d unchanged (slot array must not move)",
			got, startBefore)
	}
	if _, err := p.Read(a); err != nil {
		t.Errorf("Read(%d) failed after Compact: %v", a, err)
	}
}

func TestCompactWithNothingDeletedChangesNothing(t *testing.T) {
	p := NewPage()
	p.Insert([]byte("one"))
	p.Insert([]byte("two"))
	p.Insert([]byte("three"))

	before := p.FreeSpace()
	p.Compact()
	mustCheck(t, p)

	if got := p.FreeSpace(); got != before {
		t.Errorf("FreeSpace() = %d, want %d unchanged when nothing was deleted", got, before)
	}
	for slot, want := range map[uint16]string{0: "one", 1: "two", 2: "three"} {
		got, err := p.Read(slot)
		if err != nil {
			t.Fatalf("Read(%d) failed: %v", slot, err)
		}
		if !bytes.Equal(got, []byte(want)) {
			t.Errorf("Read(%d) = %q, want %q", slot, got, want)
		}
	}
}

func TestCompactEmptyPage(t *testing.T) {
	p := NewPage()
	p.Compact()
	mustCheck(t, p)

	if got, want := p.freeEnd(), uint16(PageSize); got != want {
		t.Errorf("freeEnd() = %d, want %d", got, want)
	}
}

func TestCompactEverythingDeleted(t *testing.T) {
	p := NewPage()

	var slots []uint16
	for i := 0; i < 5; i++ {
		s, err := p.Insert(bytes.Repeat([]byte("z"), 100))
		if err != nil {
			t.Fatal(err)
		}
		slots = append(slots, s)
	}
	for _, s := range slots {
		if err := p.Delete(s); err != nil {
			t.Fatal(err)
		}
	}

	p.Compact()
	mustCheck(t, p)

	// Every record byte is reclaimed. The slot array stays, because index
	// entries elsewhere may still point at these slot numbers.
	if got, want := p.freeEnd(), uint16(PageSize); got != want {
		t.Errorf("freeEnd() = %d, want %d (all records were dead)", got, want)
	}
	if got, want := p.NumSlots(), uint16(len(slots)); got != want {
		t.Errorf("NumSlots() = %d, want %d (tombstones must survive)", got, want)
	}
}

// Insert recovers fragmented space by itself: it compacts and retries before
// reporting the page full.
func TestInsertCompactsWhenFragmented(t *testing.T) {
	p := NewPage()
	rec := make([]byte, 512)

	var slots []uint16
	for {
		s, err := p.Insert(rec)
		if err == ErrPageFull {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		slots = append(slots, s)
	}
	if len(slots) < 2 {
		t.Fatalf("only fit %d records, need at least 2 for this test", len(slots))
	}

	// Free up one record's worth of space, but leave it fragmented: the
	// contiguous free region is still too small for another 512-byte record.
	if err := p.Delete(slots[0]); err != nil {
		t.Fatal(err)
	}
	if got := p.FreeSpace(); got >= uint16(len(rec)+slotSize) {
		t.Fatalf("contiguous free space is %d, too large for this test to prove anything", got)
	}

	// Insert should compact internally and succeed anyway.
	reused, err := p.Insert(rec)
	if err != nil {
		t.Fatalf("Insert failed despite enough fragmented space: %v", err)
	}
	mustCheck(t, p)

	if got, err := p.Read(reused); err != nil {
		t.Errorf("Read(%d) failed: %v", reused, err)
	} else if len(got) != len(rec) {
		t.Errorf("Read(%d) returned %d bytes, want %d", reused, len(got), len(rec))
	}

	// The deleted slot keeps its tombstone, it is not recycled.
	if _, err := p.Read(slots[0]); err != ErrDeleted {
		t.Errorf("deleted slot = %v, want ErrDeleted (slots are never reused)", err)
	}
}

func TestReset(t *testing.T) {
	p := NewPage()

	a, _ := p.Insert([]byte("one"))
	p.Insert([]byte("two"))
	if err := p.Delete(a); err != nil {
		t.Fatal(err)
	}

	p.Reset()
	mustCheck(t, p)

	if got, want := p.NumSlots(), uint16(0); got != want {
		t.Errorf("NumSlots() = %d, want %d", got, want)
	}
	if got, want := p.freeStart(), uint16(headerSize); got != want {
		t.Errorf("freeStart() = %d, want %d", got, want)
	}
	if got, want := p.freeEnd(), uint16(PageSize); got != want {
		t.Errorf("freeEnd() = %d, want %d", got, want)
	}
	if got, want := p.FreeSpace(), uint16(PageSize-headerSize); got != want {
		t.Errorf("FreeSpace() = %d, want %d (tombstones should be gone too)", got, want)
	}

	// Every old slot is gone, including the tombstone.
	if _, err := p.Read(0); err != ErrNoSuchSlot {
		t.Errorf("Read(0) after Reset = %v, want ErrNoSuchSlot", err)
	}
}

func TestResetThenReuse(t *testing.T) {
	p := NewPage()
	p.Insert(bytes.Repeat([]byte("x"), 3000))
	p.Reset()

	slot, err := p.Insert([]byte("fresh start"))
	if err != nil {
		t.Fatalf("Insert after Reset failed: %v", err)
	}
	if slot != 0 {
		t.Errorf("first Insert after Reset returned slot %d, want 0", slot)
	}

	got, err := p.Read(slot)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte("fresh start")) {
		t.Errorf("Read(%d) = %q, want %q", slot, got, "fresh start")
	}
	mustCheck(t, p)
}
