package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"text/template"
	"time"

	"agent-project/internal/configregistry"
	"agent-project/internal/modelgateway"
	"agent-project/internal/rollout"
	"agent-project/internal/structuredoutput"
	"github.com/cloudwego/eino/compose"
)

var (
	ErrInvalidRunInput      = errors.New("agent run input is invalid")
	ErrRuntimeNotConfigured = errors.New("agent runtime is not configured")
	ErrPromptNotFound       = errors.New("agent runtime prompt not found")
	ErrSchemaNotFound       = errors.New("agent runtime schema not found")
	ErrRouteNotFound        = errors.New("agent runtime route not found")
	ErrPromptRenderFailed   = errors.New("agent runtime prompt render failed")
	ErrAnalysisDecodeFailed = errors.New("agent runtime analysis decode failed")
	ErrFinalDecodeFailed    = errors.New("agent runtime final answer decode failed")
	ErrAnalysisModelFailed  = errors.New("agent runtime analysis model failed")
	ErrFinalModelFailed     = errors.New("agent runtime final model failed")
)

const (
	analysisPromptName = "task_analysis"
	finalPromptName    = "final_answer"
	analysisTask       = "task_analysis"
	finalTask          = "final_answer"
)

// RunInput 是一次 Agent Runtime 执行所需的业务输入。
type RunInput struct {
	RequestID string
	TenantID  string
	RunID     string
	Query     string
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
	Observer          Observer
	AnalysisRepair    structuredoutput.RepairFunc
	FinalAnswerRepair structuredoutput.RepairFunc
}

// Runtime 是串联配置选择、Prompt、模型和结构化输出的 Eino Runnable。
type Runtime struct {
	registry          *configregistry.Registry
	selector          *rollout.Selector
	model             ModelCaller
	observer          Observer
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
	mu           sync.Mutex
	started      time.Time
	shadow       bool
	bindingRunID string
	observation  RunObservation
	observed     bool
}

type executionContextKey struct{}
type forcedReleaseContextKey struct{}

type runtimeError struct {
	// runtimeError 记录失败发生在哪个阶段，同时保留原始错误，
	// 使调用方仍可通过 errors.Is/errors.As 判断取消、超时和具体错误类型。
	stage string
	cause error
}

func (e *runtimeError) Error() string {
	return "agent runtime failed at " + e.stage
}

func (e *runtimeError) Unwrap() error {
	return e.cause
}

func runtimeFailure(stage string, cause error) error {
	if cause == nil {
		return nil
	}
	return &runtimeError{stage: stage, cause: cause}
}

// NewRuntime 构建当天的类型化 Eino Chain。
func NewRuntime(cfg RuntimeConfig) (*Runtime, error) {
	// Runtime 通过依赖注入连接配置注册中心、灰度选择器和模型调用器。
	// 这样业务编排不依赖具体供应商，也便于使用 Fake 进行离线测试。
	if cfg.Registry == nil || cfg.Selector == nil || cfg.Model == nil {
		return nil, ErrRuntimeNotConfigured
	}
	observer := cfg.Observer
	if observer == nil {
		observer = noopObserver{}
	}
	runtime := &Runtime{
		registry:          cfg.Registry,
		selector:          cfg.Selector,
		model:             cfg.Model,
		observer:          observer,
		analysisRepair:    cfg.AnalysisRepair,
		finalAnswerRepair: cfg.FinalAnswerRepair,
	}
	chain, err := runtime.buildChain(context.Background())
	if err != nil {
		return nil, fmt.Errorf("build agent runtime chain: %w", err)
	}
	runtime.chain = chain
	return runtime, nil
}

// Run 执行一次正常请求，灰度选择器决定使用 active 或候选版本。
func (r *Runtime) Run(ctx context.Context, input RunInput) (RunOutput, error) {
	return r.run(ctx, input, false, "")
}

// RunShadow 强制执行候选版本，只进行模型调用和结构化解析。
//
// 当前 Runtime 没有工具或副作用依赖，因此影子运行不会改变主请求结果，
// 也不会执行写操作或访问数据库。
func (r *Runtime) RunShadow(ctx context.Context, input RunInput) (RunOutput, error) {
	return r.run(ctx, input, true, r.shadowBindingID(input.RunID))
}

func (r *Runtime) run(ctx context.Context, input RunInput, shadow bool, bindingRunID string) (RunOutput, error) {
	if r == nil || r.chain == nil {
		return RunOutput{}, ErrRuntimeNotConfigured
	}
	if err := validateRunInput(input); err != nil {
		return RunOutput{}, err
	}
	execution := &runtimeExecution{
		started:      time.Now(),
		shadow:       shadow,
		bindingRunID: bindingRunID,
		observation: RunObservation{
			RequestID: input.RequestID,
			TenantID:  input.TenantID,
			RunID:     input.RunID,
			Shadow:    shadow,
		},
	}
	// 将本次执行状态放入 Context，供 Chain 中的各阶段共享，
	// 同时继续沿用调用方 Context 的取消和超时信号。
	runCtx := context.WithValue(ctx, executionContextKey{}, execution)
	if shadow {
		// 影子运行固定使用候选版本，但不改变 Registry 的 active 指针。
		runCtx = context.WithValue(runCtx, forcedReleaseContextKey{}, r.selectorCandidateVersion())
	}
	output, err := r.chain.Invoke(runCtx, input)
	r.observeExecution(execution, output, err)
	return output, err
}

func (r *Runtime) buildChain(ctx context.Context) (compose.Runnable[RunInput, RunOutput], error) {
	chain := compose.NewChain[RunInput, RunOutput]()
	// 这些阶段存在明确的数据依赖，Append 顺序就是实际执行顺序：
	// 必须先选版本并绑定快照，再渲染 Prompt、调用模型、校验结果，最后组装输出。
	// 例如没有完成 bindRunSnapshot，后续阶段就不能安全读取 Prompt、Schema 和路由版本。
	chain.
		AppendLambda(compose.InvokableLambda(r.selectRelease)).
		AppendLambda(compose.InvokableLambda(r.bindRunSnapshot)).
		AppendLambda(compose.InvokableLambda(r.renderAnalysisPrompt)).
		AppendLambda(compose.InvokableLambda(r.callAnalysisModel)).
		AppendLambda(compose.InvokableLambda(r.decodeAnalysis)).
		AppendLambda(compose.InvokableLambda(r.renderFinalPrompt)).
		AppendLambda(compose.InvokableLambda(r.callFinalModel)).
		AppendLambda(compose.InvokableLambda(r.decodeFinalAnswer)).
		AppendLambda(compose.InvokableLambda(r.formatRunOutput))
	return chain.Compile(ctx)
}

func (r *Runtime) selectRelease(ctx context.Context, input RunInput) (runtimeState, error) {
	if err := ctx.Err(); err != nil {
		return runtimeState{}, err
	}
	execution := executionFromContext(ctx)
	if execution == nil {
		return runtimeState{}, ErrRuntimeNotConfigured
	}
	// 这里只做版本选择，不加载完整配置内容。
	// 完整快照会在下一阶段绑定，从而把本次 Run 使用的配置固定下来。
	decision, err := r.selector.Select(rollout.SelectionInput{
		RequestID: input.RequestID,
		TenantID:  input.TenantID,
	})
	if forced, ok := ctx.Value(forcedReleaseContextKey{}).(string); ok && strings.TrimSpace(forced) != "" {
		decision.ReleaseVersion = forced
		decision.Candidate = true
		decision.Reason = "shadow_candidate"
	}
	if err != nil {
		return runtimeState{}, err
	}
	execution.mu.Lock()
	execution.observation.ReleaseVersion = decision.ReleaseVersion
	execution.mu.Unlock()
	return runtimeState{input: input, execution: execution}, nil
}

func (r *Runtime) bindRunSnapshot(ctx context.Context, state runtimeState) (runtimeState, error) {
	if err := ctx.Err(); err != nil {
		return runtimeState{}, err
	}
	bindingID := state.execution.bindingRunID
	if bindingID == "" {
		bindingID = state.input.RunID
	}
	// 将灰度选择结果转换成不可变的 Run 快照。
	// 后续即使 active 版本发生变化，本次执行仍使用同一份配置。
	snapshot, err := r.registry.BindRunToVersion(bindingID, state.execution.observation.ReleaseVersion)
	if err != nil {
		return runtimeState{}, runtimeFailure("bind_run", err)
	}
	state.snapshot = snapshot.Snapshot
	return state, nil
}

func (r *Runtime) renderAnalysisPrompt(ctx context.Context, state runtimeState) (runtimeState, error) {
	// 分类 Prompt 必须从 Run 快照读取，不能在 Runtime 中硬编码，
	// 这样 Prompt 版本才能与本次 Run 的发布版本保持一致。
	promptVersion, err := promptFromSnapshot(state.snapshot, analysisPromptName)
	if err != nil {
		return runtimeState{}, err
	}
	rendered, err := renderPrompt(promptVersion.Template, map[string]any{"Query": state.input.Query})
	if err != nil {
		return runtimeState{}, err
	}
	state.analysisPrompt = rendered
	return state, nil
}

func (r *Runtime) callAnalysisModel(ctx context.Context, state runtimeState) (runtimeState, error) {
	route, err := routeFromSnapshot(state.snapshot, analysisTask)
	if err != nil {
		return runtimeState{}, err
	}
	r.recordRoute(state.execution, route)
	// Runtime 只依赖 ModelCaller，不感知 DeepSeek、Kimi 或 OpenAI 兼容协议。
	// 供应商选择和底层 HTTP 细节由 modelgateway 负责。
	response, err := r.model.Chat(ctx, modelgateway.ChatRequest{
		Task:     analysisTask,
		Provider: route.Provider,
		Messages: []modelgateway.Message{{Role: "user", Content: state.analysisPrompt}},
	})
	if err != nil {
		return runtimeState{}, runtimeFailure("analysis_model", fmt.Errorf("%w: %w", ErrAnalysisModelFailed, err))
	}
	state.analysisResponse = response
	r.recordModelResponse(state.execution, state.snapshot, response, analysisTask)
	return state, nil
}

func (r *Runtime) decodeAnalysis(ctx context.Context, state runtimeState) (runtimeState, error) {
	references, err := resolveReferences(state.snapshot, analysisPromptName, analysisTask, analysisTask)
	if err != nil {
		return runtimeState{}, err
	}
	metadata := structuredoutput.ExecutionMetadata{
		Provider:           references.Provider,
		Model:              references.Model,
		PromptVersion:      references.PromptVersion,
		SchemaVersion:      references.SchemaVersion,
		ModelConfigVersion: references.ModelConfigVersion,
	}
	// 模型输出必须经过结构化输出内核的 Schema 和业务语义校验；
	// Repair 由调用方注入，并且最多执行一次，避免无界重试。
	value, report, err := structuredoutput.DecodeTaskAnalysis(ctx, []byte(state.analysisResponse.Message.Content), structuredoutput.DecodeOptions{
		Metadata: metadata,
		Repair:   r.analysisRepair,
	})
	if err != nil {
		return runtimeState{}, runtimeFailure("analysis_decode", fmt.Errorf("%w: %w", ErrAnalysisDecodeFailed, err))
	}
	state.analysis = value
	state.analysisReport = report
	return state, nil
}

func (r *Runtime) renderFinalPrompt(ctx context.Context, state runtimeState) (runtimeState, error) {
	// 最终回答阶段同时使用原始问题和已校验的分析结果，
	// 避免把未经校验的模型原文直接拼进下一阶段。
	promptVersion, err := promptFromSnapshot(state.snapshot, finalPromptName)
	if err != nil {
		return runtimeState{}, err
	}
	rendered, err := renderPrompt(promptVersion.Template, map[string]any{
		"Query":    state.input.Query,
		"Analysis": state.analysis,
	})
	if err != nil {
		return runtimeState{}, err
	}
	state.finalPrompt = rendered
	return state, nil
}

func (r *Runtime) callFinalModel(ctx context.Context, state runtimeState) (runtimeState, error) {
	route, err := routeFromSnapshot(state.snapshot, finalTask)
	if err != nil {
		return runtimeState{}, err
	}
	r.recordRoute(state.execution, route)
	// 这是第二次、面向最终回答的模型调用；仍然通过统一 ModelCaller 执行。
	response, err := r.model.Chat(ctx, modelgateway.ChatRequest{
		Task:     finalTask,
		Provider: route.Provider,
		Messages: []modelgateway.Message{{Role: "user", Content: state.finalPrompt}},
	})
	if err != nil {
		return runtimeState{}, runtimeFailure("final_model", fmt.Errorf("%w: %w", ErrFinalModelFailed, err))
	}
	state.finalResponse = response
	r.recordModelResponse(state.execution, state.snapshot, response, finalTask)
	return state, nil
}

func (r *Runtime) decodeFinalAnswer(ctx context.Context, state runtimeState) (runtimeState, error) {
	references, err := resolveReferences(state.snapshot, finalPromptName, finalTask, finalTask)
	if err != nil {
		return runtimeState{}, err
	}
	metadata := structuredoutput.ExecutionMetadata{
		Provider:           references.Provider,
		Model:              references.Model,
		PromptVersion:      references.PromptVersion,
		SchemaVersion:      references.SchemaVersion,
		ModelConfigVersion: references.ModelConfigVersion,
	}
	// 最终回答也必须完成独立的结构化解析，确保 answered/refused 等业务状态合法。
	value, report, err := structuredoutput.DecodeFinalAnswer(ctx, []byte(state.finalResponse.Message.Content), structuredoutput.DecodeOptions{
		Metadata: metadata,
		Repair:   r.finalAnswerRepair,
	})
	if err != nil {
		return runtimeState{}, runtimeFailure("final_decode", fmt.Errorf("%w: %w", ErrFinalDecodeFailed, err))
	}
	state.finalAnswer = value
	state.finalAnswerReport = report
	return state, nil
}

func (r *Runtime) formatRunOutput(ctx context.Context, state runtimeState) (RunOutput, error) {
	if err := ctx.Err(); err != nil {
		return RunOutput{}, err
	}
	route, err := routeFromSnapshot(state.snapshot, finalTask)
	if err != nil {
		return RunOutput{}, err
	}
	provider := state.finalResponse.Provider
	if provider == "" {
		provider = route.Provider
	}
	model := state.finalResponse.Model
	if model == "" {
		model = route.Model
	}
	// 这一阶段只负责把中间状态转换成稳定的业务输出，不再发起模型调用。
	return RunOutput{
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
		Shadow:            state.execution.shadow,
	}, nil
}

func (r *Runtime) observeExecution(execution *runtimeExecution, output RunOutput, runErr error) {
	if execution == nil {
		return
	}
	execution.mu.Lock()
	defer execution.mu.Unlock()
	if execution.observed {
		return
	}
	// 观测只提交一次，避免 Chain 返回错误和上层重复处理时产生重复记录。
	execution.observed = true
	execution.observation.Duration = time.Since(execution.started)
	execution.observation.Success = runErr == nil
	execution.observation.StructuredValid = runErr == nil
	if runErr != nil {
		execution.observation.ErrorKind = classifyRuntimeError(runErr)
	}
	if runErr == nil {
		execution.observation.Provider = output.Provider
		execution.observation.Model = output.Model
		execution.observation.ReleaseVersion = output.ReleaseVersion
	}
	r.observer.Observe(execution.observation)
}

func (r *Runtime) recordModelResponse(
	execution *runtimeExecution,
	snapshot configregistry.ConfigSnapshot,
	response modelgateway.ChatResponse,
	task string,
) {
	if execution == nil {
		return
	}
	// 一次 Runtime 会调用两次模型，这里统一累加 Token 和成本，
	// 观测层只看到汇总后的 Run 级指标，不接触 Prompt 或模型原文。
	execution.mu.Lock()
	defer execution.mu.Unlock()
	if response.Usage != nil {
		execution.observation.PromptTokens += response.Usage.PromptTokens
		execution.observation.CompletionTokens += response.Usage.CompletionTokens
		execution.observation.TotalTokens += response.Usage.TotalTokens
	}
	route, ok := snapshot.Routes[task]
	if !ok {
		return
	}
	modelName := response.Model
	if modelName == "" {
		modelName = route.Model
	}
	execution.observation.Cost += estimateCost(snapshot, route.Provider, modelName, response.Usage)
}

func (r *Runtime) recordRoute(execution *runtimeExecution, route configregistry.ModelRouteVersion) {
	if execution == nil {
		return
	}
	execution.mu.Lock()
	defer execution.mu.Unlock()
	execution.observation.Provider = route.Provider
	execution.observation.Model = route.Model
}

func estimateCost(
	snapshot configregistry.ConfigSnapshot,
	provider string,
	model string,
	usage *modelgateway.Usage,
) float64 {
	if usage == nil {
		return 0
	}
	var pricing configregistry.PricingVersion
	var found bool
	for key, candidate := range snapshot.Pricing {
		if key == model || (candidate.Provider == provider && candidate.Model == model) {
			pricing = candidate
			found = true
			break
		}
	}
	if !found {
		return 0
	}
	// 价格来自本次 Run 的配置快照，避免运行中切换配置导致成本计算前后不一致。
	return float64(usage.PromptTokens)/1000*pricing.InputPer1K +
		float64(usage.CompletionTokens)/1000*pricing.OutputPer1K
}

func classifyRuntimeError(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, ErrAnalysisModelFailed):
		return "analysis_model_error"
	case errors.Is(err, ErrFinalModelFailed):
		return "final_model_error"
	case errors.Is(err, ErrAnalysisDecodeFailed):
		return "analysis_decode_error"
	case errors.Is(err, ErrFinalDecodeFailed):
		return "final_decode_error"
	default:
		return "runtime_error"
	}
}

func validateRunInput(input RunInput) error {
	if strings.TrimSpace(input.RequestID) == "" ||
		strings.TrimSpace(input.RunID) == "" ||
		strings.TrimSpace(input.Query) == "" {
		return ErrInvalidRunInput
	}
	return nil
}

func promptFromSnapshot(snapshot configregistry.ConfigSnapshot, name string) (configregistry.PromptVersion, error) {
	prompt, ok := snapshot.Prompts[name]
	if !ok || strings.TrimSpace(prompt.Template) == "" || strings.TrimSpace(prompt.Version) == "" {
		return configregistry.PromptVersion{}, ErrPromptNotFound
	}
	return prompt, nil
}

func routeFromSnapshot(snapshot configregistry.ConfigSnapshot, task string) (configregistry.ModelRouteVersion, error) {
	// 路由也必须从绑定快照读取，确保任务使用的 Provider、Model 和参数可追溯。
	route, ok := snapshot.Routes[task]
	if !ok || strings.TrimSpace(route.Provider) == "" || strings.TrimSpace(route.Model) == "" {
		return configregistry.ModelRouteVersion{}, ErrRouteNotFound
	}
	return route, nil
}

func schemaFromSnapshot(snapshot configregistry.ConfigSnapshot, kind string) (configregistry.SchemaVersion, error) {
	schema, ok := snapshot.Schemas[kind]
	if !ok || strings.TrimSpace(schema.Kind) == "" || strings.TrimSpace(schema.Version) == "" {
		return configregistry.SchemaVersion{}, ErrSchemaNotFound
	}
	return schema, nil
}

func resolveReferences(
	snapshot configregistry.ConfigSnapshot,
	promptName string,
	schemaKind string,
	task string,
) (configregistry.VersionReferences, error) {
	// 先分别确认 Prompt、Schema、Route 存在，再统一生成版本元数据，
	// 让结构化输出报告能够记录本次解析使用的完整版本组合。
	if _, err := promptFromSnapshot(snapshot, promptName); err != nil {
		return configregistry.VersionReferences{}, err
	}
	if _, err := schemaFromSnapshot(snapshot, schemaKind); err != nil {
		return configregistry.VersionReferences{}, err
	}
	if _, err := routeFromSnapshot(snapshot, task); err != nil {
		return configregistry.VersionReferences{}, err
	}
	return snapshot.ResolveVersionReferences(promptName, schemaKind, task)
}

func renderPrompt(source string, data map[string]any) (string, error) {
	// missingkey=error 会把模板变量拼写错误或配置遗漏立即变成错误，
	// 避免模型收到缺少上下文的半成品 Prompt。
	tmpl, err := template.New("runtime-prompt").Option("missingkey=error").Parse(source)
	if err != nil {
		return "", fmt.Errorf("%w: template parse failed", ErrPromptRenderFailed)
	}
	var output bytes.Buffer
	if err := tmpl.Execute(&output, data); err != nil {
		return "", fmt.Errorf("%w: template execution failed", ErrPromptRenderFailed)
	}
	if strings.TrimSpace(output.String()) == "" {
		return "", fmt.Errorf("%w: rendered prompt is empty", ErrPromptRenderFailed)
	}
	return output.String(), nil
}

func executionFromContext(ctx context.Context) *runtimeExecution {
	execution, _ := ctx.Value(executionContextKey{}).(*runtimeExecution)
	return execution
}

func (r *Runtime) selectorCandidateVersion() string {
	if r == nil || r.selector == nil {
		return ""
	}
	return r.selector.Policy().CandidateVersion
}

func (r *Runtime) shadowBindingID(runID string) string {
	return fmt.Sprintf("%s:shadow:%d", runID, time.Now().UnixNano())
}

type noopObserver struct{}

func (noopObserver) Observe(RunObservation) {}
