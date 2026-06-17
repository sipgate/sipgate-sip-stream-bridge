// Package web serves the read-only operator UI: a Svelte single-page bundle,
// built once under ./ui and embedded into the binary at compile time. It is
// mounted at /ui by cmd/sipgate-sip-stream-bridge and guarded by the same
// HTTP Basic Auth as the REST control plane (same origin, no CORS).
//
// The bundle physically lives in the `dist` subdirectory (populated by
// `make ui-go`). A committed placeholder index.html keeps `//go:embed dist`
// compilable on a fresh checkout before the bundle has been built.
package web

import (
	"embed"
	"io/fs"
)

//go:embed dist
var distFS embed.FS

// distSub returns the embedded bundle rooted at dist/, so files are served as
// /index.html, /assets/… rather than /dist/index.html.
func distSub() fs.FS {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		// Unreachable: "dist" is a compile-time embed and a valid sub-path;
		// fs.Sub only errors on an invalid path. Panic surfaces a build mistake.
		panic(err)
	}
	return sub
}
