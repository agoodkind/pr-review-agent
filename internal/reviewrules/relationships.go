package reviewrules

import (
	"fmt"
	"text/template"
)

// RelationshipReviewPolicy bounds joint review of separated source fragments.
type RelationshipReviewPolicy struct {
	Enabled      bool `json:"enabled"`
	MaximumPairs int  `json:"maximum_pairs"`
}

// Validate requires an explicit positive work limit when joint review is enabled.
func (policy RelationshipReviewPolicy) Validate() error {
	if policy.MaximumPairs < 0 || (policy.Enabled && policy.MaximumPairs == 0) {
		return fmt.Errorf("relationship_review.maximum_pairs must be positive when enabled")
	}
	return nil
}

// RelationshipReview returns the configured joint review policy.
func (policy Policy) RelationshipReview() RelationshipReviewPolicy {
	return policy.relationshipReview
}

func validateRelationshipTemplates(policy RelationshipReviewPolicy, templates map[string]*template.Template) error {
	if !policy.Enabled {
		return nil
	}
	for _, name := range []string{"relationship.input", "relationship.fragment", "relationship.gap"} {
		if _, exists := templates[name]; !exists {
			return fmt.Errorf("required prompt template %q is missing", name)
		}
	}
	return nil
}
