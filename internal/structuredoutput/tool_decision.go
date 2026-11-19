package structuredoutput

import (
	"errors"
	"strings"
)

// ToolDecision 表示模型计划是否允许调用工具，以及调用哪个工具。
type ToolDecision struct {
	ShouldCall bool           `json:"should_call" jsonschema:"required"`
	ToolName   string         `json:"tool_name,omitempty"`
	Arguments  map[string]any `json:"arguments,omitempty"`
}

// Validate 校验 should_call、tool_name 和 arguments 之间的业务关系。
func (v ToolDecision) Validate() error {
	if v.ShouldCall {
		if strings.TrimSpace(v.ToolName) == "" {
			return errors.New("tool_name is required when should_call is true")
		}
		if v.Arguments == nil {
			return errors.New("arguments is required when should_call is true")
		}
		return nil
	}
	if strings.TrimSpace(v.ToolName) != "" || v.Arguments != nil {
		return errors.New("tool_name and arguments must be omitted when should_call is false")
	}
	return nil
}
