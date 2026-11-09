// Package version carries the build version, set with -ldflags at release.
package version

// Version is the plugrelay release without the tag's "v" (goreleaser's
// {{.Version}}, e.g. "0.1.0"); "dev" for local builds.
var Version = "dev"
