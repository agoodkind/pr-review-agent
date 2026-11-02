package review_test

import (
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
)

func TestCommitMetadataReassessmentUsesTheCompleteCurrentCommitSet(t *testing.T) {
	for _, mode := range []string{"ordinary", "metadata edit"} {
		for _, scenario := range []string{"surviving target", "removed target", "replacement finding", "incomplete list", "duplicate list"} {
			t.Run(mode+"/"+scenario, func(t *testing.T) {
				prior := metadataPresentationFinding()
				prior.Surface = domain.FindingCommitMessage
				prior.CommitSHA = domain.HeadSHA(testStaleHeadSHA)
				commits := []githubapp.PullRequestCommit{{SHA: domain.HeadSHA(testHeadSHA), Message: "Update current review behavior."}}
				model := &sequenceModel{results: []domain.ReviewResult{{}}}
				options := contentCollectionOptions{onlyMetadata: true}
				switch scenario {
				case "surviving target":
					commits = append(commits, githubapp.PullRequestCommit{SHA: prior.CommitSHA, Message: prior.Evidence})
					model.metadataAssessments = [][]domain.MetadataFindingAssessment{metadataDecision("1", domain.ResolutionOpen, prior.Evidence)}
				case "replacement finding":
					commits[0].Message = "Replacement summary fragment"
					replacement := prior
					replacement.CommitSHA = commits[0].SHA
					replacement.Title = "Complete the replacement commit message"
					replacement.Body = "The replacement commit message omits a complete sentence."
					replacement.Evidence = commits[0].Message
					model.results[0].Findings = []domain.Finding{replacement}
				case "incomplete list":
					count := 2
					options.advertisedCommitCount = &count
				case "duplicate list":
					commits = append(commits, commits[0])
				}
				options.commits = commits
				fixture, _ := newContentCollectionFixture(t, model, options)
				seedMetadataReassessment(t, fixture, []domain.Finding{prior}, mode == "metadata edit")
				job := fixture.job()
				job.MetadataOnly = mode == "metadata edit"
				err := fixture.run(t.Context(), job)
				failureExpected := scenario == "incomplete list" || scenario == "duplicate list"
				if (err != nil) != failureExpected {
					t.Fatalf("Commit verification error was %v; expected failure is %t.", err, failureExpected)
				}
				stored := retainedMetadataFindings(t, failureSummaryComment(t, fixture))
				switch scenario {
				case "surviving target":
					if len(stored) != 1 || stored[0].CommitSHA != prior.CommitSHA || len(model.metadataPrompts) != 1 || !strings.Contains(model.metadataPrompts[0], prior.Evidence) || !strings.Contains(model.prompts[0], string(prior.CommitSHA)) {
						t.Fatal("Current collection and reassessment must include an unchanged earlier commit message.")
					}
				case "removed target":
					if len(stored) != 0 || len(model.metadataPrompts) != 0 || fixture.state.lastSubmitReview["event"] != string(domain.ReviewDecisionApprove) {
						t.Fatal("A complete current commit list must retire a finding for a removed commit.")
					}
				case "replacement finding":
					if len(stored) != 1 || stored[0].CommitSHA != job.Head || len(model.metadataPrompts) != 0 || fixture.state.lastSubmitReview["event"] != string(domain.ReviewDecisionRequestChanges) {
						t.Fatal("The replacement commit message must receive ordinary fresh review and preserve its new finding.")
					}
				case "incomplete list", "duplicate list":
					if len(stored) != 1 || stored[0].CommitSHA != prior.CommitSHA || len(fixture.state.submittedReviews) != 0 {
						t.Fatal("An incomplete or duplicate commit list must preserve the saved finding and withhold approval.")
					}
				}
			})
		}
	}
}
