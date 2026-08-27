// Package version exposes build version information.
package version

// release is set from the repository VERSION file for distributable builds.
var release = "dev"

// String returns the semantic version without the Git tag's leading v.
func String() string {
	return release
}
