//go:build !noui

// Package web embeds the pane that `bun run build` writes to dist/client.
package web

import (
	"embed"
	"io/fs"
)

// The all: prefix keeps files starting with _, such as the SPA shell.
//
//go:embed all:dist/client
var dist embed.FS

// UI is the built pane, rooted at dist/client.
func UI() fs.FS {
	ui, err := fs.Sub(dist, "dist/client")
	if err != nil {
		panic(err)
	}
	return ui
}
