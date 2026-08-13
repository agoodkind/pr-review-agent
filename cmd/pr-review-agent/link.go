package main

import (
	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/webhook"
)

func init() {
	keepInternalPackagesLinked()
}

// keepInternalPackagesLinked retains references until the HTTP runtime wires them in.
func keepInternalPackagesLinked() {
	_, _ = domain.ParseHeadSHA("")
	_, _ = domain.ParseReviewDecision("")
	_, _ = domain.ParseResolution("")
	_ = domain.PullRequestRef{}.Key()
	_ = domain.Finding{}.Validate()
	_ = domain.ReviewResult{}.Validate()
	_, _ = config.Load(func(string) (string, bool) { return "", false })
	_, _ = config.FromEnvironment()
	_ = marker.Review("")
	_, _ = marker.FindReview("")
	_, _ = marker.Finding("", domain.Finding{})
	_, _ = marker.FindFinding("")
	_, _ = marker.EncodeFindingBody("", domain.Finding{})
	_, _, _ = marker.DecodeFindingBody(domain.ReviewComment{})
	_, _ = marker.NormalizePath("")
	_, _, _ = webhook.ParsePullRequest("", "", nil)
	_ = webhook.VerifySHA256("", nil, nil)
	_ = webhook.PullRequestEvent{}.Job()
}
