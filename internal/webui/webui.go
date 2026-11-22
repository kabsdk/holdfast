// Package webui holds the UI: static files embedded in holdfast-server (plan-m4 §5, Q5). It is a thin client
// of /api/v1 and holds no logic of its own (decision 028). There is no build step: plain ES modules, with
// Preact and htm vendored at pinned versions.
package webui

import (
	"embed"
	"io/fs"
)

//go:embed static
var files embed.FS

// Files are the UI's files, index.html at the root.
func Files() fs.FS {
	sub, err := fs.Sub(files, "static")
	if err != nil {
		panic(err)
	}
	return sub
}
