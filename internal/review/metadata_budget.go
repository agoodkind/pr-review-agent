package review

import "goodkind.io/pr-review-agent/internal/diff"

func (service *Service) boundMetadataChunks(input diff.ReviewInput, chunks []diff.Chunk, support string, settings reviewSettings) ([]diff.Chunk, string, error) {
	for {
		bounded, err := BoundReviewChunks(chunks, nil, settings.minimumImportance, support, service.reviewPolicy, settings.maximumPromptBytes)
		if err != nil {
			return nil, "", err
		}
		work := deltaWork{PullRequest: input.PullRequest, Files: nil, Metadata: input.Metadata, Chunks: bounded}
		context, err := reviewContext(input.PullRequest, nil, classifyStructuralShortfall(work), settings.maximumPromptBytes, service.reviewPolicy)
		if err != nil {
			return nil, "", err
		}
		chunks = bounded
		if context == support {
			return chunks, support, nil
		}
		support = context
	}
}
