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

// SourceRef 表示工具结果的来源位置。
type SourceRef struct {
	Source   string
	Location string
}

// ToolError 是不绑定供应商协议的工具错误结构。
type ToolError struct {
	Code      string
	Message   string
	Retryable bool
}

// ToolResult 是工具返回给 Agent Runtime 的统一结果 Envelope。
type ToolResult struct {
	ToolName    string
	ToolVersion string
	Status      ResultStatus
	Data        json.RawMessage
	Source      []SourceRef
	Error       *ToolError
}

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
