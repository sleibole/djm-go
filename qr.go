package main

import (
	"fmt"
	"html/template"
	"strings"

	"rsc.io/qr"
)

// qrQuietZone is the blank border, in modules, that scanners need around a code.
const qrQuietZone = 4

// qrSVG draws text as an inline QR code. It has its own white background, so
// it scans on the dark TV display too. Each row's runs of dark modules are one
// path segment, which keeps the markup small.
func qrSVG(text, class string) (template.HTML, error) {
	code, err := qr.Encode(text, qr.M)
	if err != nil {
		return "", err
	}
	size := code.Size + 2*qrQuietZone
	var path strings.Builder
	for y := 0; y < code.Size; y++ {
		for x := 0; x < code.Size; {
			if !code.Black(x, y) {
				x++
				continue
			}
			run := 1
			for x+run < code.Size && code.Black(x+run, y) {
				run++
			}
			fmt.Fprintf(&path, "M%d %dh%dv1h-%dz", x+qrQuietZone, y+qrQuietZone, run, run)
			x += run
		}
	}
	return template.HTML(fmt.Sprintf(
		`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" class="%s" role="img" aria-label="QR code" shape-rendering="crispEdges">`+
			`<rect width="%d" height="%d" fill="#fff"/><path d="%s" fill="#000"/></svg>`,
		size, size, template.HTMLEscapeString(class), size, size, path.String())), nil
}
