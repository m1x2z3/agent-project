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
	RunFailed         RunStatus = "failed"
	RunBudgetExceeded RunStatus = "budget_exceeded" // 表示 Run 因任一预算超限而确定性终止。
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
	Provider              string
	Model                 string
	Duration              time.Duration
	Success               bool
	StructuredValid       bool
	PromptTokens          int
	CompletionTokens      int
	TotalTokens           int
	Cost                  float64
	ErrorKind             string
	SecurityVersion       string // 保存本次 Run 固定的安全算法及发布版本。
	SecurityChecks        int    // 累计已完成的 Query、工具结果和执行前安全检查。
	SecurityBlocks        int    // 累计因安全策略终止当前 Run 的次数。
	PromptInjectionBlocks int    // 累计 Prompt Injection 拦截次数。
	DangerousToolBlocks   int    // 累计危险能力或未审批高风险操作拦截次数。
	DataFlowBlocks        int    // 累计不可信外部内容流向非只读工具的拦截次数。
	MaxSteps              int    // 保存本次 Run 合并后的最大执行步数。
	MaxToolCalls          int    // 保存本次 Run 合并后的最大工具调用次数。
	MaxTotalTokens        int64  // 保存本次 Run 合并后的预算 Token 上限。
	MaxDurationMS         int64  // 保存本次 Run 合并后的总时间上限，统一使用毫秒。
	Steps                 int64  // 保存已经进入执行边界的模型、工具和修复步数。
	ToolCalls             int64  // 保存已经进入执行边界的工具调用次数。
	BudgetTokens          int64  // 保存预算账本累计 Token，包含真实 Usage 和本地估算。
	EstimatedTokens       int64  // 保存预算 Token 中来自本地估算的部分，不代表供应商真实用量。
	StepsSource           string // 保存最大步数限制的来源作用域，不包含用户或租户额外信息。
	ToolCallsSource       string // 保存工具调用次数限制的来源作用域。
	TokensSource          string // 保存预算 Token 上限的来源作用域。
	DurationSource        string // 保存总时间预算限制的来源作用域。
	ContextVersion        string // 保存本次 Run 固定的上下文版本，不包含 Prompt 或工具结果。
	ContextBuilds         int    // 累计已治理的工具决策和最终回答 Prompt 数量。
	ContextTrimmed        int    // 累计实际发生压缩、降级或省略的 Prompt 数量。
	ContextTokensBefore   int    // 累计未治理基线 Prompt 的本地 Token 估算。
	ContextTokensAfter    int    // 累计实际发送给模型的治理后 Prompt 本地 Token 估算。
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
