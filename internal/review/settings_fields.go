package review

import "goodkind.io/pr-review-agent/internal/domain"

func (service *Service) carriedSettingFields(job domain.ReviewJob) []string {
	carried := make([]string, 0, 6)
	unsettled := job
	unsettled.Settings = domain.ReviewSettings{
		MinimumImportance: 0, MaxFiles: 0, MaxChunks: 0, ChunkTimeout: 0,
		ChunkConcurrency: 0, MaxPromptBytes: 0,
	}
	empty := service.settingsFor(unsettled)
	resolved := service.settingsFor(job)
	if resolved.minimumImportance != empty.minimumImportance {
		carried = append(carried, "minimum_importance")
	}
	if resolved.maxFiles != empty.maxFiles {
		carried = append(carried, "max_files")
	}
	if resolved.maxChunks != empty.maxChunks {
		carried = append(carried, "max_chunks")
	}
	if resolved.chunkTimeout != empty.chunkTimeout {
		carried = append(carried, "chunk_timeout")
	}
	if resolved.chunkConcurrency != empty.chunkConcurrency {
		carried = append(carried, "chunk_concurrency")
	}
	if resolved.maximumPromptBytes != empty.maximumPromptBytes {
		carried = append(carried, "max_prompt_bytes")
	}
	return carried
}
