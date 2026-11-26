package agent

import (
	"context"
	"sync"
	"time"

	"agent-project/internal/configregistry"
	"agent-project/internal/modelgateway"
	"agent-project/internal/rollout"
	"agent-project/internal/runstore"
	"agent-project/internal/structuredoutput"
	"github.com/cloudwego/eino/compose"
)

const (
	// 这些名称同时作为配置快照中的 Prompt、Schema 和模型路由键。
	// 工具循环必须使用同一组稳定键，才能从绑定版本读取完整配置。
	analysisPromptName     = "task_analysis"
	toolDecisionPromptName = "tool_decision"
	finalPromptName        = "final_answer"
	analysisTask           = "task_analysis"
	toolDecisionTask       = "tool_decision"
	finalTask              = "final_answer"
)

// RunInput 是一次 Agent Runtime 执行所需的业务输入。
type RunInput struct {
	RequestID string // 保存一次外部请求的关联标识。
	TenantID  string // 保存当前 Run 所属的租户标识。
	RunID     string // 保存当前执行的唯一标识。
	UserID    string // 保存可信调用方提供的用户身份，用于用户级预算解析。
	Query     string // 保存用户本次提交的研发协作问题。
}

// RunOutput 是 Runtime 完成两阶段模型调用后的稳定业务结果。
type RunOutput struct {
	RequestID         string
	TenantID          string
	RunID             string
	ReleaseVersion    string
	Provider          string
	Model             string
	Analysis          structuredoutput.TaskAnalysis
	FinalAnswer       structuredoutput.FinalAnswer
	AnalysisReport    structuredoutput.DecodeReport
	FinalAnswerReport structuredoutput.DecodeReport
	Shadow            bool
}

// ModelCaller 是 Runtime 依赖的供应商无关模型调用接口。
//
// modelgateway.Router 已经实现这个接口；测试可以注入确定性的 Fake，
// 因此 Runtime 不需要访问真实模型服务。
type ModelCaller interface {
	Chat(context.Context, modelgateway.ChatRequest) (modelgateway.ChatResponse, error)
}

// Observer 接收一次 Run 的元数据观测，不接收完整 Prompt 或模型回答。
type Observer interface {
	Observe(RunObservation)
}

// RunObservation 是一次 Runtime 执行的脱敏观测结果。
type RunObservation struct {
	RequestID        string
	TenantID         string
	RunID            string
	ReleaseVersion   string
	Provider         string
	Model            string
	Duration         time.Duration
	Success          bool
	StructuredValid  bool
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	Cost             float64
	Shadow           bool
	ErrorKind        string
}

// RuntimeConfig 是 Agent Runtime 的构造参数。
type RuntimeConfig struct {
	Registry          *configregistry.Registry
	Selector          *rollout.Selector
	Model             ModelCaller
	BudgetResolver    BudgetResolver // 解析可信用户、租户和 Run 级预算；空值使用安全默认预算。
	Observer          Observer
	RunStore          runstore.Store
	OnStoreError      RunStoreErrorHandler
	AnalysisRepair    structuredoutput.RepairFunc
	FinalAnswerRepair structuredoutput.RepairFunc
}

// Runtime 是串联配置选择、Prompt、模型和结构化输出的 Eino Runnable。
type Runtime struct {
	registry          *configregistry.Registry
	selector          *rollout.Selector
	model             ModelCaller
	budgetResolver    BudgetResolver // 保存可信预算解析依赖，供每次 Run 创建独立控制器。
	observer          Observer
	runStore          runstore.Store
	onStoreError      RunStoreErrorHandler
	analysisRepair    structuredoutput.RepairFunc
	finalAnswerRepair structuredoutput.RepairFunc
	chain             compose.Runnable[RunInput, RunOutput]
}

type runtimeState struct {
	// runtimeState 是 Eino Chain 各阶段之间传递的中间状态。
	// 每个阶段只负责补充自己产生的数据，后续阶段从这里读取前置结果。
	input             RunInput
	execution         *runtimeExecution
	snapshot          configregistry.ConfigSnapshot
	analysisPrompt    string
	finalPrompt       string
	analysisResponse  modelgateway.ChatResponse
	finalResponse     modelgateway.ChatResponse
	analysis          structuredoutput.TaskAnalysis
	finalAnswer       structuredoutput.FinalAnswer
	analysisReport    structuredoutput.DecodeReport
	finalAnswerReport structuredoutput.DecodeReport
}

type runtimeExecution struct {
	// runtimeExecution 保存一次 Run 的生命周期数据。
	// Chain 的中间状态用于业务编排，本结构用于跨阶段共享观测信息。
	mu          sync.Mutex
	started     time.Time
	budget      *BudgetController // 保存当前 Run 独立的预算上限和消耗，不与其他执行共享。
	shadow      bool
	recorder    *runStoreRecorder
	observation RunObservation
	observed    bool
}

type executionContextKey struct{}
type forcedReleaseContextKey struct{}
