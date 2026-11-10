//go:build ignore

// Generates the tray and app icons: go run assets/gen.go (from desktop/).
package main

import (
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
)

// inCat reports whether the unit-square point (x, y) is inside a cat's head:
// a circle with two triangular ears.
func inCat(x, y float64) bool {
	if math.Hypot(x-0.5, y-0.58) < 0.34 {
		return true
	}
	ear := func(ax, ay, bx, by, cx, cy float64) bool {
		s := func(x1, y1, x2, y2 float64) float64 { return (x-x2)*(y1-y2) - (x1-x2)*(y-y2) }
		d1, d2, d3 := s(ax, ay, bx, by), s(bx, by, cx, cy), s(cx, cy, ax, ay)
		return !((d1 < 0 || d2 < 0 || d3 < 0) && (d1 > 0 || d2 > 0 || d3 > 0))
	}
	return ear(0.17, 0.12, 0.20, 0.52, 0.48, 0.30) || ear(0.83, 0.12, 0.80, 0.52, 0.52, 0.30)
}

func inEye(x, y float64) bool {
	return math.Hypot(x-0.38, y-0.56) < 0.055 || math.Hypot(x-0.62, y-0.56) < 0.055
}

func write(path string, size int, draw func(x, y float64) color.NRGBA) {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	const ss = 4 // supersampling for smooth edges
	for py := 0; py < size; py++ {
		for px := 0; px < size; px++ {
			var r, g, b, a float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					c := draw((float64(px)+(float64(sx)+0.5)/ss)/float64(size), (float64(py)+(float64(sy)+0.5)/ss)/float64(size))
					al := float64(c.A) / 255
					r, g, b, a = r+float64(c.R)*al, g+float64(c.G)*al, b+float64(c.B)*al, a+al
				}
			}
			if a > 0 {
				img.SetNRGBA(px, py, color.NRGBA{uint8(r / a), uint8(g / a), uint8(b / a), uint8(a / (ss * ss) * 255)})
			}
		}
	}
	f, err := os.Create(path)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		panic(err)
	}
}

func main() {
	// Template icon for the macOS menu bar: black shape, transparent eyes.
	write("assets/tray-template.png", 44, func(x, y float64) color.NRGBA {
		if inCat(x, y) && !inEye(x, y) {
			return color.NRGBA{0, 0, 0, 255}
		}
		return color.NRGBA{}
	})
	// Coloured icon for Windows / Linux trays and the app itself.
	orange := color.NRGBA{0xd9, 0x77, 0x06, 255}
	app := func(x, y float64) color.NRGBA {
		const r = 0.22 // rounded square background
		dx, dy := math.Max(math.Abs(x-0.5)-(0.5-r), 0), math.Max(math.Abs(y-0.5)-(0.5-r), 0)
		if math.Hypot(dx, dy) > r {
			return color.NRGBA{}
		}
		cx, cy := (x-0.5)/0.72+0.5, (y-0.5)/0.72+0.5
		if inCat(cx, cy) && !inEye(cx, cy) {
			return color.NRGBA{255, 255, 255, 255}
		}
		return orange
	}
	write("assets/tray.png", 64, app)
	write("assets/appicon.png", 1024, app)
}
