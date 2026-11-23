package tooling

import (
	"encoding/json"
	"strings"
)

// ResultStatus 是工具结果的稳定状态枚举。
type ResultStatus string

const (
	ResultSuccess         ResultStatus = "success"
	ResultInvalidArgument ResultStatus = "invalid_argument"
	ResultNotFound        ResultStatus = "not_found"
	ResultForbidden       ResultStatus = "forbidden"
	ResultTimeout         ResultStatus = "timeout"
	ResultInternalError   ResultStatus = "internal_error"
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
