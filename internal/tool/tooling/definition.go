package tooling

import (
	"encoding/json"
	"strings"
	"time"
)

// ResultContentKind 是可信工具定义声明的结果内容来源类别。
//
// 它只描述工具结果的数据来源，不表示内容可信，也不允许工具结果正文自行声明。
// Agent 会把这个受限类型显式转换为完整的模型输入内容类别。
type ResultContentKind string

const (
	// ResultContentToolResult 表示没有更具体来源声明的通用工具结果，同时是零值的归一结果。
	ResultContentToolResult ResultContentKind = "tool_result"
	// ResultContentDocument 表示研发文档等文档型工具结果。
	ResultContentDocument ResultContentKind = "document"
	// ResultContentLog 表示日志型工具结果。
	ResultContentLog ResultContentKind = "log"
	// ResultContentIssue 表示 Issue 型工具结果。
	ResultContentIssue ResultContentKind = "issue"
)

// Normalized 把兼容旧工具定义的零值统一归一为通用工具结果。
// 其他值保持原样，由 ToolDefinition.Validate 拒绝未知或伪造的类别。
func (k ResultContentKind) Normalized() ResultContentKind {
	if k == "" {
		return ResultContentToolResult
	}
	return k
}

// ToolDefinition 描述工具的能力、风险和执行约束。
type ToolDefinition struct {
	Name              string
	Version           string
	Description       string
	Permission        Permission
	RiskLevel         RiskLevel
	EffectClass       EffectClass
	ResultContentKind ResultContentKind
	Timeout           time.Duration
	ParameterSchema   json.RawMessage
	TenantScope       TenantScope
	AllowedTenants    []string
	CapabilityTags    []string
	Deprecated        bool
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
		!validResultContentKind(d.ResultContentKind.Normalized()) ||
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

func validResultContentKind(kind ResultContentKind) bool {
	switch kind {
	case ResultContentToolResult, ResultContentDocument, ResultContentLog, ResultContentIssue:
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

func cleanStrings(values []string) []string {
	cleaned := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			cleaned = append(cleaned, value)
		}
	}
	return cleaned
}
