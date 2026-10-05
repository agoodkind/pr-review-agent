package openai

import (
	"encoding/json"
	"fmt"
	"log/slog"

	"goodkind.io/pr-review-agent/internal/reviewrules"
)

// MarshalReviewSchema constrains rule IDs to the catalog before a model returns findings.
func MarshalReviewSchema() (json.RawMessage, error) {
	ids, err := json.Marshal(reviewrules.IDs())
	if err != nil {
		slog.Warn("Review schema encoding failed")
		return nil, fmt.Errorf("encode review rule IDs: %w", err)
	}
	return json.RawMessage(fmt.Sprintf(reviewSchemaTemplate, ids)), nil
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
	reviewSchemaTemplate = `{"type":"object","additionalProperties":false,"properties":{"overview":{"type":"string"},"omissions_acceptable":{"type":"boolean"},"decision_reason":{"type":"string"},"findings":{"type":"array","items":{"type":"object","additionalProperties":false,"properties":{"rule_id":{"type":"string","enum":%s},"path":{"type":"string"},"start_line":{"type":"integer"},"end_line":{"type":"integer"},"title":{"type":"string"},"body":{"type":"string"},"evidence":{"type":"string"},"claim":{"type":"string"},"suggestion":{"type":"string"},"importance":{"type":"integer"}},"required":["rule_id","path","start_line","end_line","title","body","evidence","claim","suggestion","importance"]}}},"required":["overview","omissions_acceptable","decision_reason","findings"]}`
	reportSchemaJSON     = json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"summary":{"type":"string"},"walkthrough":{"type":"array","maxItems":4,"items":{"type":"string"}}},"required":["summary","walkthrough"]}`)
	reconcileSchemaJSON  = json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"resolutions":{"type":"array","items":{"type":"object","additionalProperties":false,"properties":{"thread_node_id":{"type":"string"},"resolution":{"type":"string","enum":["resolved","open","uncertain"]},"reason":{"type":"string"}},"required":["thread_node_id","resolution","reason"]}}},"required":["resolutions"]}`)
	// consolidateSchemaJSON groups a chunk's own findings. The candidate numbers
	// are the ones the prompt showed, so the answer says nothing this service
	// cannot check against the findings it asked about.
	consolidateSchemaJSON = json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"groups":{"type":"array","items":{"type":"object","additionalProperties":false,"properties":{"candidates":{"type":"array","items":{"type":"integer"}},"restates_open_thread":{"type":"boolean"},"reason":{"type":"string"}},"required":["candidates","restates_open_thread","reason"]}}},"required":["groups"]}`)
)
