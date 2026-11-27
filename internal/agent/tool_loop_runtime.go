package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"agent-project/internal/configregistry"
	"agent-project/internal/modelgateway"
	"agent-project/internal/recording"
	"agent-project/internal/rollout"
	"agent-project/internal/runstore"
	"agent-project/internal/structuredoutput"
	"agent-project/internal/tool/runner"
	"agent-project/internal/tool/tooling"
)

// ToolCallRecord 表示一次已经进入工具执行边界的调用摘要。
//
// 这里只保留工具名称、版本、结果状态、错误分类和参数摘要，
// 不保存完整参数、Prompt、模型原文或 CapabilityToken。
type ToolCallRecord struct {
	ToolName        string
	ToolVersion     string
	Status          tooling.ResultStatus
	ErrorCode       string
	ArgumentsDigest string
}

// ToolLoopOutput 是受控多轮工具循环的最终输出。
//
// 它嵌入现有 RunOutput，复用发布版本、模型响应和结构化结果；
// 额外记录工具调用摘要与工具决策轮次，普通 Runtime 不需要感知这些字段。
type ToolLoopOutput struct {
	RunOutput
	ToolCalls          []ToolCallRecord
	ToolDecisionRounds int
}

// ToolCallAuthorizer 是可信调用方提供的工具授权回调。
//
// 回调接收模型的工具意图和本次 Run 上下文，负责补充显式工具版本、
// 资源范围、CapabilityToken 和幂等键，返回给 runner.Executor 执行。
// 模型输出本身不能通过这个回调获得授权。
type ToolCallAuthorizer func(
	context.Context,
	RunInput,
	structuredoutput.ToolDecision,
) (runner.ToolCallRequest, error)

// ContextPolicy 定义 ToolLoop 构建模型 Prompt 时使用的可信上下文限制。
//
// 零值字段使用安全默认值；策略只控制模型输入中的工具历史投影，不修改
// 原始工具历史、配置快照、工具执行结果或 RunStore 中的事实记录。
type ContextPolicy struct {
	MaxHistoryTokens    int // 可裁剪工具历史的目标 Token 上限，0 使用默认值。
	MaxToolResultTokens int // 单个工具 Data 的压缩目标，0 使用默认值。
	KeepLatestResults   int // 优先保留的最新工具结果数量，0 使用默认值。
}

// ToolLoopConfig 是受控多轮工具 Runtime 的依赖配置。
//
// Registry、Selector 和 Model 负责配置版本选择与模型调用；Executor
// 负责工具发现和执行；Authorizer 负责注入模型之外的可信授权上下文。
type ToolLoopConfig struct {
	Registry       *configregistry.Registry
	Selector       *rollout.Selector
	Model          ModelCaller
	BudgetResolver BudgetResolver // 解析可信三级预算，供工具循环创建独立执行控制器。
	Executor       *runner.Executor
	Authorizer     ToolCallAuthorizer
	Observer       Observer
	RunStore       runstore.Store
	OnStoreError   RunStoreErrorHandler
	ContextPolicy  ContextPolicy // 配置工具历史的模型输入投影限制，零值使用安全默认值。

	MaxToolCalls int

	AnalysisRepair     structuredoutput.RepairFunc
	ToolDecisionRepair structuredoutput.RepairFunc
	FinalAnswerRepair  structuredoutput.RepairFunc
}

var (
	// ErrToolLoopNotConfigured 表示工具循环缺少必要依赖。
	ErrToolLoopNotConfigured = errors.New("agent tool loop is not configured")
	// ErrToolDecisionDecodeFailed 表示模型返回的工具决策无法通过结构化校验。
	ErrToolDecisionDecodeFailed = errors.New("agent tool decision decode failed")
	// ErrToolDecisionModelFailed 表示工具决策阶段的模型调用失败。
	ErrToolDecisionModelFailed = errors.New("agent tool decision model failed")
	// ErrToolAuthorizationFailed 表示可信授权回调拒绝或无法补充执行上下文。
	ErrToolAuthorizationFailed = errors.New("agent tool authorization failed")
	// ErrToolExecutionFailed 表示工具执行阶段返回错误。
	ErrToolExecutionFailed = errors.New("agent tool execution failed")
	// ErrMaxToolCallsExceeded 表示一次 Run 超过了允许的工具调用次数。
	ErrMaxToolCallsExceeded = errors.New("agent maximum tool calls exceeded")
	// ErrDuplicateToolCall 表示检测到相同工具、版本和参数的重复调用。
	ErrDuplicateToolCall = errors.New("agent duplicate tool call")
	// ErrLoopDetected 表示非连续的规范化决策状态再次出现，工具循环没有产生新进展。
	ErrLoopDetected = errors.New("agent no-progress loop detected")
	// ErrToolHistoryEncodingFailed 表示工具历史无法编码为下一轮模型输入。
	ErrToolHistoryEncodingFailed = errors.New("agent tool history encoding failed")
	// ErrInvalidContextPolicy 表示上下文策略包含负数或其他不支持的限制。
	ErrInvalidContextPolicy = errors.New("agent invalid context policy")
)

const (
	// defaultMaxToolCalls 限制单次 Run 的工具调用次数，防止模型形成无限循环。
	defaultMaxToolCalls = 3
	// contextPolicyVersion 标识本地工具历史投影算法的稳定版本。
	contextPolicyVersion = "context.v1"
	// defaultMaxHistoryTokens 限制可裁剪工具历史进入 Prompt 的目标规模。
	defaultMaxHistoryTokens = 12000
	// defaultMaxToolResultTokens 限制单个工具结果的压缩目标规模。
	defaultMaxToolResultTokens = 2560
	// defaultKeepLatestResults 优先保留最新工具结果，避免旧历史挤掉近期证据。
	defaultKeepLatestResults = 10
)

// ToolLoopRuntime 负责执行受控的多轮工具调用。
//
// 它与现有 Runtime 分开，避免动态次数的工具循环改变固定的两阶段模型链。
// Runtime 内部由两条职责独立的分支组成：
//
//   - should_call=true：模型给出工具意图，经过授权、去重和次数限制后，
//     进入 runner.Executor，再由 Executor 经过 Registry 和 tooling.Invoke 执行工具；
//   - should_call=false：不执行工具，直接使用 ModelCaller 调用最终回答模型，
//     再解析成 FinalAnswer。
//
// 因此 ModelCaller 和 runner.Executor 不是互相调用的关系，而是由 Runtime
// 根据 ToolDecision 选择和编排的两个独立依赖。
type ToolLoopRuntime struct {
	// Registry 和 Selector 决定本次 Run 使用的版本快照。
	registry *configregistry.Registry
	selector *rollout.Selector
	// Model 负责分析、工具决策和最终回答阶段的供应商无关模型调用。
	model          ModelCaller
	budgetResolver BudgetResolver // 保存可信预算解析依赖，供每次工具循环创建独立控制器。
	// Executor 只服务 should_call=true 的工具分支；Runtime 不直接调用工具。
	executor *runner.Executor
	// Authorizer 负责把模型意图补充成可信的 ToolCallRequest。
	authorizer ToolCallAuthorizer
	// Observer 只接收脱敏运行元数据，不接收 Prompt 或模型原文。
	observer Observer
	// runStore 是可选的 Run 记录存储；未配置时保持原有内存观测行为。
	runStore runstore.Store
	// onStoreError 接收旁路持久化失败，不改变模型和工具主流程。
	onStoreError RunStoreErrorHandler
	// maxCalls 用于限制后续动态工具循环的最大调用次数。
	maxCalls int
	// contextPolicy 在构造时完成校验和默认值填充，之后对所有并发 Run 只读。
	contextPolicy ContextPolicy

	analysisRepair     structuredoutput.RepairFunc
	toolDecisionRepair structuredoutput.RepairFunc
	finalAnswerRepair  structuredoutput.RepairFunc
}

// toolLoopState 保存一次工具循环在版本绑定阶段产生的中间状态。
//
// 后续阶段会继续向这个状态补充分析结果、工具历史和最终回答；
// snapshot 从绑定完成后固定不变，避免执行过程中读取到新版本配置。
type toolLoopState struct {
	// input 和 execution 贯穿一次 Run，分别保存业务输入和观测状态。
	input     RunInput
	execution *runtimeExecution
	// snapshot 在绑定后固定不变，后续所有 Prompt、Schema 和路由都从这里读取。
	snapshot configregistry.ConfigSnapshot
	// 分析阶段状态。
	analysisPrompt       string
	analysisResponse     modelgateway.ChatResponse
	analysisContentTrust trustLevel // 原始分析和结构化 TaskAnalysis 始终是不可信模型内容。
	analysis             structuredoutput.TaskAnalysis
	analysisReport       structuredoutput.DecodeReport
	// 工具循环历史和当前工具决策阶段状态。
	toolHistory              []toolHistoryEntry
	externalContentSeen      bool // 当前 Run 已成功接收含非空 Data 或 Source 的不可信外部结果。
	toolDecisionPrompt       string
	toolDecisionDigests      []string // 按顺序保存当前 Run 的执行阶段与决策摘要，用于识别重复模型状态。
	toolDecisionResponse     modelgateway.ChatResponse
	toolDecisionContentTrust trustLevel // 原始工具决策及解析后的 ToolDecision 始终是不可信模型意图。
	toolDecision             structuredoutput.ToolDecision
	toolDecisionReport       structuredoutput.DecodeReport
	// 授权成功后保存的请求，供后续 Executor.Execute 使用。
	toolCallRequest     runner.ToolCallRequest
	toolArgumentsDigest string
	// 最终回答阶段状态。
	finalPrompt       string
	finalResponse     modelgateway.ChatResponse
	finalContentTrust trustLevel // 原始最终响应及解析后的 FinalAnswer 始终是不可信模型内容。
	finalAnswer       structuredoutput.FinalAnswer
	finalAnswerReport structuredoutput.DecodeReport
}

// toolHistoryEntry 保存下一轮模型需要的工具执行上下文。
//
// Data 和 Source 是工具返回的结构化结果，供模型继续判断；
// ToolCallRecord 只保留脱敏摘要，最终不会把完整工具数据放入 RunOutput。
type toolHistoryEntry struct {
	Record            ToolCallRecord
	Data              json.RawMessage
	Source            []tooling.SourceRef
	ResultContentKind tooling.ResultContentKind // 来自注册工具定义，而不是工具返回正文。
}

// normalizeContextPolicy 校验调用方策略并填充零值默认值。
//
// 返回的新值只在 Runtime 构造阶段使用；调用方传入的配置不会被原地修改，
// 因此不同 Runtime 可以独立复用同一份配置值。
func normalizeContextPolicy(policy ContextPolicy) (ContextPolicy, error) {
	// 负数无法表达合法的 Token 或保留数量限制，必须在创建 Runtime 时拒绝。
	if policy.MaxHistoryTokens < 0 ||
		policy.MaxToolResultTokens < 0 ||
		policy.KeepLatestResults < 0 {
		return ContextPolicy{}, ErrInvalidContextPolicy
	}
	// 各字段独立填默认值，允许调用方只覆盖其中一部分限制。
	if policy.MaxHistoryTokens == 0 {
		policy.MaxHistoryTokens = defaultMaxHistoryTokens
	}
	if policy.MaxToolResultTokens == 0 {
		policy.MaxToolResultTokens = defaultMaxToolResultTokens
	}
	if policy.KeepLatestResults == 0 {
		policy.KeepLatestResults = defaultKeepLatestResults
	}
	return policy, nil
}

// contextVersion 生成包含算法、发布版本和实际治理参数的稳定标识。
//
// 调用方必须传入已规范化的策略；同一 Run 使用绑定的 ReleaseVersion，
// 因此版本不会受执行过程中其他发布或配置修改影响。
func (p ContextPolicy) contextVersion(releaseVersion string) string {
	return fmt.Sprintf(
		"%s:%s:h%d:r%d:k%d",
		contextPolicyVersion,
		releaseVersion,
		p.MaxHistoryTokens,
		p.MaxToolResultTokens,
		p.KeepLatestResults,
	)
}

// NewToolLoopRuntime 创建受控多轮工具 Runtime。
//
// 工具执行器和授权回调都是必需依赖；没有它们时，Runtime 无法保证模型
// 输出不会绕过工具执行边界。MaxToolCalls 未设置时使用默认值 3；
// ContextPolicy 的零值字段使用安全默认值，负数会使构造失败。
func NewToolLoopRuntime(cfg ToolLoopConfig) (*ToolLoopRuntime, error) {
	// 这些依赖共同组成工具循环的安全边界，缺少任意一项都不能安全运行。
	if cfg.Registry == nil ||
		cfg.Selector == nil ||
		cfg.Model == nil ||
		cfg.Executor == nil ||
		cfg.Authorizer == nil {
		return nil, ErrToolLoopNotConfigured
	}
	// 在 Runtime 可运行前固定上下文策略，避免每次 Run 重复校验或读取可变配置。
	contextPolicy, err := normalizeContextPolicy(cfg.ContextPolicy)
	if err != nil {
		return nil, err
	}
	// 允许调用方不提供观测器；空实现保持主流程不需要判断 nil。
	observer := cfg.Observer
	if observer == nil {
		observer = noopObserver{}
	}
	// 默认限制用于防止后续模型循环在异常情况下无限调用工具。
	maxCalls := cfg.MaxToolCalls
	if maxCalls <= 0 {
		maxCalls = defaultMaxToolCalls
	}
	budgetResolver := cfg.BudgetResolver
	if budgetResolver == nil && cfg.MaxToolCalls > 0 {
		// 旧配置只在未提供三级预算解析器时生效，避免两套预算来源产生歧义。
		budgetResolver = FixedBudgetResolver{Budgets: BudgetSet{
			Run: BudgetLimits{MaxToolCalls: cfg.MaxToolCalls},
		}}
	}
	analysisRepair := cfg.AnalysisRepair
	if analysisRepair != nil {
		original := analysisRepair
		analysisRepair = func(
			ctx context.Context,
			request structuredoutput.RepairRequest,
		) ([]byte, error) {
			return budgetedRepair(executionFromContext(ctx), original)(ctx, request)
		}
	}
	toolDecisionRepair := cfg.ToolDecisionRepair
	if toolDecisionRepair != nil {
		original := toolDecisionRepair
		toolDecisionRepair = func(
			ctx context.Context,
			request structuredoutput.RepairRequest,
		) ([]byte, error) {
			return budgetedRepair(executionFromContext(ctx), original)(ctx, request)
		}
	}
	finalAnswerRepair := cfg.FinalAnswerRepair
	if finalAnswerRepair != nil {
		original := finalAnswerRepair
		finalAnswerRepair = func(
			ctx context.Context,
			request structuredoutput.RepairRequest,
		) ([]byte, error) {
			return budgetedRepair(executionFromContext(ctx), original)(ctx, request)
		}
	}
	return &ToolLoopRuntime{
		registry:           cfg.Registry,
		selector:           cfg.Selector,
		model:              cfg.Model,
		budgetResolver:     budgetResolver,
		executor:           cfg.Executor,
		authorizer:         cfg.Authorizer,
		observer:           observer,
		runStore:           cfg.RunStore,
		onStoreError:       cfg.OnStoreError,
		maxCalls:           maxCalls,
		contextPolicy:      contextPolicy,
		analysisRepair:     analysisRepair,
		toolDecisionRepair: toolDecisionRepair,
		finalAnswerRepair:  finalAnswerRepair,
	}, nil
}

// Run 执行一次受控多轮工具循环。
//
// 每轮工具决策都必须经过结构化解析、可信授权、只读安全检查、重复检查和次数限制；
// 只有工具结果被追加到历史后，模型才有机会发起下一轮决策。模型决定
// 不再调用工具时，流程转入最终回答阶段。所有成功和失败路径都会提交
// 一次脱敏观测。
func (r *ToolLoopRuntime) Run(
	ctx context.Context,
	input RunInput,
) (ToolLoopOutput, error) {
	if r == nil {
		return ToolLoopOutput{}, ErrToolLoopNotConfigured
	}
	if ctx == nil {
		ctx = context.Background()
	}

	// 执行准备统一负责预算、总超时、观测和 RunStore 记录器，主流程只保留业务编排。
	ctx, cancel, execution, err := r.prepareExecution(ctx, input)
	if err != nil {
		return ToolLoopOutput{}, err
	}
	defer cancel()

	// 选择本次 Run 的发布版本并绑定不可变配置快照；后续 Prompt、Schema
	// 和模型路由都从该快照读取，避免执行过程中配置版本发生漂移。
	state, err := r.selectAndBindSnapshot(ctx, input)
	if err != nil {
		return ToolLoopOutput{}, err
	}
	execution = state.execution

	// 每个阶段失败时保留当前 state 中的 execution，确保已经开始的 Run
	// 仍能记录最终错误状态；各阶段内部返回的零值 state 不会丢失观测上下文。
	// 工具循环先完成任务分析，后续工具决策和最终回答都基于这个结果。
	// 阶段函数返回更新后的 state，因此需要重新赋值保存新增的中间状态。
	state, err = r.runAnalysis(ctx, state)
	if err != nil {
		// 任一阶段失败都立即停止，统一记录失败观测，避免继续使用不完整状态。
		return r.failToolLoop(ctx, input, execution, err)
	}
	// 将用户问题、分析结果和工具历史渲染为下一次模型调用的 Prompt。
	state, err = r.renderToolDecisionPrompt(ctx, state)
	if err != nil {
		// Prompt 渲染失败时不能继续调用模型，统一收口错误和观测。
		return r.failToolLoop(ctx, input, execution, err)
	}
	// 调用模型获取工具调用意图；此时只是产生决策，还没有执行任何工具。
	state, err = r.callToolDecisionModel(ctx, state)
	if err != nil {
		// 模型调用失败时立即结束本次 Run，不伪造工具决策。
		return r.failToolLoop(ctx, input, execution, err)
	}
	// 对模型输出执行 JSON、Schema 和业务语义校验，得到可信的 ToolDecision。
	// 修复逻辑由结构化输出内核控制，最多执行一次。
	state, err = r.decodeToolDecision(ctx, state)
	if err != nil {
		// 决策无法通过校验时不能进入工具执行阶段，统一记录并返回错误。
		return r.failToolLoop(ctx, input, execution, err)
	}

	// 每轮处理一个已通过结构化校验的工具决策，依次经过可信授权、重复检查、
	// 工具执行和下一轮决策。模型返回 should_call=false 时退出循环并进入最终回答；
	// 任一步骤失败都立即收口，确保工具执行不会绕过安全边界或形成失控循环。
	for {
		// 每轮开始时检查取消和截止时间，避免 Run 已终止后继续授权或执行工具。
		if err := ctx.Err(); err != nil {
			return r.failToolLoop(ctx, input, execution, err)
		}
		// 模型明确表示不再调用工具时结束循环，由后续阶段生成最终回答。
		if !state.toolDecision.ShouldCall {
			break
		}

		// 授权回调把模型意图补充为可信执行请求，注入工具版本、资源范围、
		// CapabilityToken 等模型不能自行决定的字段。
		state, err = r.authorizeToolDecision(ctx, state)
		if err != nil {
			return r.failToolLoop(ctx, input, execution, err)
		}
		// 只读预检和 Run 级安全门禁必须先于重复检查、预算记账及任何执行事件。
		state, err = r.checkToolCallSecurity(ctx, state)
		if err != nil {
			return r.failToolLoop(ctx, input, execution, err)
		}
		// 使用工具名称、版本和规范化参数摘要识别重复调用，防止模型原地循环。
		state, err = r.checkDuplicateToolCall(ctx, state)
		if err != nil {
			return r.failToolLoop(ctx, input, execution, err)
		}
		// 只有通过授权、安全门禁和重复检查的请求才能进入 Executor；执行结果会追加到原始工具历史。
		state, err = r.executeTool(ctx, state)
		if err != nil {
			return r.failToolLoop(ctx, input, execution, err)
		}
		// 将最新工具结果重新投影到下一轮决策 Prompt，并调用、解析新的模型决策；
		// 新决策会在下一轮重新经过完整授权和执行边界。
		state, err = r.continueToolDecision(ctx, state)
		if err != nil {
			return r.failToolLoop(ctx, input, execution, err)
		}
	}

	state, err = r.renderToolLoopFinalPrompt(ctx, state)
	if err != nil {
		return r.failToolLoop(ctx, input, execution, err)
	}
	state, err = r.callToolLoopFinalModel(ctx, state)
	if err != nil {
		return r.failToolLoop(ctx, input, execution, err)
	}
	state, err = r.decodeToolLoopFinalAnswer(ctx, state)
	if err != nil {
		return r.failToolLoop(ctx, input, execution, err)
	}
	output, err := r.formatToolLoopOutput(ctx, state)
	if err != nil {
		return r.failToolLoop(ctx, input, execution, err)
	}
	r.observeToolLoopExecution(ctx, input, execution, output, nil)
	return output, nil
}

// failToolLoop 统一收口失败路径，并保证当前 Run 的观测只提交一次。
func (r *ToolLoopRuntime) failToolLoop(
	ctx context.Context,
	input RunInput,
	execution *runtimeExecution,
	err error,
) (ToolLoopOutput, error) {
	// WithTimeoutCause 到期后，模型或工具通常只返回通用 deadline 错误。
	// 在提交观测和 RunStore 终态前恢复结构化预算原因；调用方自身的 deadline
	// 不带 BudgetViolation Cause，因此不会被误写成 Run 总时间超预算。
	if ctx != nil && ctx.Err() == context.DeadlineExceeded {
		if violation, ok := context.Cause(ctx).(BudgetViolation); ok {
			err = runtimeFailure("tool_loop_budget", violation)
		}
	}
	r.observeToolLoopExecution(ctx, input, execution, ToolLoopOutput{}, err)
	return ToolLoopOutput{}, err
}

// selectAndBindSnapshot 选择灰度版本并将本次 Run 绑定到该版本。
//
// Selector 只负责决定版本号，Registry.BindRunToVersion 负责保存不可变快照。
// 两步分离后，后续 Prompt、Schema 和模型路由都从同一份快照读取。
func (r *ToolLoopRuntime) selectAndBindSnapshot(
	ctx context.Context,
	input RunInput,
) (toolLoopState, error) {
	if r == nil || r.registry == nil || r.selector == nil {
		return toolLoopState{}, ErrToolLoopNotConfigured
	}
	if err := ctx.Err(); err != nil {
		return toolLoopState{}, err
	}
	if err := validateRunInput(input); err != nil {
		return toolLoopState{}, err
	}

	execution := executionFromContext(ctx)
	if execution == nil {
		// 正常入口已经在 Run 中创建执行对象；这里的默认对象仅供阶段级测试或
		// 防御性直接调用使用，不能在已有 execution 时覆盖其预算和开始时间。
		started := recording.ClockFromContext(ctx).Now()
		budget, err := newBudgetController(BudgetSet{}, started)
		if err != nil {
			return toolLoopState{}, err
		}
		execution = &runtimeExecution{
			started:  started,
			budget:   budget,
			recorder: &runStoreRecorder{store: r.runStore, onError: r.onStoreError},
			observation: RunObservation{
				RequestID: input.RequestID,
				TenantID:  input.TenantID,
				RunID:     input.RunID,
			},
		}
	}

	// 先由灰度选择器决定发布版本，只得到版本号，不直接读取配置内容。
	decision, err := r.selector.Select(rollout.SelectionInput{
		RequestID: input.RequestID,
		TenantID:  input.TenantID,
	})
	if err != nil {
		return toolLoopState{}, runtimeFailure("tool_loop_select_release", err)
	}
	if err := ctx.Err(); err != nil {
		return toolLoopState{}, err
	}

	// 版本选择完成后立即绑定 Run 快照，保证本次执行不会中途读到新版本配置。
	bound, err := r.registry.BindRunToVersion(input.RunID, decision.ReleaseVersion)
	if err != nil {
		return toolLoopState{}, runtimeFailure("tool_loop_bind_run", err)
	}
	// 复用 Run 入口的 execution，只补充本次绑定的发布版本，避免覆盖预算账本。
	execution.mu.Lock()
	execution.observation.ReleaseVersion = bound.ReleaseVersion
	execution.observation.ContextVersion = r.contextPolicy.contextVersion(bound.ReleaseVersion)
	execution.securityVersion = securityVersion(bound.ReleaseVersion)
	execution.observation.SecurityVersion = execution.securityVersion
	execution.mu.Unlock()
	// 快照绑定成功后登记 run.started，后续 model.call 和工具事件才能
	// 追加到同一条已存在的 Run 记录；存储失败不会影响主流程。
	execution.recorder.recordRunStarted(
		ctx,
		input,
		bound.Snapshot,
		execution.started,
	)
	return toolLoopState{
		input:     input,
		execution: execution,
		snapshot:  bound.Snapshot,
	}, nil
}

// formatToolLoopOutput 将工具循环的中间状态整理成最终脱敏结果。
//
// 工具返回的 Data 和 Source 只用于后续模型决策，不进入最终输出；
// ToolCalls 只保留名称、版本、状态、错误分类和参数摘要，避免把完整
// 工具参数或工具返回内容暴露给上层调用方。
func (r *ToolLoopRuntime) formatToolLoopOutput(
	ctx context.Context,
	state toolLoopState,
) (ToolLoopOutput, error) {
	if err := ctx.Err(); err != nil {
		return ToolLoopOutput{}, err
	}
	if r == nil {
		return ToolLoopOutput{}, ErrToolLoopNotConfigured
	}
	route, err := routeFromSnapshot(state.snapshot, finalTask)
	if err != nil {
		return ToolLoopOutput{}, err
	}

	provider := state.finalResponse.Provider
	if provider == "" {
		provider = route.Provider
	}
	model := state.finalResponse.Model
	if model == "" {
		model = route.Model
	}

	// 复制摘要切片，避免调用方修改返回值后污染本次循环的中间状态。
	toolCalls := make([]ToolCallRecord, len(state.toolHistory))
	for index, entry := range state.toolHistory {
		toolCalls[index] = entry.Record
	}

	// 每次工具执行后都会产生下一轮 ToolDecision；最后一次 should_call=false
	// 进入最终回答。因此成功完成时，决策轮次等于工具调用次数加最终决策轮。
	output := ToolLoopOutput{
		RunOutput: RunOutput{
			RequestID:         state.input.RequestID,
			TenantID:          state.input.TenantID,
			RunID:             state.input.RunID,
			ReleaseVersion:    state.snapshot.ReleaseVersion,
			Provider:          provider,
			Model:             model,
			Analysis:          state.analysis,
			FinalAnswer:       state.finalAnswer,
			AnalysisReport:    state.analysisReport,
			FinalAnswerReport: state.finalAnswerReport,
			Shadow:            state.execution != nil && state.execution.shadow,
		},
		ToolCalls:          toolCalls,
		ToolDecisionRounds: len(toolCalls) + 1,
	}
	return output, nil
}

// observeToolLoopExecution 提交工具循环的最终观测。
//
// 它复用普通 Runtime 的观测实现，确保工具循环同样记录总耗时、模型用量、
// 成本、发布版本和错误分类，并且不会记录 Prompt、模型原文或工具完整数据。
func (r *ToolLoopRuntime) observeToolLoopExecution(
	ctx context.Context,
	input RunInput,
	execution *runtimeExecution,
	output ToolLoopOutput,
	runErr error,
) {
	if r == nil {
		return
	}
	recordExecutionObservation(ctx, r.observer, execution, output.RunOutput, runErr)
	if execution == nil || execution.shadow || execution.recorder == nil {
		return
	}
	// 工具循环与普通 Runtime 共用同一个 RunStore 生命周期：
	// 先提交最终观测，再由 finishRun 写入 run.succeeded、run.failed
	// 或 run.canceled，并更新 Run 的最终状态。
	execution.recorder.finishRun(ctx, input, execution, runErr)
}
