package tooling

import (
	"strings"
	"time"
)

// CapabilityToken 是一次工具执行授权的声明。
//
// 当前实现只表达绑定关系和确定性校验边界，不是带签名的 JWT，也没有持久化
// nonce 防重放能力。生产实现需要由认证和授权服务签发，并保存防重放状态。
type CapabilityToken struct {
	TokenID       string
	TenantID      string
	RunID         string
	ToolName      string
	ToolVersion   string
	ResourceScope []string
	Permissions   []Permission
	ExpiresAt     time.Time
	Nonce         string
}

// Validate 检查令牌是否包含不可缺失的绑定字段。
func (t CapabilityToken) Validate(now time.Time) error {
	if strings.TrimSpace(t.TokenID) == "" ||
		strings.TrimSpace(t.TenantID) == "" ||
		strings.TrimSpace(t.RunID) == "" ||
		strings.TrimSpace(t.ToolName) == "" ||
		strings.TrimSpace(t.ToolVersion) == "" ||
		strings.TrimSpace(t.Nonce) == "" ||
		t.ExpiresAt.IsZero() {
		return ErrCapabilityTokenInvalid
	}
	if !now.Before(t.ExpiresAt) {
		return ErrCapabilityExpired
	}
	return nil
}

func (t CapabilityToken) hasPermission(permission Permission) bool {
	if strings.TrimSpace(string(permission)) == "" {
		return true
	}
	for _, granted := range t.Permissions {
		if strings.TrimSpace(string(granted)) == string(permission) {
			return true
		}
	}
	return false
}

func (t CapabilityToken) covers(resources []string) bool {
	allowed := make(map[string]struct{}, len(t.ResourceScope))
	for _, resource := range t.ResourceScope {
		resource = strings.TrimSpace(resource)
		if resource != "" {
			allowed[resource] = struct{}{}
		}
	}
	for _, resource := range resources {
		resource = strings.TrimSpace(resource)
		if resource == "" {
			return false
		}
		if _, ok := allowed[resource]; !ok {
			return false
		}
	}
	return true
}
