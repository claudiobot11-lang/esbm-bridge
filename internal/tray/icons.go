// Tray icons — 16×16 PNG bytes, three color variants for the three
// states the icon represents. Drawn programmatically so we don't
// ship a separate assets directory.
//
// Build-time tag is empty here because the icons are platform-neutral
// PNGs and only the *_windows.go file actually wires them up.

package tray

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
)

var (
	iconGreen  = makePNG(color.NRGBA{R: 22, G: 163, B: 74, A: 255})
	iconYellow = makePNG(color.NRGBA{R: 202, G: 138, B: 4, A: 255})
	iconRed    = makePNG(color.NRGBA{R: 220, G: 38, B: 38, A: 255})
)

// makePNG renders a 16×16 filled disc on a transparent background.
// Good enough for a status indicator in the Windows notification area;
// anyone who wants a real logo can drop a .ico in later.
func makePNG(fill color.NRGBA) []byte {
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
