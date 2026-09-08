package shale

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// newTestPager opens a pager on a fresh file inside the test's temp directory,
// which Go deletes when the test finishes. The returned path can be reopened.
func newTestPager(t *testing.T) (*Pager, string) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "test.db")
	pg, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%q) failed: %v", path, err)
	}
	t.Cleanup(func() { pg.Close() })

	return pg, path
}

func TestOpenCreatesFile(t *testing.T) {
	pg, path := newTestPager(t)

	if got, want := pg.NumPages(), uint32(0); got != want {
		t.Errorf("NumPages() = %d, want %d on a new file", got, want)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("Open did not create the file: %v", err)
	}
}

func TestOpenRejectsBadSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "junk.db")
	if err := os.WriteFile(path, make([]byte, 100), 0666); err != nil {
		t.Fatal(err)
	}

	pg, err := Open(path)
	if err == nil {
		pg.Close()
		t.Fatal("Open accepted a file that is not a whole number of pages")
	}
	// errors.Is, not ==, because the error is wrapped with %w.
	if !errors.Is(err, ErrCorruptFile) {
		t.Errorf("Open error = %v, want it to wrap ErrCorruptFile", err)
	}
}

func TestAllocateReturnsSequentialNumbers(t *testing.T) {
	pg, _ := newTestPager(t)

	for want := uint32(0); want < 3; want++ {
		got, err := pg.Allocate()
		if err != nil {
			t.Fatalf("Allocate failed: %v", err)
		}
		if got != want {
			t.Errorf("Allocate() = %d, want %d", got, want)
		}
		if got, want := pg.NumPages(), want+1; got != want {
			t.Errorf("NumPages() = %d, want %d", got, want)
		}
	}
}

// The one that catches the zero-filled page trap. Growing the file with
// Truncate would leave freeEnd at 0, so FreeSpace would wrap to 65528 instead
// of reporting 4088.
func TestAllocateWritesValidPage(t *testing.T) {
	pg, _ := newTestPager(t)

	n, err := pg.Allocate()
	if err != nil {
		t.Fatal(err)
	}

	page, err := pg.ReadPage(n)
	if err != nil {
		t.Fatalf("ReadPage(%d) failed: %v", n, err)
	}
	mustCheck(t, page)

	if got, want := page.FreeSpace(), uint16(PageSize-headerSize); got != want {
		t.Errorf("FreeSpace() = %d, want %d (an allocated page must be initialised)", got, want)
	}
	if got, want := page.NumSlots(), uint16(0); got != want {
		t.Errorf("NumSlots() = %d, want %d", got, want)
	}
}

func TestReadPageOutOfRange(t *testing.T) {
	pg, _ := newTestPager(t)

	if _, err := pg.ReadPage(0); !errors.Is(err, ErrInvalidPage) {
		t.Errorf("ReadPage(0) on an empty file = %v, want ErrInvalidPage", err)
	}

	if _, err := pg.Allocate(); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.ReadPage(1); !errors.Is(err, ErrInvalidPage) {
		t.Errorf("ReadPage(1) with 1 page = %v, want ErrInvalidPage", err)
	}
}

func TestWritePageOutOfRange(t *testing.T) {
	pg, _ := newTestPager(t)

	if err := pg.WritePage(0, NewPage()); !errors.Is(err, ErrInvalidPage) {
		t.Errorf("WritePage(0) on an empty file = %v, want ErrInvalidPage", err)
	}
}

// The milestone: records survive the process that wrote them. This is the line
// between a data structure and a database.
func TestRoundTripAcrossClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "round.db")

	// Two records per page, distinguishable so a mixed-up offset is obvious.
	want := map[uint32][][]byte{
		0: {[]byte("page zero, record one"), []byte("page zero, record two")},
		1: {[]byte("page one, record one"), []byte("page one, record two")},
		2: {[]byte("page two, record one"), bytes.Repeat([]byte("z"), 1000)},
	}

	// Write everything, then close.
	func() {
		pg, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer pg.Close()

		for n := uint32(0); n < 3; n++ {
			got, err := pg.Allocate()
			if err != nil {
				t.Fatal(err)
			}
			if got != n {
				t.Fatalf("Allocate() = %d, want %d", got, n)
			}

			page, err := pg.ReadPage(n)
			if err != nil {
				t.Fatal(err)
			}
			for _, rec := range want[n] {
				if _, err := page.Insert(rec); err != nil {
					t.Fatalf("Insert into page %d failed: %v", n, err)
				}
			}
			if err := pg.WritePage(n, page); err != nil {
				t.Fatalf("WritePage(%d) failed: %v", n, err)
			}
		}

		if err := pg.Sync(); err != nil {
			t.Fatalf("Sync failed: %v", err)
		}
	}()

	// Reopen the same file and read everything back.
	pg, err := Open(path)
	if err != nil {
		t.Fatalf("reopening %q failed: %v", path, err)
	}
	defer pg.Close()

	if got, want := pg.NumPages(), uint32(3); got != want {
		t.Fatalf("NumPages() = %d after reopen, want %d", got, want)
	}

	for n := uint32(0); n < 3; n++ {
		page, err := pg.ReadPage(n)
		if err != nil {
			t.Fatalf("ReadPage(%d) after reopen failed: %v", n, err)
		}
		mustCheck(t, page)

		for slot, rec := range want[n] {
			got, err := page.Read(uint16(slot))
			if err != nil {
				t.Fatalf("page %d slot %d: %v", n, slot, err)
			}
			if !bytes.Equal(got, rec) {
				t.Errorf("page %d slot %d = %q, want %q", n, slot, got, rec)
			}
		}
	}
}

// Pins the documented behaviour: ReadPage has no cache, so two reads of the
// same page are independent copies. When the buffer pool lands this test should
// start failing, which is exactly the signal that the pool is doing its job.
func TestReadPageHasNoCache(t *testing.T) {
	pg, _ := newTestPager(t)

	n, err := pg.Allocate()
	if err != nil {
		t.Fatal(err)
	}

	first, err := pg.ReadPage(n)
	if err != nil {
		t.Fatal(err)
	}
	second, err := pg.ReadPage(n)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := first.Insert([]byte("only in the first copy")); err != nil {
		t.Fatal(err)
	}

	if got := second.NumSlots(); got != 0 {
		t.Errorf("second copy has %d slots, want 0: the two reads are not independent", got)
	}
	if _, err := second.Read(0); !errors.Is(err, ErrNoSuchSlot) {
		t.Errorf("second copy sees the first copy's insert: %v", err)
	}
}
