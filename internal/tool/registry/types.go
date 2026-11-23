package registry

import "agent-project/internal/tool/tooling"

// RegisteredTool 表示注册中心保存的一条工具注册记录。
//
// Definition 是注册时捕获的工具定义快照，用于名称、版本、能力标签和
// 废弃状态等发现信息；Tool 是后续交给 tooling.Invoke 的工具实例。注册中心
// 只负责保存和返回它们，不直接调用 Tool.Execute。
type RegisteredTool struct {
	Definition tooling.ToolDefinition
	Tool       tooling.Tool
}
