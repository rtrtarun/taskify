//go:build !webembed

// Package web holds the built frontend. See embed.go.
package web

import "io/fs"

// Dist reports that this binary was built without the frontend.
func Dist() (fs.FS, bool) { return nil, false }
