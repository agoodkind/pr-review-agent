package version

import "runtime/debug"

var sourceRevision = readSourceRevision()

// SourceRevision returns the source commit recorded in the binary build metadata.
func SourceRevision() string {
	return sourceRevision
}

func readSourceRevision() string {
	information, available := debug.ReadBuildInfo()
	revision := ""
	modified := ""
	if available {
		for _, setting := range information.Settings {
			if setting.Key == "vcs.revision" {
				revision = setting.Value
			}
			if setting.Key == "vcs.modified" {
				modified = setting.Value
			}
		}
	}
	if revision != "" {
		if modified == "false" {
			return revision
		}
		if modified == "true" {
			return revision + "-dirty"
		}
		return revision + "-unverified"
	}
	if len(Commit) == 40 {
		return Commit + "-unversioned"
	}
	return Commit
}
