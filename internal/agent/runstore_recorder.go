package agent

import (
	"context"
	"errors"
	"time"

	"agent-project/internal/configregistry"
	"agent-project/internal/modelgateway"
	"agent-project/internal/runstore"
)

// runStoreRecorder 是 Agent Runtime 与 RunStore 之间的记录适配器。
//
// Runtime 只需要把运行生命周期信息交给这个适配器；适配器后续会负责
// 调用 runstore.Store 保存 Run 和事件，并在保存失败时通过 onError 通知
// 观测层。这样普通 Runtime 和 ToolLoopRuntime 可以共用同一套持久化边界，
// 而不需要直接处理 Store 的错误分类细节。
//
// store 为 nil 时表示未启用 RunStore，Runtime 应保持原有执行行为；
// onError 为 nil 时表示调用方不需要接收持久化失败通知。
type runStoreRecorder struct {
	store   runstore.Store
	onError RunStoreErrorHandler
}

// recordRunStarted 将一次 Runtime 执行登记到 RunStore。
//
// 它先创建一条 running 状态的 RunRecord，再追加 run.started 事件；
// 只有 RunRecord 创建成功后才会追加事件，避免事件指向不存在的 Run。
// 两次写入都属于旁路持久化：失败时只通知 onError，不把存储错误返回给
// Runtime 主流程，也不把底层错误原文暴露出去。
func (r *runStoreRecorder) recordRunStarted(
	ctx context.Context,
	input RunInput,
	snapshot configregistry.ConfigSnapshot,
	startedAt time.Time,
) {
	if r == nil || r.store == nil {
		return
	}
	storeCtx := detachedStoreContext(ctx)
	startedAt = startedAt.UTC()
	record := runstore.RunRecord{
		RunID:     input.RunID,
		RequestID: input.RequestID,
		TenantID:  input.TenantID,
		Status:    runstore.RunRunning,
		Config:    configReferencesFromSnapshot(snapshot),
		StartedAt: startedAt,
	}
	if err := r.store.CreateRun(storeCtx, record); err != nil {
		r.notifyStoreFailure(input, "create_run", err)
		return
	}

	event := runstore.RunEvent{
		RunID:      input.RunID,
		RequestID:  input.RequestID,
		TenantID:   input.TenantID,
		Type:       runstore.EventRunStarted,
		OccurredAt: startedAt,
		Status:     string(runstore.RunRunning),
	}
	if err := r.store.AppendEvent(storeCtx, event); err != nil {
		r.notifyStoreFailure(input, "append_event", err)
	}
}

// notifyStoreFailure 将 Store 错误转换为稳定分类后通知调用方。
//
// 适配器只传递 Run 身份、操作名称和错误分类，不传递底层错误文本，
// 从而避免敏感配置或供应商错误通过持久化失败回调泄露。
func (r *runStoreRecorder) notifyStoreFailure(
	input RunInput,
	operation string,
	err error,
) {
	if r == nil || r.onError == nil || err == nil {
		return
	}
	r.onError(RunStoreFailure{
		RunID:     input.RunID,
		RequestID: input.RequestID,
		TenantID:  input.TenantID,
		Operation: operation,
		ErrorKind: classifyRunStoreError(err),
	})
}

// recordModelCall 将一次模型调用转换为脱敏的 model.call 事件并写入 RunStore。
//
// 成功调用只保存 Provider、Model、耗时和 success 状态；失败调用只保存
// canceled、timeout 或 model_error 等稳定分类。response 中的消息内容、
// 请求中的 Prompt 和底层供应商错误原文都不会进入事件。
func (r *runStoreRecorder) recordModelCall(
	ctx context.Context,
	input RunInput,
	route configregistry.ModelRouteVersion,
	response modelgateway.ChatResponse,
	duration time.Duration,
	callErr error,
) {
	if r == nil || r.store == nil {
		return
	}
	storeCtx := detachedStoreContext(ctx)
	status, errorKind := classifyModelCall(callErr)
	provider := response.Provider
	if provider == "" {
		provider = route.Provider
	}
	model := response.Model
	if model == "" {
		model = route.Model
	}
	event := runstore.RunEvent{
		RunID:      input.RunID,
		RequestID:  input.RequestID,
		TenantID:   input.TenantID,
		Type:       runstore.EventModelCall,
		OccurredAt: time.Now().UTC(),
		Provider:   provider,
		Model:      model,
		Status:     status,
		DurationMS: duration.Milliseconds(),
		ErrorKind:  errorKind,
	}
	if err := r.store.AppendEvent(storeCtx, event); err != nil {
		r.notifyStoreFailure(input, "append_model_call", err)
	}
}

// recordToolCall 记录一次已经通过授权、即将进入 Executor 的工具调用。
//
// 这里只保存工具身份和参数摘要；完整参数、CapabilityToken 和模型原文
// 都不进入 RunStore。调用方应在 Executor.Execute 前调用本方法，确保
// tool.call 表示“进入执行边界”，而不是仅表示模型产生了调用意图。
func (r *runStoreRecorder) recordToolCall(
	ctx context.Context,
	input RunInput,
	toolName string,
	toolVersion string,
	argumentsDigest string,
) {
	if r == nil || r.store == nil {
		return
	}
	event := runstore.RunEvent{
		RunID:           input.RunID,
		RequestID:       input.RequestID,
		TenantID:        input.TenantID,
		Type:            runstore.EventToolCall,
		OccurredAt:      time.Now().UTC(),
		ToolName:        toolName,
		ToolVersion:     toolVersion,
		Status:          "started",
		ArgumentsDigest: argumentsDigest,
	}
	if err := r.store.AppendEvent(detachedStoreContext(ctx), event); err != nil {
		r.notifyStoreFailure(input, "append_tool_call", err)
	}
}

// recordToolResult 记录 Executor 返回后的工具结果。
//
// resultStatus、errorKind 和 resultDigest 都由调用方提前转换为稳定的
// 脱敏值；方法不会接收或保存完整工具结果、参数和底层错误文本。
// 普通 Runtime 没有 Executor 和工具执行阶段，因此不需要调用这个方法；
// ToolLoopRuntime 才会在 tool.call 之后记录 tool.result。这里记录的是
// 单次工具执行结果，不是整个 Run 的最终状态；Run 的 succeeded、failed
// 或 canceled 仍由 finishRun 负责收口。
func (r *runStoreRecorder) recordToolResult(
	ctx context.Context,
	input RunInput,
	toolName string,
	toolVersion string,
	status string,
	errorKind string,
	argumentsDigest string,
	resultDigest string,
	duration time.Duration,
) {
	if r == nil || r.store == nil {
		return
	}
	event := runstore.RunEvent{
		RunID:           input.RunID,
		RequestID:       input.RequestID,
		TenantID:        input.TenantID,
		Type:            runstore.EventToolResult,
		OccurredAt:      time.Now().UTC(),
		ToolName:        toolName,
		ToolVersion:     toolVersion,
		Status:          status,
		ErrorKind:       errorKind,
		DurationMS:      duration.Milliseconds(),
		ArgumentsDigest: argumentsDigest,
		ResultDigest:    resultDigest,
	}
	if err := r.store.AppendEvent(detachedStoreContext(ctx), event); err != nil {
		r.notifyStoreFailure(input, "append_tool_result", err)
	}
}

// finishRun 记录一次普通 Run 的最终状态，并保存最终脱敏观测。
//
// 它先追加与生命周期状态对应的结束事件，再调用 FinishRun 更新 Run 记录；
// 即使结束事件写入失败，也会继续尝试更新最终状态。持久化失败只通知
// onError，不改变 Runtime 已经得到的业务结果。
func (r *runStoreRecorder) finishRun(
	ctx context.Context,
	input RunInput,
	execution *runtimeExecution,
	runErr error,
) {
	if r == nil || r.store == nil || execution == nil || execution.shadow {
		return
	}
	observation := snapshotRunObservation(execution)
	status, eventType := classifyRunCompletion(runErr)
	event := runstore.RunEvent{
		RunID:      input.RunID,
		RequestID:  input.RequestID,
		TenantID:   input.TenantID,
		Type:       eventType,
		OccurredAt: time.Now().UTC(),
		Provider:   observation.Provider,
		Model:      observation.Model,
		Status:     string(status),
		DurationMS: observation.Duration.Milliseconds(),
		ErrorKind:  observation.ErrorKind,
	}
	// 结束阶段要尽量保留，即使结束事件因为存储异常没有写成功，
	// 因此两个 Store 操作分别执行并分别通知失败。
	storeCtx := detachedStoreContext(ctx)
	if err := r.store.AppendEvent(storeCtx, event); err != nil {
		r.notifyStoreFailure(input, "append_finish_event", err)
	}
	if err := r.store.FinishRun(storeCtx, input.RunID, status, observation); err != nil {
		r.notifyStoreFailure(input, "finish_run", err)
	}
}

// detachedStoreContext 保留调用方 Context 中的值，但移除取消和截止时间。
//
// RunStore 是旁路持久化边界；主请求取消后仍需要记录 canceled 结果，
// 因此不能直接把已经取消的业务 Context 传给 Store。nil Context 则退回
// Background，避免向存储层传递非法 Context。
func detachedStoreContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return context.WithoutCancel(ctx)
}

// snapshotRunObservation 将 Runtime 的观测转换成 RunStore 使用的观测副本。
//
// 读取时只复制脱敏字段，并在锁内完成快照，避免持久化过程中继续持有
// Runtime 执行锁，也避免把可变的观测结构直接交给 Store。
func snapshotRunObservation(execution *runtimeExecution) runstore.RunObservation {
	if execution == nil {
		return runstore.RunObservation{}
	}
	execution.mu.Lock()
	defer execution.mu.Unlock()
	return runstore.RunObservation{
		Provider:         execution.observation.Provider,
		Model:            execution.observation.Model,
		Duration:         execution.observation.Duration,
		Success:          execution.observation.Success,
		StructuredValid:  execution.observation.StructuredValid,
		PromptTokens:     execution.observation.PromptTokens,
		CompletionTokens: execution.observation.CompletionTokens,
		TotalTokens:      execution.observation.TotalTokens,
		Cost:             execution.observation.Cost,
		ErrorKind:        execution.observation.ErrorKind,
	}
}

// classifyRunCompletion 把 Runtime 结果映射为 RunStore 的生命周期状态和结束事件。
//
// Context 取消和超时都表示本次执行没有正常完成，因此统一进入 canceled；
// 其他错误进入 failed；只有没有错误时才进入 succeeded。
func classifyRunCompletion(err error) (runstore.RunStatus, runstore.EventType) {
	switch {
	case err == nil:
		return runstore.RunSucceeded, runstore.EventRunSucceeded
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return runstore.RunCanceled, runstore.EventRunCanceled
	default:
		return runstore.RunFailed, runstore.EventRunFailed
	}
}

// classifyModelCall 将模型调用错误转换成事件可保存的稳定状态。
//
// 这里不返回底层错误文本；事件只需要区分成功、取消、超时和其他模型失败，
// 便于后续统计而不扩大敏感信息暴露面。
func classifyModelCall(err error) (string, string) {
	switch {
	case err == nil:
		return "success", ""
	case errors.Is(err, context.Canceled):
		return "canceled", "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout", "timeout"
	default:
		return "failed", "model_error"
	}
}

// RunStoreFailure 描述一次 RunStore 写入失败的脱敏信息。
//
// 该结构只用于通知观测或日志层，不携带底层错误原文、Prompt、模型输出、
// 工具参数或 API Key。
type RunStoreFailure struct {
	RunID     string
	RequestID string
	TenantID  string
	Operation string
	ErrorKind string
}

// RunStoreErrorHandler 接收 RunStore 写入失败的稳定元数据。
//
// 回调不会改变 Runtime 的主流程；调用方可以在这里接入日志、指标或告警。
type RunStoreErrorHandler func(RunStoreFailure)

// classifyRunStoreError 将存储层错误转换成稳定分类，避免把底层错误文本
// 直接暴露给上层或写入业务响应。
func classifyRunStoreError(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, runstore.ErrInvalidRun),
		errors.Is(err, runstore.ErrInvalidEvent),
		errors.Is(err, runstore.ErrInvalidEventType):
		return "invalid"
	case errors.Is(err, runstore.ErrRunAlreadyExists):
		return "already_exists"
	case errors.Is(err, runstore.ErrRunNotFound):
		return "not_found"
	case errors.Is(err, runstore.ErrRunIdentityMismatch):
		return "identity_mismatch"
	case errors.Is(err, runstore.ErrRunAlreadyFinished):
		return "already_finished"
	case errors.Is(err, runstore.ErrInvalidStateTransition):
		return "invalid_state_transition"
	default:
		return "storage_error"
	}
}

// configReferencesFromSnapshot 将一次 Run 使用的 ConfigSnapshot 转换为
// RunStore 中 ConfigReferences 所需的版本引用，供后续记录 Run 时绑定配置版本。
//
// 它会复制发布版本，以及 Prompt、Schema、Route、Pricing 各配置项的版本号；
// 不复制 Prompt 模板、Schema 内容、模型参数或其他配置正文，避免 RunStore
// 保存敏感信息和可变的执行配置。
func configReferencesFromSnapshot(
	snapshot configregistry.ConfigSnapshot,
) runstore.ConfigReferences {
	references := runstore.ConfigReferences{
		ReleaseVersion:  snapshot.ReleaseVersion,
		PromptVersions:  make(map[string]string, len(snapshot.Prompts)),
		SchemaVersions:  make(map[string]string, len(snapshot.Schemas)),
		RouteVersions:   make(map[string]string, len(snapshot.Routes)),
		PricingVersions: make(map[string]string, len(snapshot.Pricing)),
	}
	for name, prompt := range snapshot.Prompts {
		references.PromptVersions[name] = prompt.Version
	}
	for kind, schema := range snapshot.Schemas {
		references.SchemaVersions[kind] = schema.Version
	}
	for task, route := range snapshot.Routes {
		references.RouteVersions[task] = route.Version
	}
	for key, pricing := range snapshot.Pricing {
		references.PricingVersions[key] = pricing.Version
	}
	return references
}
