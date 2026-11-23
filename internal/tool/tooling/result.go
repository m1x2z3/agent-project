package tooling

import (
	"encoding/json"
	"strings"
)

// Validate 检查结果状态和错误字段是否一致。
func (r ToolResult) Validate() error {
	if strings.TrimSpace(r.ToolName) == "" || strings.TrimSpace(r.ToolVersion) == "" ||
		!validResultStatus(r.Status) {
		return ErrResultInvalid
	}
	if r.Status == ResultSuccess {
		if r.Error != nil || !json.Valid(r.Data) {
			return ErrResultInvalid
		}
		return nil
	}
	if r.Error == nil ||
		strings.TrimSpace(r.Error.Code) == "" ||
		strings.TrimSpace(r.Error.Message) == "" {
		return ErrResultInvalid
	}
	if len(r.Data) > 0 && !json.Valid(r.Data) {
		return ErrResultInvalid
	}
	return nil
}

func validResultStatus(status ResultStatus) bool {
	switch status {
	case ResultSuccess, ResultInvalidArgument, ResultNotFound, ResultForbidden,
		ResultTimeout, ResultInternalError:
		return true
	default:
		return false
	}
}
