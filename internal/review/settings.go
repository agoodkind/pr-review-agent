package review

import (
	"time"

	"goodkind.io/pr-review-agent/internal/domain"
)

type reviewSettings struct {
	approveEachCommit  bool
	minimumImportance  int
	maxFiles           int
	maxChunks          int
	chunkTimeout       time.Duration
	chunkConcurrency   int
	maximumPromptBytes int
}

// Numeric overrides require positive values, and importance cannot exceed ten.
// A nil approval setting uses the process value; explicit false overrides true.
func (service *Service) settingsFor(job domain.ReviewJob) reviewSettings {
	settings := reviewSettings{
		approveEachCommit:  service.approveEachCommit,
		minimumImportance:  service.minimumImportance,
		maxFiles:           service.reviewMaxFiles,
		maxChunks:          service.reviewMaxChunks,
		chunkTimeout:       service.chunkTimeout,
		chunkConcurrency:   service.chunkConcurrency,
		maximumPromptBytes: service.maximumPromptBytes,
	}
	if job.Settings.ApproveEachCommit != nil {
		settings.approveEachCommit = *job.Settings.ApproveEachCommit
	}
	if job.Settings.MinimumImportance > 0 && job.Settings.MinimumImportance <= domain.MaximumFindingImportance {
		settings.minimumImportance = job.Settings.MinimumImportance
	}
	if job.Settings.MaxFiles > 0 {
		settings.maxFiles = job.Settings.MaxFiles
	}
	if job.Settings.MaxChunks > 0 {
		settings.maxChunks = job.Settings.MaxChunks
	}
	if job.Settings.ChunkTimeout > 0 {
		settings.chunkTimeout = job.Settings.ChunkTimeout
	}
	if job.Settings.ChunkConcurrency > 0 {
		settings.chunkConcurrency = job.Settings.ChunkConcurrency
	}
	if job.Settings.MaxPromptBytes > 0 {
		settings.maximumPromptBytes = job.Settings.MaxPromptBytes
	}
	return settings
}
