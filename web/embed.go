//go:build webembed

// Package web holds the built frontend. With the webembed build tag the
// contents of web/dist are compiled into the binary (run `npm run build` in
// web/ first); without it Dist reports that no frontend is present, so a plain
// `go build` needs no Node toolchain.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// Dist returns the built frontend with index.html at its root.
func Dist() (fs.FS, bool) {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil, false
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return nil, false
	}
	return sub, true
}
