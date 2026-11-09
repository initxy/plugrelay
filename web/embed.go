// Package web embeds the built console (React app in this directory).
//
// `npm run build` writes the console to dist/. A placeholder dist/index.html
// is committed so the Go build works before the console has been built; it
// carries <meta name="plugrelay-console" content="placeholder"> and tells the
// reader how to build the real thing.
package web

import (
	"embed"
	"io/fs"
)

// FS is the console build, rooted at "dist".
//
//go:embed all:dist
var FS embed.FS

// Dist returns the build output with "dist/" stripped, ready for http.FileServerFS.
func Dist() fs.FS {
	sub, err := fs.Sub(FS, "dist")
	if err != nil {
		panic(err) // unreachable: "dist" is embedded above
	}
	return sub
}
