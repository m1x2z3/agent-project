package configregistry

import "time"

// PromptVersion 表示一个可发布的 Prompt 版本。
type PromptVersion struct {
	Name     string
	Version  string
	Template string
}

// SchemaVersion 表示一个结构化输出 Schema 版本。
type SchemaVersion struct {
	Kind    string
	Version string
}

// ModelRouteVersion 表示一个任务到模型供应商的版本化路由。
//
// 这里仅保存运行参数，不保存 API Key 等敏感凭据。
type ModelRouteVersion struct {
	Task        string
	Version     string
	Provider    string
	Model       string
	Temperature float64
	MaxTokens   int
	Timeout     time.Duration
}

// PricingVersion 表示一个模型价格配置版本。
type PricingVersion struct {
	Provider    string
	Model       string
	Version     string
	Currency    string
	InputPer1K  float64
	OutputPer1K float64
}

// ConfigSnapshot 是一次完整发布对应的不可变配置快照。
type ConfigSnapshot struct {
	ReleaseVersion string
	Prompts        map[string]PromptVersion
	Schemas        map[string]SchemaVersion
	Routes         map[string]ModelRouteVersion
	Pricing        map[string]PricingVersion
	Metadata       ReleaseMetadata
}

// ReleaseMetadata 记录发布、激活和回滚关系。
type ReleaseMetadata struct {
	Version         string
	PublishedBy     string
	PublishedAt     time.Time
	EffectiveAt     time.Time
	PreviousVersion string
	RollbackFrom    string
}

// RunSnapshot 表示 Run 创建时绑定的配置快照。
type RunSnapshot struct {
	RunID          string
	ReleaseVersion string
	Snapshot       ConfigSnapshot
}

// VersionReferences 是将配置快照映射到结构化输出解析所需版本的最小信息。
//
// 调用方可以使用这些字段构造 structuredoutput.ExecutionMetadata，
// configregistry 不直接依赖 structuredoutput，避免控制面和执行内核互相耦合。
type VersionReferences struct {
	Provider           string
	Model              string
	PromptVersion      string
	SchemaVersion      string
	ModelConfigVersion string
}
