package liveproof

import (
	"os"
	"path/filepath"
)

// ReadReport loads a named report from the report directory.
func ReadReport(reportDirectory string, reportName string) ([]byte, error) {
	reportPath := filepath.Join(reportDirectory, reportName)
	return os.ReadFile(reportPath)
}
