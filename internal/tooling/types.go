package tooling

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

// Tool 是工具实现必须满足的最小接口。
//
// 工具实现不接收模型原始输出。调用方应通过 Invoke 进入确定性执行边界，
// 先完成权限、租户、Run 和能力令牌校验，再调用 Execute。
type Tool interface {
	Definition() ToolDefinition
	Execute(context.Context, ToolExecutionRequest) (ToolResult, error)
}

// Permission 表示工具要求的稳定权限标识。
type Permission string

// RiskLevel 表示工具执行失败或被误用时的风险等级。
type RiskLevel string

const (
	RiskLow    RiskLevel = "low"
	RiskMedium RiskLevel = "medium"
	RiskHigh   RiskLevel = "high"
)

// EffectClass 表示工具可能产生的副作用类别。
type EffectClass string

const (
	EffectRead              EffectClass = "read"
	EffectIdempotentWrite   EffectClass = "idempotent_write"
	EffectNonRetryableWrite EffectClass = "non_retryable_write"
)

// TenantScope 表示工具允许访问的租户边界。
type TenantScope string

const (
	// TenantScopeGlobal 表示工具不限制到某个具体租户，但仍要求调用链携带租户上下文。
	TenantScopeGlobal TenantScope = "global"
	// TenantScopeAllowlist 表示工具只允许在 AllowedTenants 中的租户使用。
	TenantScopeAllowlist TenantScope = "tenant_allowlist"
	// TenantScopeCurrentTenant 表示工具只能访问当前令牌绑定的租户资源。
	TenantScopeCurrentTenant TenantScope = "current_tenant"
)

// ToolDefinition 描述工具的能力、风险和执行约束。
type ToolDefinition struct {
	Name            string
	Version         string
	Description     string
	Permission      Permission
	RiskLevel       RiskLevel
	EffectClass     EffectClass
	Timeout         time.Duration
	ParameterSchema json.RawMessage
	TenantScope     TenantScope
	AllowedTenants  []string
	CapabilityTags  []string
	Deprecated      bool
}

// Validate 检查工具定义是否满足统一契约。
func (d ToolDefinition) Validate() error {
	if strings.TrimSpace(d.Name) == "" ||
		strings.TrimSpace(d.Version) == "" ||
		strings.TrimSpace(d.Description) == "" ||
		strings.TrimSpace(string(d.Permission)) == "" {
		return ErrInvalidDefinition
	}
	if d.Timeout <= 0 || !validRiskLevel(d.RiskLevel) || !validEffectClass(d.EffectClass) ||
		!validTenantScope(d.TenantScope) || !validJSONSchema(d.ParameterSchema) {
		return ErrInvalidDefinition
	}
	if d.TenantScope == TenantScopeAllowlist && len(cleanStrings(d.AllowedTenants)) == 0 {
		return ErrInvalidDefinition
	}
	if d.TenantScope != TenantScopeAllowlist && len(d.AllowedTenants) > 0 {
		return ErrInvalidDefinition
	}
	return nil
}

func validRiskLevel(level RiskLevel) bool {
	switch level {
	case RiskLow, RiskMedium, RiskHigh:
		return true
	default:
		return false
	}
}

func validEffectClass(effect EffectClass) bool {
	switch effect {
	case EffectRead, EffectIdempotentWrite, EffectNonRetryableWrite:
		return true
	default:
		return false
	}
}

func validTenantScope(scope TenantScope) bool {
	switch scope {
	case TenantScopeGlobal, TenantScopeAllowlist, TenantScopeCurrentTenant:
		return true
	default:
		return false
	}
}

// validJSONSchema 只验证参数 Schema 是合法 JSON 对象。
//
// Schema 的具体兼容性检查可以在工具注册中心阶段统一编译；本日先保证工具
// 契约不会携带空值、数组、字符串或损坏的 JSON。
func validJSONSchema(raw json.RawMessage) bool {
	_, err := compileJSONSchema(raw)
	return err == nil
}

// compileJSONSchema 将工具参数 Schema 编译成可执行的校验器。
func compileJSONSchema(raw json.RawMessage) (*jsonschema.Schema, error) {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	if len(raw) == 0 || !json.Valid(raw) {
		return nil, ErrInvalidDefinition
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, ErrInvalidDefinition
	}
	const schemaURL = "inmemory://tooling/parameters.json"
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(schemaURL, document); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidDefinition, err)
	}
	return compiler.Compile(schemaURL)
}

// validateArguments 使用工具定义中的 Schema 校验本次调用参数。
func validateArguments(schema json.RawMessage, arguments json.RawMessage) error {
	validator, err := compileJSONSchema(schema)
	if err != nil {
		return ErrInvalidDefinition
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(arguments))
	if err != nil {
		return ErrArgumentsInvalid
	}
	if err := validator.Validate(instance); err != nil {
		return ErrArgumentsInvalid
	}
	return nil
}

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

func cleanStrings(values []string) []string {
	cleaned := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			cleaned = append(cleaned, value)
		}
	}
	return cleaned
}
