package cloudflareops

import "strings"

func testRevisionAllowed(actual string, revisions []string) bool {
	if !reportedRevisionPattern.MatchString(actual) {
		return false
	}

	for _, revision := range revisions {
		if strings.HasPrefix(revision, actual) {
			return true
		}
	}

	return false
}
