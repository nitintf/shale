package shale

import "testing"

// A fresh page must describe itself correctly. Nothing else works if this
// doesn't, because every other method reads these three fields.
func TestNewPageHeader(t *testing.T) {
	p := NewPage()

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
		t.Errorf("FreeSpace() = %d, want %d", got, want)
	}
}

// If two header offsets overlap, writing one field silently corrupts another.
// Distinctive values make that obvious instead of subtle.
func TestHeaderFieldsAreIndependent(t *testing.T) {
	p := NewPage()

	p.setNumSlots(0x1111)
	p.setFreeStart(0x2222)
	p.setFreeEnd(0x3333)

	if got := p.NumSlots(); got != 0x1111 {
		t.Errorf("NumSlots() = %#04x, want 0x1111 (another field overwrote it)", got)
	}
	if got := p.freeStart(); got != 0x2222 {
		t.Errorf("freeStart() = %#04x, want 0x2222 (another field overwrote it)", got)
	}
	if got := p.freeEnd(); got != 0x3333 {
		t.Errorf("freeEnd() = %#04x, want 0x3333 (another field overwrote it)", got)
	}
}

// The on-disk byte order is part of the file format. If this test ever fails,
// every database file written by the old code has become unreadable.
func TestHeaderIsLittleEndian(t *testing.T) {
	p := NewPage()
	p.setNumSlots(300) // 0x012C

	if got, want := p.data[offNumSlots], byte(0x2C); got != want {
		t.Errorf("byte %d = %#02x, want %#02x (low byte comes first in little-endian)",
			offNumSlots, got, want)
	}
	if got, want := p.data[offNumSlots+1], byte(0x01); got != want {
		t.Errorf("byte %d = %#02x, want %#02x (high byte comes second)",
			offNumSlots+1, got, want)
	}
}

// The serialization primitive everything else is built on. offFlags is the
// reserved field, so writing to it disturbs nothing.
func TestU16RoundTrip(t *testing.T) {
	p := NewPage()

	for _, v := range []uint16{0, 1, 255, 256, 300, 4095, 4096, 65534, 65535} {
		p.setU16(offFlags, v)
		if got := p.u16(offFlags); got != v {
			t.Errorf("wrote %d, read back %d", v, got)
		}
	}
}

func TestFreeSpaceTracksHeader(t *testing.T) {
	p := NewPage()

	p.setFreeStart(100)
	p.setFreeEnd(1000)

	if got, want := p.FreeSpace(), uint16(900); got != want {
		t.Errorf("FreeSpace() = %d, want %d", got, want)
	}
}
