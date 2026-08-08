// Package teamdating carries the embedded frontend (DD-18).
//
// The //go:embed directive can only reach files inside its own package
// directory, and web/ lives at the module root — so the declaration lives here
// rather than under cmd/teamdating/. The alternative (a second copy of web/
// under cmd/) would mean two sources of truth for the same assets, and nothing
// in the build would tell you which one you were actually serving.
package teamdating

import (
	"embed"
	"io/fs"
)

//go:embed web
var embedded embed.FS

// Web returns the embedded web/ directory rooted at its contents, so
// "index.html" resolves rather than "web/index.html".
func Web() (fs.FS, error) { return fs.Sub(embedded, "web") }
