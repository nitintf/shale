package shale

import (
	"errors"
	"fmt"
	"os"
)

// Pager owns the database file and translates page numbers into byte offsets.
// It is the only part of the engine that talks to the filesystem.
//
// A database file is just a sequence of fixed-size pages with no header of its
// own, so page N always begins at byte N*PageSize and locating a page needs no
// lookup structure at all.
type Pager struct {
	f      *os.File
	nPages uint32
}

var (
	ErrInvalidPage = errors.New("pager: invalid page")
	ErrCorruptFile = errors.New("pager: corrupted file")
)

// Open opens the database file at path, creating it if it does not exist.
//
// The file size must be a whole number of pages. Anything else means the file is
// truncated or is not a shale database at all, and refusing here is far better
// than discovering it halfway through a read.
func Open(path string) (*Pager, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0666)
	if err != nil {
		return nil, err
	}

	fInfo, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}

	fSize := fInfo.Size()
	if fSize%PageSize != 0 {
		f.Close()
		return nil, fmt.Errorf("%w: %d bytes is not a whole number of %d-byte pages",
			ErrCorruptFile, fSize, PageSize)
	}

	return &Pager{f: f, nPages: uint32(fSize / PageSize)}, nil
}

// Close closes the underlying file.
//
// It flushes nothing. A page written but not synced may still be sitting in the
// OS page cache, so call Sync first when that matters.
func (p *Pager) Close() error {
	return p.f.Close()
}

// readInto reads page n into buf, which must be exactly one page long.
//
// This is the primitive ReadPage wraps. The buffer pool calls it to read
// straight into a frame it already owns, so the bytes move from disk to their
// final home once, with no intermediate buffer.
func (p *Pager) readInto(n uint32, buf []byte) error {
	if n >= p.nPages {
		return fmt.Errorf("%w: page %d of %d", ErrInvalidPage, n, p.nPages)
	}
	if len(buf) != PageSize {
		panic("shale: readInto buffer is not exactly one page")
	}

	if _, err := p.f.ReadAt(buf, pageOffset(n)); err != nil {
		return fmt.Errorf("read page %d: %w", n, err)
	}

	return nil
}

// ReadPage reads page n off disk into a freshly allocated buffer.
//
// The bytes are already in page format, so nothing is parsed or converted.
//
// There is no cache: reading the same page twice gives two independent copies,
// and writing both back loses one set of changes. Go through a BufferPool when
// that matters.
func (p *Pager) ReadPage(n uint32) (*Page, error) {
	buf := make([]byte, PageSize)
	if err := p.readInto(n, buf); err != nil {
		return nil, err
	}
	return PageFrom(buf), nil
}

// WritePage writes page n back to the file.
//
// Returning nil does not mean the data is on disk. The bytes sit in the OS page
// cache until the kernel flushes them, so a crash can still lose them. Sync is
// what makes a write durable.
func (p *Pager) WritePage(n uint32, page *Page) error {
	if n >= p.nPages {
		return fmt.Errorf("%w: page %d of %d", ErrInvalidPage, n, p.nPages)
	}

	if _, err := p.f.WriteAt(page.data, pageOffset(n)); err != nil {
		return fmt.Errorf("write page %d: %w", n, err)
	}

	return nil
}

// Allocate grows the file by one page and returns its number.
//
// It writes an initialised page rather than just extending the file. A
// zero-filled page is not valid here: freeEnd would be 0, so FreeSpace would
// wrap and the first insert would run off the end of the page.
func (p *Pager) Allocate() (uint32, error) {
	n := p.nPages

	if _, err := p.f.WriteAt(NewPage().data, pageOffset(n)); err != nil {
		return 0, fmt.Errorf("allocate page %d: %w", n, err)
	}

	p.nPages++
	return n, nil
}

// Sync flushes all writes to disk.
func (p *Pager) Sync() error {
	return p.f.Sync()
}

// NumPages returns the number of pages in the file.
func (p *Pager) NumPages() uint32 {
	return p.nPages
}

// pageOffset returns the byte offset where page n begins.
//
// The int64 is the whole point. A uint32 multiplication overflows at 4GB, and a
// 4GB database is not exotic. n is widened before the multiply, never after:
// int64(n * PageSize) would overflow first and then widen the wrong answer.
func pageOffset(n uint32) int64 {
	return int64(n) * PageSize
}
