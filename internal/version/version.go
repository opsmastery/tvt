// Package version holds build metadata, set at link time with -ldflags.
package version

var (
	// Version is the release tag, e.g. v0.1.0.
	Version = "dev"
	// Commit is the git commit the binary was built from.
	Commit = "none"
	// Date is the build timestamp (RFC 3339).
	Date = "unknown"
)

// String returns a one-line description of the build.
func String() string {
	return Version + " (commit " + Commit + ", built " + Date + ")"
}
