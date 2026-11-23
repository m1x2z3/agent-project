package tooling

import (
	"encoding/json"
	"strings"
)

// ToolExecutionRequest 是经过模型决策转换后，准备进入工具执行边界的请求。
type ToolExecutionRequest struct {
	ToolName        string
	ToolVersion     string
	TenantID        string
	RunID           string
	RequestID       string
	Arguments       json.RawMessage
	ResourceScope   []string
	CapabilityToken CapabilityToken
	IdempotencyKey  string
}

// Validate 检查请求自身的确定性字段，不检查工具定义和能力令牌。
func (r ToolExecutionRequest) Validate() error {
	if strings.TrimSpace(r.ToolName) == "" ||
		strings.TrimSpace(r.ToolVersion) == "" ||
		strings.TrimSpace(r.TenantID) == "" ||
		strings.TrimSpace(r.RunID) == "" ||
		strings.TrimSpace(r.RequestID) == "" ||
		!json.Valid(r.Arguments) {
		return ErrInvalidRequest
	}
	return nil
}
