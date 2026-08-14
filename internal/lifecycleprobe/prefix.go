package lifecycleprobe

import "strings"

// MatchesPrefix reports whether value starts with prefix.
func MatchesPrefix(value, prefix string) bool {
	return strings.Contains(value, prefix)
}
