package openai

import "encoding/json"

const (
	reviewSchemaName      = "review_result"
	reconcileSchemaName   = "thread_resolutions"
	consolidateSchemaName = "finding_consolidation"
)

// The review schema asks for findings and nothing else.
//
// It used to require a coverage_complete boolean, and no prompt ever told the
// model what the field meant, so the model answered a required boolean blind.
// One blind false set the whole pass incomplete and blocked a head with nothing
// wrong with it. Whether this service handed the model the whole diff is a fact
// only this service can observe, so it is never asked for here.
var (
	reviewSchemaJSON    = json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"findings":{"type":"array","items":{"type":"object","additionalProperties":false,"properties":{"path":{"type":"string"},"start_line":{"type":"integer"},"end_line":{"type":"integer"},"title":{"type":"string"},"body":{"type":"string"},"evidence":{"type":"string"},"claim":{"type":"string"},"suggestion":{"type":"string"},"importance":{"type":"integer"}},"required":["path","start_line","end_line","title","body","evidence","claim","suggestion","importance"]}}},"required":["findings"]}`)
	reconcileSchemaJSON = json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"resolutions":{"type":"array","items":{"type":"object","additionalProperties":false,"properties":{"thread_node_id":{"type":"string"},"resolution":{"type":"string","enum":["resolved","open","uncertain"]},"reason":{"type":"string"}},"required":["thread_node_id","resolution","reason"]}}},"required":["resolutions"]}`)
	// consolidateSchemaJSON groups a chunk's own findings. The candidate numbers
	// are the ones the prompt showed, so the answer says nothing this service
	// cannot check against the findings it asked about.
	consolidateSchemaJSON = json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"groups":{"type":"array","items":{"type":"object","additionalProperties":false,"properties":{"candidates":{"type":"array","items":{"type":"integer"}},"restates_open_thread":{"type":"boolean"},"reason":{"type":"string"}},"required":["candidates","restates_open_thread","reason"]}}},"required":["groups"]}`)
)
