// Package buildinfo holds the version metadata stamped into the kb binary at
// link time, by the Makefile and .goreleaser.yaml alike:
//
//	-X github.com/xen0bit/kb/internal/buildinfo.Version=...
package buildinfo

import (
	"fmt"
	"runtime"
)

var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// String is the one-line form printed by `kb version`.
func String() string {
	return fmt.Sprintf("kb %s (commit %s, built %s, %s/%s)",
		Version, Commit, Date, runtime.GOOS, runtime.GOARCH)
}
