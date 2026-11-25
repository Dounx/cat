//go:build ignore

// Renders the desktop PNG icons from the SVG sources in assets/:
//
//	go run assets/gen.go   (from desktop/)
//
// Requires rsvg-convert (librsvg).
package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
)

type target struct {
	src  string
	dst  string
	size int
}

func main() {
	bin, err := exec.LookPath("rsvg-convert")
	if err != nil {
		fmt.Fprintln(os.Stderr, "rsvg-convert not found (install librsvg, e.g. `brew install librsvg`)")
		os.Exit(1)
	}
	targets := []target{
		{src: "assets/tray-template.svg", dst: "assets/tray-template.png", size: 44},
		{src: "assets/tray.svg", dst: "assets/tray.png", size: 64},
		{src: "assets/appicon.svg", dst: "assets/appicon.png", size: 1024},
	}
	for _, t := range targets {
		s := strconv.Itoa(t.size)
		cmd := exec.Command(bin, "-w", s, "-h", s, t.src, "-o", t.dst)
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "render %s -> %s: %v\n", t.src, t.dst, err)
			os.Exit(1)
		}
		fmt.Printf("rendered %s (%dx%d)\n", t.dst, t.size, t.size)
	}
}
