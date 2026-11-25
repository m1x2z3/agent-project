package runstore

import "time"

// RunStatus 表示一次 Run 当前所处的生命周期状态。
type RunStatus string

const (
	// RunRunning 表示 Run 已创建，仍在执行过程中。
	RunRunning RunStatus = "running"
	// RunSucceeded 表示 Run 已成功完成。
	RunSucceeded RunStatus = "succeeded"
	// RunFailed 表示 Run 因错误结束。
	RunFailed RunStatus = "failed"
	// RunCanceled 表示 Run 因 Context 取消或主动取消结束。
	RunCanceled RunStatus = "canceled"
)

// ConfigReferences 记录一次 Run 使用的配置版本引用。
//
// 这里只保存版本号，不保存 Prompt 模板、完整 Schema、API Key 或其他
// 敏感配置内容。后续需要恢复配置时，可以根据 ReleaseVersion 从配置
// 注册中心查询对应的不可变快照。
type ConfigReferences struct {
	ReleaseVersion  string
	PromptVersions  map[string]string
	SchemaVersions  map[string]string
	RouteVersions   map[string]string
	PricingVersions map[string]string
}

// RunObservation 保存一次 Run 的脱敏运行观测。
//
// 该结构只记录模型调用的元数据和统计结果，不保存完整 Prompt、模型回答、
// 工具参数、工具返回数据或 API Key。
type RunObservation struct {
	Provider         string
	Model            string
	Duration         time.Duration
	Success          bool
	StructuredValid  bool
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	Cost             float64
	ErrorKind        string
}

// RunRecord 是一次 Agent Run 的持久化领域记录。
//
// 它把 Run 身份、生命周期状态、配置版本引用和脱敏观测放在一起，
// 但不保存完整 Prompt、模型回答或工具执行原文。
type RunRecord struct {
	RunID       string
	RequestID   string
	TenantID    string
	Status      RunStatus
	Config      ConfigReferences
	StartedAt   time.Time
	FinishedAt  time.Time
	Observation RunObservation
}
