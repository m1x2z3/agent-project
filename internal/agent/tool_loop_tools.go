package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"agent-project/internal/recording"
	"agent-project/internal/structuredoutput"
	"agent-project/internal/tool/registry"
	"agent-project/internal/tool/runner"
	"agent-project/internal/tool/tooling"
)

// digestToolArguments 为工具参数生成稳定摘要。
//
// encoding/json 会按稳定顺序编码 map 的键，因此相同语义的参数不会因为
// map 插入顺序不同而产生不同摘要。摘要只用于审计和重复调用判断。
func digestToolArguments(arguments map[string]any) (string, error) {
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return "", fmt.Errorf("%w: marshal arguments", ErrToolHistoryEncodingFailed)
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// digestToolResult 为工具结果生成稳定摘要。
//
// 摘要输入只包含结果状态、工具身份、错误码、结构化数据和来源位置；
// 错误消息不会进入摘要，避免把供应商或数据源原文间接带入持久化记录。
// 原始结果只在当前进程内参与计算，RunStore 只保存最终哈希。
func digestToolResult(
	request runner.ToolCallRequest,
	result tooling.ToolResult,
	executionErr error,
) string {
	status, errorCode := toolExecutionEventStatus(result, executionErr)
	payload := struct {
		ToolName    string
		ToolVersion string
		Status      string
		ErrorCode   string
		Data        json.RawMessage
		Source      []tooling.SourceRef
	}{
		ToolName:    request.Decision.ToolName,
		ToolVersion: request.ToolVersion,
		Status:      status,
		ErrorCode:   errorCode,
		Data:        result.Data,
		Source:      result.Source,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		encoded = []byte(status + ":" + errorCode)
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// authorizeToolDecision 调用可信授权回调，把模型意图补充成可执行请求。
//
// 授权回调负责注入工具版本、资源范围、CapabilityToken 和幂等键；
// 回调返回后仍要校验请求，确保它没有偏离当前模型决策。这个阶段只
// 生成请求，不调用 Executor.Execute。
func (r *ToolLoopRuntime) authorizeToolDecision(
	ctx context.Context,
	state toolLoopState,
) (toolLoopState, error) {
	// 授权属于模型决策之后的阶段；请求已经取消时，不再继续调用外部授权逻辑。
	if err := ctx.Err(); err != nil {
		return toolLoopState{}, err
	}
	// should_call=false 表示模型明确决定不调用工具，因此不需要生成授权请求。
	if !state.toolDecision.ShouldCall {
		return state, nil
	}
	if r == nil || r.authorizer == nil {
		return toolLoopState{}, runtimeFailure(
			"tool_loop_authorize",
			ErrToolAuthorizationFailed,
		)
	}

	// 授权回调由可信调用方提供，负责补充模型无法自行生成的版本、
	// 租户、Run、资源范围、CapabilityToken 和幂等键。
	// 先复制模型决策，避免回调通过 Arguments 的 Map 引用修改状态中的原始决策。
	decision, err := cloneToolDecision(state.toolDecision)
	if err != nil {
		return toolLoopState{}, runtimeFailure(
			"tool_loop_authorize",
			fmt.Errorf("%w: clone decision", ErrToolAuthorizationFailed),
		)
	}
	request, err := r.authorizer(ctx, state.input, decision)
	if err != nil {
		return toolLoopState{}, runtimeFailure(
			"tool_loop_authorize",
			fmt.Errorf("%w: %w", ErrToolAuthorizationFailed, err),
		)
	}
	// 授权回调只能补充执行上下文，不能把模型决定的工具或参数改成另一份意图。
	if !reflect.DeepEqual(request.Decision, state.toolDecision) {
		return toolLoopState{}, runtimeFailure(
			"tool_loop_authorize",
			fmt.Errorf("%w: decision changed", ErrToolAuthorizationFailed),
		)
	}
	// 授权结果必须绑定当前请求的租户、Run 和 Request，不能借用其他执行上下文。
	if request.TenantID != state.input.TenantID ||
		request.RunID != state.input.RunID ||
		request.RequestID != state.input.RequestID {
		return toolLoopState{}, runtimeFailure(
			"tool_loop_authorize",
			fmt.Errorf("%w: execution context changed", ErrToolAuthorizationFailed),
		)
	}
	// 对回调返回的完整请求做确定性校验，确保它具备进入 Executor 的基本条件。
	if err := request.Validate(recording.ClockFromContext(ctx).Now()); err != nil {
		return toolLoopState{}, runtimeFailure(
			"tool_loop_authorize",
			fmt.Errorf("%w: invalid authorized request", ErrToolAuthorizationFailed),
		)
	}
	// 只有完整请求通过校验后，才保存到状态供后续工具执行阶段使用。
	state.toolCallRequest = request
	return state, nil
}

// checkToolCallSecurity 在授权之后、预算和执行之前完成只读契约及安全门禁。
//
// Executor.Inspect 会提前发现工具发现、参数 Schema、权限和审批等契约问题；
// ErrApprovalRequired 转换为本日稳定的危险请求错误，同时保留底层审批原因。
// 其他契约错误继续沿用 ErrToolExecutionFailed 分类，不冒充安全策略命中。
func (r *ToolLoopRuntime) checkToolCallSecurity(
	ctx context.Context,
	state toolLoopState,
) (toolLoopState, error) {
	if err := ctx.Err(); err != nil {
		return toolLoopState{}, err
	}
	if !state.toolDecision.ShouldCall {
		return state, nil
	}
	if r == nil || r.executor == nil {
		return state, runtimeFailure("tool_loop_tool_security", ErrToolExecutionFailed)
	}
	definition, err := r.executor.Inspect(ctx, state.toolCallRequest)
	if err != nil {
		if errors.Is(err, tooling.ErrApprovalRequired) {
			// 审批拒绝已经完成一次确定性的危险工具检查；其他契约错误不计安全检查。
			securityErr := fmt.Errorf("%w: %w", ErrDangerousToolRequest, err)
			recordSecurityCheck(state.execution)
			recordSecurityBlock(state.execution, securityErr)
			return state, runtimeFailure("tool_loop_tool_security", securityErr)
		}
		return state, runtimeFailure(
			"tool_loop_tool_security",
			fmt.Errorf("%w: %w", ErrToolExecutionFailed, err),
		)
	}
	// 普通契约通过后，危险能力与数据流合并计为一次执行前安全检查。
	recordSecurityCheck(state.execution)
	// 注册定义只用于本地判断；普通 Capability Token 不能放行保留的网络出站能力。
	if err := inspectToolRequestSecurity(definition, state.externalContentSeen); err != nil {
		recordSecurityBlock(state.execution, err)
		return state, runtimeFailure("tool_loop_tool_security", err)
	}
	return state, nil
}

// checkDuplicateToolCall 检查当前调用是否已经在本次 Run 中执行过。
//
// 工具名、显式版本和参数摘要共同构成一次调用的逻辑标识；只有三者
// 同时相同才视为重复。这里不执行工具，只为后续执行阶段保存摘要。
func (r *ToolLoopRuntime) checkDuplicateToolCall(
	ctx context.Context,
	state toolLoopState,
) (toolLoopState, error) {
	if err := ctx.Err(); err != nil {
		return toolLoopState{}, err
	}
	if !state.toolDecision.ShouldCall {
		return state, nil
	}

	// 该阶段应当紧接授权阶段执行；缺少版本说明授权请求没有准备完整。
	if state.toolCallRequest.ToolVersion == "" ||
		state.toolCallRequest.Decision.ToolName == "" {
		return toolLoopState{}, runtimeFailure(
			"tool_loop_duplicate_check",
			ErrToolAuthorizationFailed,
		)
	}
	digest, err := digestToolArguments(state.toolCallRequest.Decision.Arguments)
	if err != nil {
		return toolLoopState{}, runtimeFailure(
			"tool_loop_duplicate_check",
			err,
		)
	}

	// 摘要不包含原始参数，只用于比较和审计，避免敏感参数进入错误或日志。
	for _, entry := range state.toolHistory {
		if entry.Record.ToolName == state.toolCallRequest.Decision.ToolName &&
			entry.Record.ToolVersion == state.toolCallRequest.ToolVersion &&
			entry.Record.ArgumentsDigest == digest {
			return toolLoopState{}, runtimeFailure(
				"tool_loop_duplicate_check",
				ErrDuplicateToolCall,
			)
		}
	}
	state.toolArgumentsDigest = digest

	// 精确重复检查之后，再比较不包含授权版本的规范化模型决策。
	// 这样同版本重复仍返回 ErrDuplicateToolCall；版本变化但模型状态重复时，
	// 也能识别 A -> B -> A 等没有产生新决策进展的循环。
	decisionDigest, err := digestToolLoopDecision("tool_execution", state.toolDecision)
	if err != nil {
		return toolLoopState{}, runtimeFailure("tool_loop_duplicate_check", err)
	}
	for _, previous := range state.toolDecisionDigests {
		if previous == decisionDigest {
			return toolLoopState{}, runtimeFailure("tool_loop_duplicate_check", ErrLoopDetected)
		}
	}
	state.toolDecisionDigests = append(state.toolDecisionDigests, decisionDigest)
	return state, nil
}

// checkToolCallLimit 检查当前工具调用是否超过本次 Run 的最大次数。
//
// toolHistory 中的每一条记录都代表一次已经进入执行边界的调用，因此
// 只有历史数量达到上限时才拒绝当前调用。该检查必须发生在 Executor
// 之前，确保被拒绝的调用不会触发工具或数据源。
func (r *ToolLoopRuntime) checkToolCallLimit(
	ctx context.Context,
	state toolLoopState,
) (toolLoopState, error) {
	if err := ctx.Err(); err != nil {
		return toolLoopState{}, err
	}
	if !state.toolDecision.ShouldCall {
		return state, nil
	}
	if r == nil {
		return toolLoopState{}, ErrToolLoopNotConfigured
	}
	maxCalls := r.maxCalls
	if maxCalls <= 0 {
		maxCalls = defaultMaxToolCalls
	}
	if len(state.toolHistory) >= maxCalls {
		return toolLoopState{}, runtimeFailure(
			"tool_loop_max_calls",
			ErrMaxToolCallsExceeded,
		)
	}
	return state, nil
}

// executeTool 执行当前授权后的单次工具调用，并把结果追加到工具历史。
//
// Executor 内部会再次经过 Registry 和 tooling.Invoke，因此这里不直接调用
// Tool.Execute。工具返回的业务失败会作为结构化历史继续传给下一轮模型；
// 执行器、参数或安全边界错误则保留脱敏失败记录后停止当前 Runtime。
// 该方法只负责工具分支，不调用最终回答模型。
func (r *ToolLoopRuntime) executeTool(
	ctx context.Context,
	state toolLoopState,
) (toolLoopState, error) {
	if err := ctx.Err(); err != nil {
		return toolLoopState{}, err
	}
	if !state.toolDecision.ShouldCall {
		return state, nil
	}
	if state.execution == nil || state.execution.budget == nil {
		return toolLoopState{}, ErrRuntimeNotConfigured
	}
	// 工具必须在进入 Executor 前占用本次 Run 的工具次数和执行步数；
	// 被预算拒绝时不访问 Registry、工具实现或外部数据源。
	if err := state.execution.budget.BeginToolCall(recording.ClockFromContext(ctx).Now()); err != nil {
		var violation BudgetViolation
		if errors.As(err, &violation) && violation.Reason == "max_tool_calls_exceeded" {
			// 保留旧调用方可识别的错误哨兵，同时通过 ErrBudgetExceeded 提供统一终态分类。
			err = fmt.Errorf("%w: %w", ErrMaxToolCallsExceeded, err)
		}
		return state, runtimeFailure("tool_loop_execute_budget", err)
	}
	var err error
	if r == nil || r.executor == nil {
		return toolLoopState{}, runtimeFailure(
			"tool_loop_execute",
			ErrToolExecutionFailed,
		)
	}

	// 该方法通常紧接重复调用检查执行；这里补齐摘要计算，确保单测或
	// 后续调用方直接使用本方法时，历史记录仍然具备稳定的脱敏标识。
	digest := state.toolArgumentsDigest
	if digest == "" {
		digest, err = digestToolArguments(state.toolCallRequest.Decision.Arguments)
		if err != nil {
			return toolLoopState{}, runtimeFailure("tool_loop_execute", err)
		}
	}

	// Executor 会完成工具解析、参数编码、租户和 Capability Token 校验，
	// 最终再把调用交给 tooling.Invoke。
	if state.execution != nil && state.execution.recorder != nil {
		state.execution.recorder.recordToolCall(
			ctx,
			state.input,
			state.toolCallRequest.Decision.ToolName,
			state.toolCallRequest.ToolVersion,
			digest,
		)
	}
	callStarted := recording.ClockFromContext(ctx).Now()
	result, err := r.executor.Execute(ctx, state.toolCallRequest)
	// 对原始 Data/Source 立即生成脱敏报告，不在命中时提前返回；下面仍需保留
	// 已经发生的真实工具结果和历史。执行错误优先沿用原失败，不再做内容判断。
	injectionReport := promptInjectionReport{}
	if err == nil {
		injectionReport = inspectToolResultPromptInjection(result)
		// 每个成功返回的工具结果只联合检查一次 Data/Source，包括空内容和业务失败。
		recordSecurityCheck(state.execution)
		// 污染状态只在真实执行成功后更新；业务失败但没有 Data/Source 不制造污染。
		state.externalContentSeen = state.externalContentSeen || toolResultContainsExternalContent(result)
	}
	status, errorCode := toolExecutionEventStatus(result, err)
	if state.execution != nil && state.execution.recorder != nil {
		state.execution.recorder.recordToolResult(
			ctx,
			state.input,
			state.toolCallRequest.Decision.ToolName,
			state.toolCallRequest.ToolVersion,
			status,
			errorCode,
			digest,
			digestToolResult(state.toolCallRequest, result, err),
			recording.ClockFromContext(ctx).Now().Sub(callStarted),
		)
	}
	state.toolHistory = append(state.toolHistory, toolHistoryEntryFromExecution(
		state.toolCallRequest,
		digest,
		result,
		err,
	))
	state.toolArgumentsDigest = digest

	if err != nil {
		// 只向上返回稳定错误类型；完整底层错误不进入工具历史或用户输出。
		return state, runtimeFailure(
			"tool_loop_execute",
			fmt.Errorf("%w: %w", ErrToolExecutionFailed, err),
		)
	}
	if injectionReport.blocked() {
		// 工具事实已经保留，但攻击内容不得进入压缩、Prompt、下一次模型或后续工具。
		recordSecurityBlock(state.execution, ErrPromptInjectionDetected)
		return state, runtimeFailure("tool_loop_tool_result_security", ErrPromptInjectionDetected)
	}
	return state, nil
}

// toolExecutionEventStatus 将工具结果或执行边界错误转换为事件可保存的
// 稳定状态和错误分类，不传播工具实现、数据源或供应商的原始错误消息。
func toolExecutionEventStatus(
	result tooling.ToolResult,
	executionErr error,
) (string, string) {
	if result.Status != "" {
		if result.Error != nil {
			return string(result.Status), result.Error.Code
		}
		return string(result.Status), ""
	}
	status, errorCode := classifyToolExecutionError(executionErr)
	return string(status), errorCode
}

// toolHistoryEntryFromExecution 将工具结果转换成下一轮模型可见的历史项。
//
// Data 和 Source 是工具契约允许继续传递的结构化信息；参数只保存摘要。
// 当 Executor 在进入工具前失败时，根据稳定错误类型生成状态和错误分类，
// 不使用底层错误文本，避免敏感信息进入 Prompt。
func toolHistoryEntryFromExecution(
	request runner.ToolCallRequest,
	digest string,
	result tooling.ToolResult,
	executionErr error,
) toolHistoryEntry {
	record := ToolCallRecord{
		ToolName:        request.Decision.ToolName,
		ToolVersion:     request.ToolVersion,
		Status:          result.Status,
		ArgumentsDigest: digest,
	}
	if result.Error != nil {
		record.ErrorCode = result.Error.Code
	}
	if record.Status == "" {
		record.Status, record.ErrorCode = classifyToolExecutionError(executionErr)
	}

	entry := toolHistoryEntry{
		Record:            record,
		ResultContentKind: result.ResultContentKind,
	}
	if len(result.Data) > 0 && json.Valid(result.Data) {
		entry.Data = append(json.RawMessage(nil), result.Data...)
	}
	if len(result.Source) > 0 {
		entry.Source = make([]tooling.SourceRef, len(result.Source))
		copy(entry.Source, result.Source)
	}
	return entry
}

// classifyToolExecutionError 把执行边界错误归类为稳定的工具结果状态。
//
// 这里只返回有限的错误码，不传播供应商、数据源或工具实现的原始消息。
func classifyToolExecutionError(err error) (tooling.ResultStatus, string) {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return tooling.ResultTimeout, "timeout"
	case errors.Is(err, context.Canceled):
		return tooling.ResultInternalError, "canceled"
	case errors.Is(err, runner.ErrInvalidToolCallRequest),
		errors.Is(err, runner.ErrInvalidToolDecision),
		errors.Is(err, runner.ErrToolVersionRequired),
		errors.Is(err, runner.ErrArgumentsEncodingFailed),
		errors.Is(err, tooling.ErrInvalidRequest),
		errors.Is(err, tooling.ErrArgumentsInvalid):
		return tooling.ResultInvalidArgument, "invalid_argument"
	case errors.Is(err, registry.ErrToolNotFound),
		errors.Is(err, registry.ErrToolVersionNotFound):
		return tooling.ResultNotFound, "tool_not_found"
	case errors.Is(err, registry.ErrToolDeprecated),
		errors.Is(err, tooling.ErrCapabilityTokenMissing),
		errors.Is(err, tooling.ErrCapabilityTokenInvalid),
		errors.Is(err, tooling.ErrTenantMismatch),
		errors.Is(err, tooling.ErrRunMismatch),
		errors.Is(err, tooling.ErrToolMismatch),
		errors.Is(err, tooling.ErrToolVersionMismatch),
		errors.Is(err, tooling.ErrCapabilityExpired),
		errors.Is(err, tooling.ErrResourceScopeDenied),
		errors.Is(err, tooling.ErrPermissionDenied),
		errors.Is(err, tooling.ErrApprovalRequired):
		return tooling.ResultForbidden, "forbidden"
	default:
		return tooling.ResultInternalError, "execution_failed"
	}
}

// cloneToolDecision 深拷贝工具决策，避免授权回调修改运行状态中的参数 Map。
//
// ToolDecision 来自结构化 JSON，因此参数可以通过 JSON 往返复制；
// 如果参数包含不可编码值，则直接拒绝进入授权阶段。
func cloneToolDecision(decision structuredoutput.ToolDecision) (structuredoutput.ToolDecision, error) {
	// JSON 往返可以同时复制嵌套 Map，避免只复制顶层结构留下引用别名。
	encoded, err := json.Marshal(decision)
	if err != nil {
		return structuredoutput.ToolDecision{}, err
	}
	var cloned structuredoutput.ToolDecision
	if err := json.Unmarshal(encoded, &cloned); err != nil {
		return structuredoutput.ToolDecision{}, err
	}
	// 返回独立对象后，授权回调对参数的修改不会影响原始状态。
	return cloned, nil
}

// compressToolResultData 将超过目标的单个工具 Data 投影为确定性 JSON 摘要。
//
// 函数先用 json.Compact 校验并紧凑编码原始数据，避免无意义地压缩只包含
// 大量空白的数据。未超过目标时返回原始 bytes 的独立副本；超过目标时只保留
// 原始 Token 估算、SHA-256 摘要和 UTF-8 安全预览。maxTokens 必须来自已规范化
// 的正数策略，压缩过程不调用模型，也不修改调用方持有的 Data。
func compressToolResultData(
	data json.RawMessage,
	maxTokens int,
) (json.RawMessage, bool, error) {
	if maxTokens <= 0 {
		return nil, false, ErrInvalidContextPolicy
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, data); err != nil {
		return nil, false, fmt.Errorf("%w: invalid tool data", ErrToolHistoryEncodingFailed)
	}
	// 是否压缩按紧凑内容判断，original_tokens 则保留原始 Data 的本地估算口径。
	if estimateTokenCount(compact.String()) <= maxTokens {
		return append(json.RawMessage(nil), data...), false, nil
	}

	// Token 估算按每四个 Unicode 码点计算，因此预览也按码点而不是字节截断。
	previewRunes := []rune(compact.String())
	// 只有 maxTokens 不超过现有内容的四分之一时才相乘，避免超大自定义值溢出。
	if maxTokens <= len(previewRunes)/4 && len(previewRunes) > maxTokens*4 {
		previewRunes = previewRunes[:maxTokens*4]
	}
	sum := sha256.Sum256(data)
	projected := struct {
		Compressed     bool   `json:"compressed"`
		OriginalTokens int    `json:"original_tokens"`
		Digest         string `json:"digest"`
		Preview        string `json:"preview"`
	}{
		Compressed:     true,
		OriginalTokens: estimateTokenCount(string(data)),
		Digest:         "sha256:" + hex.EncodeToString(sum[:]),
		Preview:        string(previewRunes),
	}
	encoded, err := json.Marshal(projected)
	if err != nil {
		return nil, false, fmt.Errorf("%w: marshal compressed tool data", ErrToolHistoryEncodingFailed)
	}
	return encoded, true, nil
}

// contextGovernanceReport 描述一次模型 Prompt 构建产生的脱敏治理结果。
//
// 报告只包含上下文版本、是否发生治理和本地 Token 估算，不携带 Prompt、
// 工具 Data、压缩预览或来源内容；后续 Prompt 构建阶段每次提交一份报告。
type contextGovernanceReport struct {
	contextVersion string // 标识本次 Prompt 使用的上下文算法、发布版本和实际策略。
	trimmed        bool   // 表示本次构建是否实际发生压缩、历史降级或条目省略。
	tokensBefore   int    // 保存相同模板下未治理基线 Prompt 的本地 Token 估算。
	tokensAfter    int    // 保存实际发送给模型的治理后 Prompt 本地 Token 估算。
}

// promptSourceRef 是允许进入模型 Prompt 的工具来源投影。
type promptSourceRef struct {
	Source   string `json:"source"`
	Location string `json:"location"`
}

// promptToolResultContent 把 Data 和 Source 放在同一个不可信内容投影中。
// 两个字段都不携带授权语义，外部正文也不能覆盖所属历史的可信标签。
type promptToolResultContent struct {
	Data   json.RawMessage   `json:"data,omitempty"`
	Source []promptSourceRef `json:"source,omitempty"`
}

// promptHistoryEntry 是允许进入模型 Prompt 的单条工具历史投影。
//
// 字段必须显式列出，不能直接序列化内部 toolHistoryEntry，避免未来新增的
// 授权或执行字段意外暴露给模型。Trust 由 Runtime 固定写入，ContentKind 只能
// 从注册工具定义转换，不能读取 Data、Source 或工具正文中的伪造声明。
type promptHistoryEntry struct {
	ToolName        string                  `json:"tool_name"`
	ToolVersion     string                  `json:"tool_version"`
	Status          string                  `json:"status"`
	ErrorCode       string                  `json:"error_code,omitempty"`
	ArgumentsDigest string                  `json:"arguments_digest"`
	Trust           trustLevel              `json:"trust"`
	ContentKind     contentKind             `json:"content_kind"`
	Content         promptToolResultContent `json:"content"`
}

// promptHistoryEnvelope 为模型说明安全版本、历史治理版本和裁剪情况。
// 两套版本分别标识安全标签算法与历史治理算法，不能相互替代。
type promptHistoryEnvelope struct {
	SecurityVersion string               `json:"security_version"`
	ContextVersion  string               `json:"context_version"`
	OmittedEntries  int                  `json:"omitted_entries"`
	Entries         []promptHistoryEntry `json:"entries"`
}

// projectToolHistory 将内部工具历史复制为独立的模型可见条目。
//
// Data 和 Source 都会复制，后续压缩、降级和省略只修改返回的投影，不会
// 修改原始 toolHistory。所有 Data 在投影时验证，不能通过后续省略掩盖非法 JSON。
func projectToolHistory(history []toolHistoryEntry) ([]promptHistoryEntry, error) {
	entries := make([]promptHistoryEntry, 0, len(history))
	for _, entry := range history {
		kind, err := contentKindFromToolResult(entry.ResultContentKind)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrToolHistoryEncodingFailed, err)
		}
		// 只映射允许进入 Prompt 的脱敏元数据和代码固定的外部内容标签。
		promptEntry := promptHistoryEntry{
			ToolName:        entry.Record.ToolName,
			ToolVersion:     entry.Record.ToolVersion,
			Status:          string(entry.Record.Status),
			ErrorCode:       entry.Record.ErrorCode,
			ArgumentsDigest: entry.Record.ArgumentsDigest,
			Trust:           trustUntrustedExternal,
			ContentKind:     kind,
		}
		if len(entry.Data) > 0 {
			if !json.Valid(entry.Data) {
				return nil, fmt.Errorf("%w: invalid tool data", ErrToolHistoryEncodingFailed)
			}
			promptEntry.Content.Data = append(json.RawMessage(nil), entry.Data...)
		}
		if len(entry.Source) > 0 {
			promptEntry.Content.Source = make([]promptSourceRef, 0, len(entry.Source))
			for _, source := range entry.Source {
				promptEntry.Content.Source = append(promptEntry.Content.Source, promptSourceRef{
					Source:   source.Source,
					Location: source.Location,
				})
			}
		}
		entries = append(entries, promptEntry)
	}
	return entries, nil
}

// marshalToolHistoryEnvelope 编码带上下文版本和省略数量的历史投影。
func marshalToolHistoryEnvelope(
	entries []promptHistoryEntry,
	runSecurityVersion string,
	contextVersion string,
	omittedEntries int,
) (string, error) {
	if entries == nil {
		entries = make([]promptHistoryEntry, 0)
	}
	if runSecurityVersion == "" {
		return "", fmt.Errorf("%w: missing security version", ErrToolHistoryEncodingFailed)
	}
	for _, entry := range entries {
		switch entry.ContentKind {
		case contentKindToolResult, contentKindDocument, contentKindLog, contentKindIssue:
		default:
			return "", fmt.Errorf("%w: invalid tool result kind", ErrToolHistoryEncodingFailed)
		}
		if entry.Trust != trustUntrustedExternal {
			return "", fmt.Errorf("%w: invalid tool result trust", ErrToolHistoryEncodingFailed)
		}
	}
	data, err := json.Marshal(promptHistoryEnvelope{
		SecurityVersion: runSecurityVersion,
		ContextVersion:  contextVersion,
		OmittedEntries:  omittedEntries,
		Entries:         entries,
	})
	if err != nil {
		return "", fmt.Errorf("%w: marshal tool history", ErrToolHistoryEncodingFailed)
	}
	return string(data), nil
}

// encodeGovernedToolHistory 从原始历史构建独立、确定性的版本化治理投影。
//
// 每条 Data 先按单结果目标压缩。整个 envelope 仍超过历史目标时，按从最旧
// 到最新的顺序移除非关键旧条目的 Data；仍超过目标时再省略这些条目。
// 最新 KeepLatestResults 条和失败条目属于关键证据，不降级也不省略，因此
// 固定元数据和关键证据允许超过目标，而不会为了硬凑上限被删除。
func encodeGovernedToolHistory(
	history []toolHistoryEntry,
	policy ContextPolicy,
	releaseVersion string,
) (string, contextGovernanceReport, error) {
	normalized, err := normalizeContextPolicy(policy)
	if err != nil {
		return "", contextGovernanceReport{}, err
	}
	report := contextGovernanceReport{
		contextVersion: normalized.contextVersion(releaseVersion),
	}
	entries, err := projectToolHistory(history)
	if err != nil {
		return "", contextGovernanceReport{}, err
	}

	// 关键标记跟随原始顺序；后续省略条目时同步删除对应标记。
	critical := make([]bool, len(entries))
	latestStart := len(entries) - normalized.KeepLatestResults
	if latestStart < 0 {
		latestStart = 0
	}
	for index := range entries {
		failed := entries[index].ErrorCode != "" ||
			(entries[index].Status != "" && entries[index].Status != string(tooling.ResultSuccess))
		critical[index] = index >= latestStart || failed
		if len(entries[index].Content.Data) == 0 {
			continue
		}
		compressed, didCompress, err := compressToolResultData(
			entries[index].Content.Data,
			normalized.MaxToolResultTokens,
		)
		if err != nil {
			return "", contextGovernanceReport{}, err
		}
		entries[index].Content.Data = compressed
		report.trimmed = report.trimmed || didCompress
	}

	encoded, err := marshalToolHistoryEnvelope(entries, securityVersion(releaseVersion), report.contextVersion, 0)
	if err != nil {
		return "", contextGovernanceReport{}, err
	}
	// 第一阶段只降级非关键旧条目，每次移除最旧的一份 Data 后重新计算整体估算。
	for estimateTokenCount(encoded) > normalized.MaxHistoryTokens {
		degraded := false
		for index := range entries {
			if critical[index] || len(entries[index].Content.Data) == 0 {
				continue
			}
			entries[index].Content.Data = nil
			report.trimmed = true
			degraded = true
			break
		}
		if !degraded {
			break
		}
		encoded, err = marshalToolHistoryEnvelope(entries, securityVersion(releaseVersion), report.contextVersion, 0)
		if err != nil {
			return "", contextGovernanceReport{}, err
		}
	}

	// 第二阶段才省略非关键旧条目；关键证据即使超出目标也完整保留其投影。
	omittedEntries := 0
	for estimateTokenCount(encoded) > normalized.MaxHistoryTokens {
		oldest := -1
		for index := range entries {
			if !critical[index] {
				oldest = index
				break
			}
		}
		if oldest < 0 {
			break
		}
		entries = append(entries[:oldest], entries[oldest+1:]...)
		critical = append(critical[:oldest], critical[oldest+1:]...)
		omittedEntries++
		report.trimmed = true
		encoded, err = marshalToolHistoryEnvelope(entries, securityVersion(releaseVersion), report.contextVersion, omittedEntries)
		if err != nil {
			return "", contextGovernanceReport{}, err
		}
	}
	return encoded, report, nil
}

// digestToolLoopDecision 为“执行阶段 + 规范化模型决策”生成稳定摘要。
//
// encoding/json 会按排序后的键编码参数 Map，因此语义相同的决策不会因 Map
// 插入顺序不同而得到不同摘要。摘要只用于当前 Run 的循环检测，不保存原始参数。
func digestToolLoopDecision(
	stage string,
	decision structuredoutput.ToolDecision,
) (string, error) {
	encoded, err := json.Marshal(struct {
		Stage    string                        `json:"stage"`
		Decision structuredoutput.ToolDecision `json:"decision"`
	}{
		Stage:    stage,
		Decision: decision,
	})
	if err != nil {
		return "", fmt.Errorf("%w: marshal loop decision", ErrToolHistoryEncodingFailed)
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
