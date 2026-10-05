package openai

import (
	"encoding/json"
	"fmt"
	"log/slog"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

// MarshalReviewSchema constrains rule IDs to the catalog before a model returns findings.
func MarshalReviewSchema(policy reviewrules.Policy) (json.RawMessage, error) {
	ids, err := json.Marshal(policy.Catalog().IDs())
	if err != nil {
		slog.Warn("Review schema encoding failed")
		return nil, fmt.Errorf("encode review rule IDs: %w", err)
	}
	surfaces, err := json.Marshal([]domain.FindingSurface{
		domain.FindingFile, domain.FindingPullRequestTitle, domain.FindingPullRequestDescription, domain.FindingCommitMessage,
	})
	if err != nil {
		return nil, fmt.Errorf("encode finding surfaces: %w", err)
	}
	actions, err := json.Marshal([]domain.CorrectionAction{
		domain.CorrectionNone, domain.CorrectionReplace, domain.CorrectionDelete,
	})
	if err != nil {
		return nil, fmt.Errorf("encode finding correction actions: %w", err)
	}
	descriptions, err := marshalFindingDescriptions(policy)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(fmt.Sprintf(reviewSchemaTemplate, ids, surfaces,
		descriptions["surface"], descriptions["commit_sha"], actions, descriptions["correction_action"],
		descriptions["path"], descriptions["start_line"], descriptions["end_line"])), nil
}

func marshalFindingDescriptions(policy reviewrules.Policy) (map[string]json.RawMessage, error) {
	descriptions := make(map[string]json.RawMessage)
	var data reviewrules.PromptData
	for _, field := range []string{"surface", "commit_sha", "correction_action", "path", "start_line", "end_line"} {
		description, err := policy.Render("schema.finding."+field, data)
		if err != nil {
			slog.Warn("Finding schema description rendering failed", "field", field)
			return nil, fmt.Errorf("render finding schema description: %w", err)
		}
		encoded, err := json.Marshal(description)
		if err != nil {
			slog.Warn("Finding schema description encoding failed", "field", field)
			return nil, fmt.Errorf("encode finding schema description: %w", err)
		}
		descriptions[field] = encoded
	}
	return descriptions, nil
}

// MarshalReportSchema applies the configured walkthrough bound to structured output.
func MarshalReportSchema(policy reviewrules.Policy) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(reportSchemaTemplate, policy.Limits().WalkthroughItems))
}

const (
	reviewSchemaName      = "review_result"
	reportSchemaName      = "review_report"
	reconcileSchemaName   = "thread_resolutions"
	consolidateSchemaName = "finding_consolidation"
)

// The review schema asks for findings, whether supplied omission metadata is
// sufficient to judge the rest of the change, and a plain explanation.
var (
	reviewSchemaTemplate = `{"type":"object","additionalProperties":false,"properties":{"overview":{"type":"string"},"omissions_acceptable":{"type":"boolean"},"decision_reason":{"type":"string"},"findings":{"type":"array","items":{"type":"object","additionalProperties":false,"properties":{"rule_id":{"type":"string","enum":%s},"surface":{"type":"string","enum":%s,"description":%s},"commit_sha":{"type":"string","description":%s},"correction_action":{"type":"string","enum":%s,"description":%s},"path":{"type":"string","description":%s},"start_line":{"type":"integer","minimum":1,"description":%s},"end_line":{"type":"integer","minimum":1,"description":%s},"title":{"type":"string"},"body":{"type":"string"},"evidence":{"type":"string"},"claim":{"type":"string"},"suggestion":{"type":"string"},"importance":{"type":"integer","minimum":1,"maximum":10}},"required":["rule_id","surface","commit_sha","correction_action","path","start_line","end_line","title","body","evidence","claim","suggestion","importance"]}}},"required":["overview","omissions_acceptable","decision_reason","findings"]}`
	reportSchemaTemplate = `{"type":"object","additionalProperties":false,"properties":{"summary":{"type":"string"},"walkthrough":{"type":"array","maxItems":%d,"items":{"type":"string"}}},"required":["summary","walkthrough"]}`
	reconcileSchemaJSON  = json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"resolutions":{"type":"array","items":{"type":"object","additionalProperties":false,"properties":{"thread_node_id":{"type":"string"},"resolution":{"type":"string","enum":["resolved","open","uncertain"]},"reason":{"type":"string"}},"required":["thread_node_id","resolution","reason"]}}},"required":["resolutions"]}`)
	// consolidateSchemaJSON groups a chunk's own findings. The candidate numbers
	// are the ones the prompt showed, so the answer says nothing this service
	// cannot check against the findings it asked about.
	consolidateSchemaJSON = json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"groups":{"type":"array","items":{"type":"object","additionalProperties":false,"properties":{"candidates":{"type":"array","items":{"type":"integer"}},"restates_open_thread":{"type":"boolean"},"reason":{"type":"string"}},"required":["candidates","restates_open_thread","reason"]}}},"required":["groups"]}`)
)
