package main

import (
	"embed"
	"fmt"
	"html/template"
)

// Tabler icons (MIT, see icons/LICENSE). Each SVG was copied in with aria-hidden="true"
// and class="icon", so icon-only buttons need their own aria-label.
//
//go:embed icons/*.svg
var iconFS embed.FS

// icon inlines an SVG so its stroke="currentColor" follows the surrounding text color.
func icon(name string) (template.HTML, error) {
	b, err := iconFS.ReadFile("icons/" + name + ".svg")
	if err != nil {
		return "", fmt.Errorf("icon %q: %w", name, err)
	}
	return template.HTML(b), nil
}
