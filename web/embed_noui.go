//go:build noui

package web

import "io/fs"

// UI is nil in backend-only builds, which never need Bun.
func UI() fs.FS { return nil }
