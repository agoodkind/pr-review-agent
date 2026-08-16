package openai

import "encoding/json"

const (
	reviewSchemaName    = "review_result"
	reconcileSchemaName = "thread_resolutions"
)

var (
	reviewSchemaJSON    = json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"coverage_complete":{"type":"boolean"},"findings":{"type":"array","items":{"type":"object","additionalProperties":false,"properties":{"path":{"type":"string"},"start_line":{"type":"integer"},"end_line":{"type":"integer"},"title":{"type":"string"},"body":{"type":"string"},"suggestion":{"type":"string"},"importance":{"type":"integer"}},"required":["path","start_line","end_line","title","body","suggestion","importance"]}}},"required":["coverage_complete","findings"]}`)
	reconcileSchemaJSON = json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"resolutions":{"type":"array","items":{"type":"object","additionalProperties":false,"properties":{"thread_node_id":{"type":"string"},"resolution":{"type":"string","enum":["resolved","open","uncertain"]},"reason":{"type":"string"}},"required":["thread_node_id","resolution","reason"]}}},"required":["resolutions"]}`)
)
