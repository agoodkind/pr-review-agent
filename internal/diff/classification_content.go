package diff

import "strings"

// ClassificationContent returns available source text for file classification.
func (file *FileContext) ClassificationContent() string {
	if file.Status != "removed" {
		return file.CurrentContent
	}
	parsed, err := parsePatch(file.Patch)
	if err != nil {
		return ""
	}
	var lines []string
	for _, hunk := range parsed.hunks {
		previousWasOld := false
		for _, row := range strings.Split(hunk.text, "\n")[1:] {
			if row == `\ No newline at end of file` {
				if previousWasOld && len(lines) > 0 {
					last := len(lines) - 1
					lines[last] = strings.TrimSuffix(lines[last], "\n")
				}
				continue
			}
			previousWasOld = strings.HasPrefix(row, "-") || strings.HasPrefix(row, " ")
			if previousWasOld {
				lines = append(lines, row[1:]+"\n")
			}
		}
	}
	return strings.Join(lines, "")
}
