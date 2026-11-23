package tooling

import (
	"encoding/json"
	"strings"
)

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
