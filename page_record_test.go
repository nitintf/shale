package shale

import (
	"bytes"
	"testing"
)

// One insert into a fresh page, with every number checked by hand:
// a 5-byte record costs 5 bytes of record area plus a 4-byte slot.
func TestInsertUpdatesHeader(t *testing.T) {
	p := NewPage()

	slot, err := p.Insert([]byte("hello"))
	if err != nil {
		t.Fatalf("Insert failed: %v", err)
	}

	if slot != 0 {
		t.Errorf("first Insert returned slot %d, want 0", slot)
	}
	if got, want := p.NumSlots(), uint16(1); got != want {
		t.Errorf("NumSlots() = %d, want %d", got, want)
	}
	if got, want := p.freeStart(), uint16(headerSize+slotSize); got != want {
		t.Errorf("freeStart() = %d, want %d", got, want)
	}
	if got, want := p.freeEnd(), uint16(PageSize-5); got != want {
		t.Errorf("freeEnd() = %d, want %d", got, want)
	}
	if got, want := p.FreeSpace(), uint16(PageSize-headerSize-5-slotSize); got != want {
		t.Errorf("FreeSpace() = %d, want %d", got, want)
	}
}

// The test worth keeping forever. Varying lengths are the point: equal-sized
// records hide offset arithmetic bugs.
func TestRoundTrip(t *testing.T) {
	p := NewPage()

	records := [][]byte{
		[]byte("first"),
		[]byte("second record, a good deal longer than the first one"),
		[]byte(""), // a legal empty record, distinct from a deleted one
		[]byte("d"),
		bytes.Repeat([]byte("x"), 300),
	}

	slots := make([]uint16, len(records))
	for i, rec := range records {
		s, err := p.Insert(rec)
		if err != nil {
			t.Fatalf("Insert(%d bytes) failed: %v", len(rec), err)
		}
		slots[i] = s
	}

	for i, rec := range records {
		got, err := p.Read(slots[i])
		if err != nil {
			t.Fatalf("Read(%d) failed: %v", slots[i], err)
		}
		if !bytes.Equal(got, rec) {
			t.Errorf("Read(%d) = %q (%d bytes), want %q (%d bytes)",
				slots[i], got, len(got), rec, len(rec))
		}
	}
}

// Slots grow forward while records grow backward, so the two orders are
// mirrored inside the page. Slot 0 must still be the first record inserted.
func TestSlotOrderIsInsertionOrder(t *testing.T) {
	p := NewPage()

	first, _ := p.Insert([]byte("first"))
	second, _ := p.Insert([]byte("second"))

	if first != 0 || second != 1 {
		t.Fatalf("got slots %d and %d, want 0 and 1", first, second)
	}

	got, err := p.Read(0)
	if err != nil {
		t.Fatalf("Read(0) failed: %v", err)
	}
	if !bytes.Equal(got, []byte("first")) {
		t.Errorf("Read(0) = %q, want %q", got, "first")
	}
}

// Proves the size check in Insert is right. Without it this would write past
// the end of the page instead of returning an error.
func TestInsertUntilFull(t *testing.T) {
	p := NewPage()
	rec := make([]byte, 512)

	inserted := 0
	for {
		_, err := p.Insert(rec)
		if err == ErrPageFull {
			break
		}
		if err != nil {
			t.Fatalf("Insert returned %v, want nil or ErrPageFull", err)
		}
		inserted++
		if inserted > 100 {
			t.Fatal("page never returned ErrPageFull")
		}
	}

	if inserted == 0 {
		t.Fatal("page rejected the very first record")
	}
	if got := p.FreeSpace(); got >= 512+slotSize {
		t.Errorf("page reported full with %d bytes free, enough for another record", got)
	}
	t.Logf("fit %d records of 512 bytes", inserted)
}

// A full page must be left completely untouched by the insert that failed.
func TestFailedInsertChangesNothing(t *testing.T) {
	p := NewPage()

	tooBig := make([]byte, PageSize)
	before := struct{ slots, start, end uint16 }{p.NumSlots(), p.freeStart(), p.freeEnd()}

	if _, err := p.Insert(tooBig); err != ErrPageFull {
		t.Fatalf("Insert of a page-sized record = %v, want ErrPageFull", err)
	}

	if p.NumSlots() != before.slots || p.freeStart() != before.start || p.freeEnd() != before.end {
		t.Errorf("header changed after a failed Insert: %d/%d/%d, want %d/%d/%d",
			p.NumSlots(), p.freeStart(), p.freeEnd(), before.slots, before.start, before.end)
	}
}

func TestReadOutOfRange(t *testing.T) {
	p := NewPage()

	if _, err := p.Read(0); err != ErrNoSuchSlot {
		t.Errorf("Read(0) on an empty page = %v, want ErrNoSuchSlot", err)
	}

	p.Insert([]byte("a"))
	p.Insert([]byte("b"))

	// Just past the end is where off-by-one lives.
	if _, err := p.Read(2); err != ErrNoSuchSlot {
		t.Errorf("Read(2) with 2 slots = %v, want ErrNoSuchSlot", err)
	}
	if _, err := p.Read(9999); err != ErrNoSuchSlot {
		t.Errorf("Read(9999) = %v, want ErrNoSuchSlot", err)
	}
}

func TestDeleteThenRead(t *testing.T) {
	p := NewPage()

	slot, err := p.Insert([]byte("doomed"))
	if err != nil {
		t.Fatal(err)
	}

	if err := p.Delete(slot); err != nil {
		t.Fatalf("Delete(%d) failed: %v", slot, err)
	}
	if _, err := p.Read(slot); err != ErrDeleted {
		t.Errorf("Read of a deleted slot = %v, want ErrDeleted", err)
	}
}

func TestDeleteTwice(t *testing.T) {
	p := NewPage()
	slot, _ := p.Insert([]byte("doomed"))

	if err := p.Delete(slot); err != nil {
		t.Fatalf("first Delete failed: %v", err)
	}
	if err := p.Delete(slot); err != ErrDeleted {
		t.Errorf("second Delete = %v, want ErrDeleted", err)
	}
}

func TestDeleteOutOfRange(t *testing.T) {
	p := NewPage()

	if err := p.Delete(0); err != ErrNoSuchSlot {
		t.Errorf("Delete(0) on an empty page = %v, want ErrNoSuchSlot", err)
	}
}

// Documented behaviour, not a bug: only Compact reclaims space. The slot stays
// as a tombstone so its index never shifts.
func TestDeleteDoesNotReclaimSpace(t *testing.T) {
	p := NewPage()
	slot, _ := p.Insert([]byte("some record"))

	before := p.FreeSpace()
	slotsBefore := p.NumSlots()

	if err := p.Delete(slot); err != nil {
		t.Fatal(err)
	}

	if got := p.FreeSpace(); got != before {
		t.Errorf("FreeSpace() = %d after Delete, want %d unchanged", got, before)
	}
	if got := p.NumSlots(); got != slotsBefore {
		t.Errorf("NumSlots() = %d after Delete, want %d unchanged", got, slotsBefore)
	}
}

// Delete zeroes the offset but keeps the length, so the size of the hole is
// still recorded for a future Compact.
func TestDeleteKeepsLength(t *testing.T) {
	p := NewPage()
	rec := []byte("eleven byte")
	slot, _ := p.Insert(rec)

	if err := p.Delete(slot); err != nil {
		t.Fatal(err)
	}

	offset, length := p.readSlot(slot)
	if offset != 0 {
		t.Errorf("offset = %d after Delete, want 0 (the dead marker)", offset)
	}
	if got, want := length, uint16(len(rec)); got != want {
		t.Errorf("length = %d after Delete, want %d preserved", got, want)
	}
}

func TestDeleteLeavesOtherRecords(t *testing.T) {
	p := NewPage()

	a, _ := p.Insert([]byte("keep me"))
	b, _ := p.Insert([]byte("drop me"))
	c, _ := p.Insert([]byte("keep me too"))

	if err := p.Delete(b); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		slot uint16
		want string
	}{
		{a, "keep me"},
		{c, "keep me too"},
	} {
		got, err := p.Read(tc.slot)
		if err != nil {
			t.Fatalf("Read(%d) failed after deleting a neighbour: %v", tc.slot, err)
		}
		if !bytes.Equal(got, []byte(tc.want)) {
			t.Errorf("Read(%d) = %q, want %q", tc.slot, got, tc.want)
		}
	}
}

// Fill the page with records of many different sizes, then read every single
// one back. This is the test most likely to catch an offset bug.
func TestRoundTripManySizes(t *testing.T) {
	p := NewPage()
	want := make(map[uint16][]byte)

	for i := 0; ; i++ {
		rec := make([]byte, 1+i%60)
		for j := range rec {
			rec[j] = byte(i + j)
		}

		slot, err := p.Insert(rec)
		if err == ErrPageFull {
			break
		}
		if err != nil {
			t.Fatalf("Insert failed: %v", err)
		}
		want[slot] = rec
	}

	if len(want) < 10 {
		t.Fatalf("only fit %d records, expected the page to hold many more", len(want))
	}

	for slot, rec := range want {
		got, err := p.Read(slot)
		if err != nil {
			t.Fatalf("Read(%d) failed: %v", slot, err)
		}
		if !bytes.Equal(got, rec) {
			t.Fatalf("slot %d = %q, want %q", slot, got, rec)
		}
	}
	t.Logf("round-tripped %d records", len(want))
}

// mustCheck asserts the page invariants hold. Call it after every mutation.
func mustCheck(t *testing.T, p *Page) {
	t.Helper()
	if err := p.check(); err != nil {
		t.Fatalf("page invariant violated: %v", err)
	}
}

func TestInvariantsHoldThroughout(t *testing.T) {
	p := NewPage()
	mustCheck(t, p)

	var slots []uint16
	for i := 0; i < 20; i++ {
		rec := make([]byte, 1+i%40)
		slot, err := p.Insert(rec)
		if err != nil {
			t.Fatalf("Insert failed: %v", err)
		}
		slots = append(slots, slot)
		mustCheck(t, p)
	}

	// Delete every third record.
	for i := 0; i < len(slots); i += 3 {
		if err := p.Delete(slots[i]); err != nil {
			t.Fatalf("Delete(%d) failed: %v", slots[i], err)
		}
		mustCheck(t, p)
	}
}
