// Package version holds build identity stamped by go-makefile.
package version

import "fmt"

// Commit, Version, Dirty, and BuildTime are set through build linker flags.
var (
	Commit    string
	Version   string
	Dirty     string
	BuildTime string
)

// String returns the binary's build identity.
func String() string {
	version := Version
	if version == "" {
		version = "dev"
	}

	commit := Commit
	if commit == "" {
		commit = "unknown"
	}

	buildTime := BuildTime
	if buildTime == "" {
		buildTime = "unknown"
	}

	return fmt.Sprintf("pr-review-agent %s (%s, built %s)", version, commit, buildTime)
}
