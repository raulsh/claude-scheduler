package api

import (
	"io/fs"

	"github.com/raulsh/claude-scheduler/internal/webui"
)

// webFS resolves the embedded SPA tree. It is a variable so tests can swap
// in a fixture without a build step.
var webFS = func() (fs.FS, error) { return webui.FS() }
