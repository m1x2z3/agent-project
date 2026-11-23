package registry

import (
	"encoding/json"

	"agent-project/internal/tool/tooling"
)

// cloneToolDefinition 复制工具定义中的所有可变字段。
//
// 注册中心会保存工具定义快照。ParameterSchema、AllowedTenants 和
// CapabilityTags 都是可变的切片，必须复制底层数据，避免调用方在注册后
// 修改原对象时污染注册中心内部状态。
func cloneToolDefinition(definition tooling.ToolDefinition) tooling.ToolDefinition {
	definition.ParameterSchema = cloneRawMessage(definition.ParameterSchema)
	definition.AllowedTenants = cloneStrings(definition.AllowedTenants)
	definition.CapabilityTags = cloneStrings(definition.CapabilityTags)
	return definition
}

func cloneRawMessage(raw json.RawMessage) json.RawMessage {
	if raw == nil {
		return nil
	}
	cloned := make(json.RawMessage, len(raw))
	copy(cloned, raw)
	return cloned
}

func cloneStrings(values []string) []string {
	if values == nil {
		return nil
	}
	cloned := make([]string, len(values))
	copy(cloned, values)
	return cloned
}
