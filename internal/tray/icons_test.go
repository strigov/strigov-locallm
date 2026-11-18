package tray

import (
	"bytes"
	"encoding/binary"
	"image/png"
	"testing"
)

// §11.1: значок — ICO с PNG внутри (Vista+): ICONDIR, по ICONDIRENTRY на размер, затем данные PNG.
func TestICO(t *testing.T) {
	ico := ICO(Green)
	le := binary.LittleEndian
	if le.Uint16(ico[0:]) != 0 || le.Uint16(ico[2:]) != 1 || int(le.Uint16(ico[4:])) != len(icoSizes) {
		t.Fatalf("ICONDIR % x", ico[:6])
	}
	if len(icoSizes) != 5 || icoSizes[0] != 16 || icoSizes[4] != 48 {
		t.Fatalf("sizes %v", icoSizes)
	}
	cr, cg, cb, _ := Green.rgba().RGBA()
	for i, want := range icoSizes {
		e := ico[6+16*i:]
		size, off := le.Uint32(e[8:]), le.Uint32(e[12:])
		if int(e[0]) != want || int(e[1]) != want || le.Uint16(e[4:]) != 1 || le.Uint16(e[6:]) != 32 {
			t.Fatalf("entry %d: % x", i, e[:16])
		}
		if int(off)+int(size) > len(ico) {
			t.Fatalf("entry %d: data %d+%d outside %d bytes", i, off, size, len(ico))
		}
		img, err := png.Decode(bytes.NewReader(ico[off : off+size]))
		if err != nil {
			t.Fatalf("entry %d: %v", i, err)
		}
		if b := img.Bounds(); b.Dx() != want || b.Dy() != want {
			t.Fatalf("entry %d: bounds %v", i, b)
		}
		if r, g, b, a := img.At(want*4/5, want*4/5).RGBA(); a != 0xffff || r != cr || g != cg || b != cb {
			t.Fatalf("entry %d: status indicator %v %v %v %v", i, r, g, b, a)
		}
		if _, _, _, a := img.At(0, 0).RGBA(); a != 0 {
			t.Fatalf("entry %d: corner must be transparent, alpha %v", i, a)
		}
	}
	seen := map[string]bool{}
	for c := Gray; c <= Blue; c++ {
		seen[string(ICO(c))] = true
	}
	if len(seen) != 4 {
		t.Fatal("icons of different colours must differ")
	}
}
