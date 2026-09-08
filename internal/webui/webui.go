// Package webui holds the built single-page application, embedded into the
// binary so the .deb ships one file.
//
// The Vite build in web/ writes into the dist directory here. A placeholder
// index.html is committed so the embed directive resolves on a fresh clone,
// before the frontend has ever been built.
package webui

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var embedded embed.FS

// FS returns the SPA's file tree rooted at dist.
func FS() (fs.FS, error) { return fs.Sub(embedded, "dist") }

// Built reports whether a real frontend build is present, as opposed to the
// committed placeholder. The service uses this to warn rather than to fail.
func Built() bool {
	entries, err := embedded.ReadDir("dist/assets")
	return err == nil && len(entries) > 0
}
