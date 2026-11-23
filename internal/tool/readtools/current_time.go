package readtools

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"agent-project/internal/tool/tooling"
)

const (
	// GetCurrentTimeName 是当前时间工具的稳定名称。
	GetCurrentTimeName = "get_current_time"
)

var getCurrentTimeParameterSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "timezone": {
      "type": "string",
      "minLength": 1,
      "description": "IANA 时区，例如 Asia/Shanghai"
    }
  },
  "required": ["timezone"],
  "additionalProperties": false
}`)

// GetCurrentTimeParameterSchema 返回当前时间工具的参数 Schema 副本。
//
// 返回副本而不是共享可变切片，避免调用方修改 Schema 后影响后续工具定义。
func GetCurrentTimeParameterSchema() json.RawMessage {
	cloned := make(json.RawMessage, len(getCurrentTimeParameterSchema))
	copy(cloned, getCurrentTimeParameterSchema)
	return cloned
}

// CurrentTimeTool 返回指定 IANA 时区的当前时间。
//
// Now 由调用方注入，生产环境可以传入 time.Now，测试则使用固定时钟，
// 从而避免单测依赖机器当前时间。
type CurrentTimeTool struct {
	now      func() time.Time
	observer Observer
}

// NewCurrentTimeTool 创建当前时间只读工具。
func NewCurrentTimeTool(now func() time.Time, observer Observer) *CurrentTimeTool {
	if now == nil {
		now = time.Now
	}
	if observer == nil {
		observer = NoopObserver{}
	}
	return &CurrentTimeTool{now: now, observer: observer}
}

// Definition 返回当前时间工具的稳定契约。
func (t *CurrentTimeTool) Definition() tooling.ToolDefinition {
	return tooling.ToolDefinition{
		Name:            GetCurrentTimeName,
		Version:         ReadToolVersion,
		Description:     "读取指定 IANA 时区的当前时间",
		Permission:      tooling.Permission("time:read"),
		RiskLevel:       tooling.RiskLow,
		EffectClass:     tooling.EffectRead,
		Timeout:         time.Second,
		ParameterSchema: GetCurrentTimeParameterSchema(),
		TenantScope:     tooling.TenantScopeGlobal,
		CapabilityTags:  []string{"read_only", "time"},
	}
}

// Execute 解析时区并生成稳定的 JSON 结果。
//
// 租户、Run、能力令牌和参数 Schema 已由 tooling.Invoke 在进入 Execute 前校验；
// Execute 只负责当前时间这个工具自身的业务语义。
func (t *CurrentTimeTool) Execute(
	ctx context.Context,
	request tooling.ToolExecutionRequest,
) (result tooling.ToolResult, returnErr error) {
	start := time.Now()
	result = tooling.ToolResult{
		ToolName:    GetCurrentTimeName,
		ToolVersion: ReadToolVersion,
	}
	defer func() {
		observer := t.observer
		if isObservationSuppressed(ctx) {
			observer = NoopObserver{}
		}
		observeTool(observer, start, request, result, returnErr)
	}()

	if err := ctx.Err(); err != nil {
		return tooling.ToolResult{}, err
	}

	var arguments currentTimeArguments
	if err := json.Unmarshal(request.Arguments, &arguments); err != nil {
		result.Status = tooling.ResultInvalidArgument
		result.Error = &tooling.ToolError{
			Code:      "invalid_argument",
			Message:   "timezone argument is invalid",
			Retryable: false,
		}
		return result, nil
	}

	timezone := strings.TrimSpace(arguments.Timezone)
	location, err := time.LoadLocation(timezone)
	if err != nil {
		result.Status = tooling.ResultInvalidArgument
		result.Error = &tooling.ToolError{
			Code:      "invalid_timezone",
			Message:   "timezone must be a valid IANA location",
			Retryable: false,
		}
		return result, nil
	}

	current := t.now().In(location)
	data, err := json.Marshal(currentTimeResult{
		CurrentTime:   current.Format(time.RFC3339),
		Timezone:      timezone,
		UnixTimestamp: current.Unix(),
	})
	if err != nil {
		result.Status = tooling.ResultInternalError
		result.Error = &tooling.ToolError{
			Code:      "result_encoding_failed",
			Message:   "failed to encode tool result",
			Retryable: false,
		}
		return result, nil
	}

	result.Status = tooling.ResultSuccess
	result.Data = data
	return result, nil
}

type currentTimeArguments struct {
	Timezone string `json:"timezone"`
}

type currentTimeResult struct {
	CurrentTime   string `json:"current_time"`
	Timezone      string `json:"timezone"`
	UnixTimestamp int64  `json:"unix_timestamp"`
}

var _ tooling.Tool = (*CurrentTimeTool)(nil)
