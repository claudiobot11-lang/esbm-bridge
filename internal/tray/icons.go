// Tray icons — three color variants drawn at runtime as 16×16 ICO
// files. Windows' Shell_NotifyIconW only accepts .ico (the docs say
// "Win32 icon"), and fyne.io/systray's SetIcon on Windows hands the
// bytes straight to that API. PNGs returned `unable to set icon: The
// operation completed successfully.` even though the error number
// is 0 — Win32's polite way of saying "wrong format". We wrap the
// PNG payload in a minimal ICONDIR header so the same data works on
// both macOS/Linux (raw PNG) and Windows (ICO with embedded PNG).

package tray

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
)

var (
	iconGreen  = makeICO(color.NRGBA{R: 22, G: 163, B: 74, A: 255})
	iconYellow = makeICO(color.NRGBA{R: 202, G: 138, B: 4, A: 255})
	iconRed    = makeICO(color.NRGBA{R: 220, G: 38, B: 38, A: 255})
)

// renderPNG draws a 16×16 filled disc on transparent background and
// returns the PNG bytes. Good enough for a status indicator; anyone
// who wants a real logo can drop a .ico in later.
func renderPNG(fill color.NRGBA) []byte {
	const w, h = 16, 16
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	cx, cy := float64(w)/2, float64(h)/2
	r := 7.0
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			dx, dy := float64(x)+0.5-cx, float64(y)+0.5-cy
			if dx*dx+dy*dy <= r*r {
				img.SetNRGBA(x, y, fill)
			}
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

// makeICO wraps the rendered PNG in a single-image ICO container.
// Layout (from the ICO spec):
//
//	ICONDIR        6 bytes
//	ICONDIRENTRY  16 bytes  (one per image; we have exactly one)
//	image data    variable  (PNG bytes — Vista+ accepts this directly)
//
// All multi-byte fields are little-endian.
func makeICO(fill color.NRGBA) []byte {
	payload := renderPNG(fill)
	const width, height = 16, 16
	var buf bytes.Buffer
	// ICONDIR
	_ = binary.Write(&buf, binary.LittleEndian, uint16(0)) // Reserved
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1)) // Type 1 = icon
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1)) // Image count
	// ICONDIRENTRY
	_ = binary.Write(&buf, binary.LittleEndian, uint8(width))  // 0 = 256
	_ = binary.Write(&buf, binary.LittleEndian, uint8(height)) // 0 = 256
	_ = binary.Write(&buf, binary.LittleEndian, uint8(0))      // Color palette (0 for non-paletted)
	_ = binary.Write(&buf, binary.LittleEndian, uint8(0))      // Reserved
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))     // Color planes
	_ = binary.Write(&buf, binary.LittleEndian, uint16(32))    // Bits per pixel
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(payload)))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(22)) // Offset of image data (6 + 16)
	buf.Write(payload)
	return buf.Bytes()
}
