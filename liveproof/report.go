package liveproof

import (
	"os"
)

// ReadReport loads a named report from the report directory.
func ReadReport(reportDirectory string, reportName string) ([]byte, error) {
	root, err := os.OpenRoot(reportDirectory)
	if err != nil {
		return nil, err
	}
	defer root.Close()

	return root.ReadFile(reportName)
}
