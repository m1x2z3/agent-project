package tooling

import "strings"

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

func cleanStrings(values []string) []string {
	cleaned := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			cleaned = append(cleaned, value)
		}
	}
	return cleaned
}
