package marker

import (
	"fmt"
	"strings"
)

// FailureDismissal binds automatic withdrawal evidence to one review ID.
func FailureDismissal(reviewID int64) string {
	return fmt.Sprintf("<!-- pr-review-agent:failure-dismissal:v1 review=%d -->", reviewID)
}

// HasFailureDismissal requires the review ID from the GitHub dismissal event.
func HasFailureDismissal(message string, reviewID int64) bool {
	return strings.Contains(message, FailureDismissal(reviewID))
}
