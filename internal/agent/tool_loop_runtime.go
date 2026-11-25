package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"agent-project/internal/configregistry"
	"agent-project/internal/modelgateway"
	"agent-project/internal/rollout"
	"agent-project/internal/runstore"
	"agent-project/internal/structuredoutput"
	"agent-project/internal/tool/registry"
	"agent-project/internal/tool/runner"
	"agent-project/internal/tool/tooling"
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
	model ModelCaller
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
	analysisPrompt   string
	analysisResponse modelgateway.ChatResponse
	analysis         structuredoutput.TaskAnalysis
	analysisReport   structuredoutput.DecodeReport
	// 工具循环历史和当前工具决策阶段状态。
	toolHistory          []toolHistoryEntry
	toolDecisionPrompt   string
	toolDecisionResponse modelgateway.ChatResponse
	toolDecision         structuredoutput.ToolDecision
	toolDecisionReport   structuredoutput.DecodeReport
	// 授权成功后保存的请求，供后续 Executor.Execute 使用。
	toolCallRequest     runner.ToolCallRequest
	toolArgumentsDigest string
	// 最终回答阶段状态。
	finalPrompt       string
	finalResponse     modelgateway.ChatResponse
	finalAnswer       structuredoutput.FinalAnswer
	finalAnswerReport structuredoutput.DecodeReport
}

// toolHistoryEntry 保存下一轮模型需要的工具执行上下文。
//
// Data 和 Source 是工具返回的结构化结果，供模型继续判断；
// ToolCallRecord 只保留脱敏摘要，最终不会把完整工具数据放入 RunOutput。
type toolHistoryEntry struct {
	Record ToolCallRecord
	Data   json.RawMessage
	Source []tooling.SourceRef
}

// NewToolLoopRuntime 创建受控多轮工具 Runtime。
//
// 工具执行器和授权回调都是必需依赖；没有它们时，Runtime 无法保证模型
// 输出不会绕过工具执行边界。MaxToolCalls 未设置时使用默认值 3。
func NewToolLoopRuntime(cfg ToolLoopConfig) (*ToolLoopRuntime, error) {
	// 这些依赖共同组成工具循环的安全边界，缺少任意一项都不能安全运行。
	if cfg.Registry == nil ||
		cfg.Selector == nil ||
		cfg.Model == nil ||
		cfg.Executor == nil ||
		cfg.Authorizer == nil {
		return nil, ErrToolLoopNotConfigured
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
	return &ToolLoopRuntime{
		registry:           cfg.Registry,
		selector:           cfg.Selector,
		model:              cfg.Model,
		executor:           cfg.Executor,
		authorizer:         cfg.Authorizer,
		observer:           observer,
		runStore:           cfg.RunStore,
		onStoreError:       cfg.OnStoreError,
		maxCalls:           maxCalls,
		analysisRepair:     cfg.AnalysisRepair,
		toolDecisionRepair: cfg.ToolDecisionRepair,
		finalAnswerRepair:  cfg.FinalAnswerRepair,
	}, nil
}

// Run 执行一次受控多轮工具循环。
//
// 每轮工具决策都必须经过结构化解析、可信授权、重复检查和次数限制；
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

	state, err := r.selectAndBindSnapshot(ctx, input)
	if err != nil {
		return ToolLoopOutput{}, err
	}
	execution := state.execution

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

	for {
		if err := ctx.Err(); err != nil {
			return r.failToolLoop(ctx, input, execution, err)
		}
		if !state.toolDecision.ShouldCall {
			break
		}

		state, err = r.authorizeToolDecision(ctx, state)
		if err != nil {
			return r.failToolLoop(ctx, input, execution, err)
		}
		state, err = r.checkDuplicateToolCall(ctx, state)
		if err != nil {
			return r.failToolLoop(ctx, input, execution, err)
		}
		state, err = r.executeTool(ctx, state)
		if err != nil {
			return r.failToolLoop(ctx, input, execution, err)
		}
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
	// 从这里开始，观测和后续阶段都携带同一个 release version。
	execution := &runtimeExecution{
		started: time.Now(),
		// 每次工具循环都使用独立的记录器，确保本次 Run 的事件带有
		// 正确的 Run、Request 和 Tenant 标识；记录器本身只负责旁路存储。
		recorder: &runStoreRecorder{
			store:   r.runStore,
			onError: r.onStoreError,
		},
		observation: RunObservation{
			RequestID:      input.RequestID,
			TenantID:       input.TenantID,
			RunID:          input.RunID,
			ReleaseVersion: bound.ReleaseVersion,
		},
	}
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

// runAnalysis 复用现有 Runtime 的任务分析阶段。
//
// Prompt、Schema 和模型路由都从已经绑定的配置快照读取，避免工具循环
// 使用另一套未版本化的分析配置。模型原文只留在当前阶段状态中，
// 后续只使用通过结构化校验的 TaskAnalysis。
func (r *ToolLoopRuntime) runAnalysis(
	ctx context.Context,
	state toolLoopState,
) (toolLoopState, error) {
	if err := ctx.Err(); err != nil {
		return toolLoopState{}, err
	}
	// 读取 Prompt、Schema 和路由必须使用已经绑定的快照，不能回到 Registry 查询 active 版本。
	promptVersion, err := promptFromSnapshot(state.snapshot, analysisPromptName)
	if err != nil {
		return toolLoopState{}, err
	}
	// Prompt 只接收原始查询；分析结果必须在模型返回并通过校验后才会产生。
	rendered, err := renderPrompt(promptVersion.Template, map[string]any{
		"Query": state.input.Query,
	})
	if err != nil {
		return toolLoopState{}, err
	}
	// 路由参数也从同一快照读取，确保 Provider、Model 和生成参数可追溯。
	route, err := routeFromSnapshot(state.snapshot, analysisTask)
	if err != nil {
		return toolLoopState{}, err
	}
	recordRoute(state.execution, route)

	// 模型调用只接收已经渲染好的 Prompt；原始供应商协议由 ModelCaller 屏蔽。
	callStarted := time.Now()
	response, err := r.model.Chat(ctx, modelgateway.ChatRequest{
		Task:        analysisTask,
		Provider:    route.Provider,
		Model:       route.Model,
		Temperature: float64Pointer(route.Temperature),
		MaxTokens:   maxTokensPointer(route.MaxTokens),
		Timeout:     route.Timeout,
		Messages:    []modelgateway.Message{{Role: "user", Content: rendered}},
	})
	if state.execution != nil && state.execution.recorder != nil {
		state.execution.recorder.recordModelCall(
			ctx,
			state.input,
			route,
			response,
			time.Since(callStarted),
			err,
		)
	}
	if err != nil {
		return toolLoopState{}, runtimeFailure(
			"tool_loop_analysis_model",
			fmt.Errorf("%w: %w", ErrAnalysisModelFailed, err),
		)
	}
	// 原始响应仅留在当前阶段状态中，最终输出和观测不会暴露模型原文。
	state.analysisPrompt = rendered
	state.analysisResponse = response
	recordModelResponse(state.execution, state.snapshot, response, analysisTask)

	references, err := resolveReferences(
		state.snapshot,
		analysisPromptName,
		analysisTask,
		analysisTask,
	)
	if err != nil {
		return toolLoopState{}, err
	}
	// 解析元数据必须来自同一份快照，防止使用错误版本的 Schema 解读模型响应。
	value, report, err := structuredoutput.DecodeTaskAnalysis(
		ctx,
		[]byte(response.Message.Content),
		structuredoutput.DecodeOptions{
			Metadata: structuredoutput.ExecutionMetadata{
				Provider:           references.Provider,
				Model:              references.Model,
				PromptVersion:      references.PromptVersion,
				SchemaVersion:      references.SchemaVersion,
				ModelConfigVersion: references.ModelConfigVersion,
			},
			Repair: r.analysisRepair,
		},
	)
	if err != nil {
		return toolLoopState{}, runtimeFailure(
			"tool_loop_analysis_decode",
			fmt.Errorf("%w: %w", ErrAnalysisDecodeFailed, err),
		)
	}
	// 后续工具决策只使用经过结构化校验的分析结果。
	state.analysis = value
	state.analysisReport = report
	return state, nil
}

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

// renderToolDecisionPrompt 为当前待执行的工具决策轮次构造模型输入。
//
// 工具历史只通过受控 JSON 进入 Prompt；模型看不到 CapabilityToken、
// 完整模型原文或其他执行上下文。Prompt 模板本身来自本次 Run 的配置快照。
// 首次调用用于准备第一轮决策；工具执行后再次调用则用于准备下一轮决策。
func (r *ToolLoopRuntime) renderToolDecisionPrompt(
	ctx context.Context,
	state toolLoopState,
) (toolLoopState, error) {
	if err := ctx.Err(); err != nil {
		return toolLoopState{}, err
	}
	// 工具决策 Prompt 和工具历史都必须来自当前固定快照与受控状态。
	promptVersion, err := promptFromSnapshot(state.snapshot, toolDecisionPromptName)
	if err != nil {
		return toolLoopState{}, err
	}
	// 工具历史先转换成模型可见格式，再渲染模板，避免暴露内部授权字段。
	history, err := encodeToolHistory(state.toolHistory)
	if err != nil {
		return toolLoopState{}, err
	}
	rendered, err := renderPrompt(promptVersion.Template, map[string]any{
		"Query":       state.input.Query,
		"Analysis":    state.analysis,
		"ToolHistory": history,
	})
	if err != nil {
		return toolLoopState{}, err
	}
	// 保存本轮 Prompt，下一步模型调用只读取这个已完成的结果。
	state.toolDecisionPrompt = rendered
	return state, nil
}

// continueToolDecision 将工具结果带入下一轮工具决策。
//
// 工具执行完成后，先把脱敏后的 toolHistory 渲染进 Prompt，再调用模型并
// 严格解析新的 ToolDecision。这个阶段只负责“结果回传和下一次决策”，
// 不执行新工具；后续是否授权和执行仍需经过完整边界。
func (r *ToolLoopRuntime) continueToolDecision(
	ctx context.Context,
	state toolLoopState,
) (toolLoopState, error) {
	if err := ctx.Err(); err != nil {
		return toolLoopState{}, err
	}
	// 重新渲染 Prompt，使刚追加的工具结果成为模型下一次决策的输入。
	rendered, err := r.renderToolDecisionPrompt(ctx, state)
	if err != nil {
		return toolLoopState{}, err
	}
	// 使用包含最新工具历史的 Prompt 调用模型，但仍只获取调用意图。
	called, err := r.callToolDecisionModel(ctx, rendered)
	if err != nil {
		return toolLoopState{}, err
	}
	// 新的模型输出必须再次经过 Schema 和业务语义校验，不能直接进入授权。
	return r.decodeToolDecision(ctx, called)
}

// renderToolLoopFinalPrompt 为工具循环的最终回答阶段构造模型输入。
//
// 最终回答只使用原始问题、已校验的任务分析和脱敏后的工具历史；
// Prompt 模板来自本次 Run 的配置快照，避免绕过版本绑定读取全局配置。
func (r *ToolLoopRuntime) renderToolLoopFinalPrompt(
	ctx context.Context,
	state toolLoopState,
) (toolLoopState, error) {
	if err := ctx.Err(); err != nil {
		return toolLoopState{}, err
	}
	// 最终回答模板必须来自当前 Run 的固定版本。
	promptVersion, err := promptFromSnapshot(state.snapshot, finalPromptName)
	if err != nil {
		return toolLoopState{}, err
	}
	// 工具结果通过受控编码进入 Prompt，避免把内部授权字段暴露给模型。
	history, err := encodeToolHistory(state.toolHistory)
	if err != nil {
		return toolLoopState{}, err
	}
	rendered, err := renderPrompt(promptVersion.Template, map[string]any{
		"Query":       state.input.Query,
		"Analysis":    state.analysis,
		"ToolHistory": history,
	})
	if err != nil {
		return toolLoopState{}, err
	}
	state.finalPrompt = rendered
	return state, nil
}

// callToolLoopFinalModel 调用最终回答阶段的模型。
//
// 模型路由和生成参数都来自本次 Run 的配置快照；模型响应只保存到中间
// 状态，必须在后续 decodeToolLoopFinalAnswer 中通过结构化校验后才能作为
// 最终业务结果使用。该方法只负责 should_call=false 的最终回答分支，
// 不经过 runner.Executor，也不会执行任何工具。
func (r *ToolLoopRuntime) callToolLoopFinalModel(
	ctx context.Context,
	state toolLoopState,
) (toolLoopState, error) {
	if err := ctx.Err(); err != nil {
		return toolLoopState{}, err
	}
	// 最终回答使用固定快照中的路由，不能回退到全局默认模型。
	route, err := routeFromSnapshot(state.snapshot, finalTask)
	if err != nil {
		return toolLoopState{}, err
	}
	recordRoute(state.execution, route)

	// 这里只调用模型生成最终回答文本，不允许模型响应绕过后续解析校验。
	callStarted := time.Now()
	response, err := r.model.Chat(ctx, modelgateway.ChatRequest{
		Task:        finalTask,
		Provider:    route.Provider,
		Model:       route.Model,
		Temperature: float64Pointer(route.Temperature),
		MaxTokens:   maxTokensPointer(route.MaxTokens),
		Timeout:     route.Timeout,
		Messages: []modelgateway.Message{{
			Role:    "user",
			Content: state.finalPrompt,
		}},
	})
	if state.execution != nil && state.execution.recorder != nil {
		state.execution.recorder.recordModelCall(
			ctx,
			state.input,
			route,
			response,
			time.Since(callStarted),
			err,
		)
	}
	if err != nil {
		return toolLoopState{}, runtimeFailure(
			"tool_loop_final_model",
			fmt.Errorf("%w: %w", ErrFinalModelFailed, err),
		)
	}
	state.finalResponse = response
	recordModelResponse(state.execution, state.snapshot, response, finalTask)
	return state, nil
}

// decodeToolLoopFinalAnswer 严格解析最终回答模型的结构化输出。
//
// Prompt、Schema、模型和模型配置版本都从同一份 Run 快照解析，再交给
// structuredoutput.DecodeFinalAnswer 完成 JSON Schema 与业务语义校验；
// 只有通过校验的结果才会保存为最终答案。
func (r *ToolLoopRuntime) decodeToolLoopFinalAnswer(
	ctx context.Context,
	state toolLoopState,
) (toolLoopState, error) {
	if err := ctx.Err(); err != nil {
		return toolLoopState{}, err
	}
	// 解析元数据必须与最终回答使用的 Prompt 和路由来自同一份快照。
	references, err := resolveReferences(
		state.snapshot,
		finalPromptName,
		finalTask,
		finalTask,
	)
	if err != nil {
		return toolLoopState{}, err
	}
	value, report, err := structuredoutput.DecodeFinalAnswer(
		ctx,
		[]byte(state.finalResponse.Message.Content),
		structuredoutput.DecodeOptions{
			Metadata: structuredoutput.ExecutionMetadata{
				Provider:           references.Provider,
				Model:              references.Model,
				PromptVersion:      references.PromptVersion,
				SchemaVersion:      references.SchemaVersion,
				ModelConfigVersion: references.ModelConfigVersion,
			},
			Repair: r.finalAnswerRepair,
		},
	)
	if err != nil {
		return toolLoopState{}, runtimeFailure(
			"tool_loop_final_decode",
			fmt.Errorf("%w: %w", ErrFinalDecodeFailed, err),
		)
	}
	// 只有完整通过 Schema 和业务语义校验的结果才进入最终状态。
	state.finalAnswer = value
	state.finalAnswerReport = report
	return state, nil
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
	recordExecutionObservation(r.observer, execution, output.RunOutput, runErr)
	if execution == nil || execution.shadow || execution.recorder == nil {
		return
	}
	// 工具循环与普通 Runtime 共用同一个 RunStore 生命周期：
	// 先提交最终观测，再由 finishRun 写入 run.succeeded、run.failed
	// 或 run.canceled，并更新 Run 的最终状态。
	execution.recorder.finishRun(ctx, input, execution, runErr)
}

// callToolDecisionModel 使用当前 Run 快照中的工具决策路由调用模型。
//
// 这一阶段只获取模型的结构化决策文本，不把模型输出直接交给工具执行器；
// 后续还必须经过 DecodeToolDecision、授权回调和 runner.Executor。
func (r *ToolLoopRuntime) callToolDecisionModel(
	ctx context.Context,
	state toolLoopState,
) (toolLoopState, error) {
	if err := ctx.Err(); err != nil {
		return toolLoopState{}, err
	}
	// 工具决策路由来自 Run 快照，不使用全局默认模型，保证版本可追溯。
	route, err := routeFromSnapshot(state.snapshot, toolDecisionTask)
	if err != nil {
		return toolLoopState{}, err
	}
	recordRoute(state.execution, route)

	// 这里只请求模型给出调用意图，不允许模型响应直接触发工具。
	callStarted := time.Now()
	response, err := r.model.Chat(ctx, modelgateway.ChatRequest{
		Task:        toolDecisionTask,
		Provider:    route.Provider,
		Model:       route.Model,
		Temperature: float64Pointer(route.Temperature),
		MaxTokens:   maxTokensPointer(route.MaxTokens),
		Timeout:     route.Timeout,
		Messages: []modelgateway.Message{{
			Role:    "user",
			Content: state.toolDecisionPrompt,
		}},
	})
	if state.execution != nil && state.execution.recorder != nil {
		state.execution.recorder.recordModelCall(
			ctx,
			state.input,
			route,
			response,
			time.Since(callStarted),
			err,
		)
	}
	if err != nil {
		return toolLoopState{}, runtimeFailure(
			"tool_loop_tool_decision_model",
			fmt.Errorf("%w: %w", ErrToolDecisionModelFailed, err),
		)
	}
	// 原始响应只留在中间状态，下一阶段会执行严格结构化校验。
	state.toolDecisionResponse = response
	recordModelResponse(state.execution, state.snapshot, response, toolDecisionTask)
	return state, nil
}

// decodeToolDecision 对工具决策模型输出执行 Schema 和业务语义校验。
//
// 解析报告记录本次使用的 Prompt、Schema、模型和模型配置版本；
// 非法输出只能通过注入的 RepairFunc 获得一次修复机会。
func (r *ToolLoopRuntime) decodeToolDecision(
	ctx context.Context,
	state toolLoopState,
) (toolLoopState, error) {
	// 先确认 Prompt、Schema、Route 三种版本元数据都来自同一份快照。
	references, err := resolveReferences(
		state.snapshot,
		toolDecisionPromptName,
		toolDecisionTask,
		toolDecisionTask,
	)
	if err != nil {
		return toolLoopState{}, err
	}
	metadata := structuredoutput.ExecutionMetadata{
		Provider:           references.Provider,
		Model:              references.Model,
		PromptVersion:      references.PromptVersion,
		SchemaVersion:      references.SchemaVersion,
		ModelConfigVersion: references.ModelConfigVersion,
	}
	// DecodeToolDecision 会执行严格 JSON、Schema 和业务语义校验，
	// 必要时只调用一次注入的修复函数。
	value, report, err := structuredoutput.DecodeToolDecision(
		ctx,
		[]byte(state.toolDecisionResponse.Message.Content),
		structuredoutput.DecodeOptions{
			Metadata: metadata,
			Repair:   r.toolDecisionRepair,
		},
	)
	if err != nil {
		return toolLoopState{}, runtimeFailure(
			"tool_loop_tool_decision_decode",
			fmt.Errorf("%w: %w", ErrToolDecisionDecodeFailed, err),
		)
	}
	// 只有通过校验的 ToolDecision 才能进入授权阶段。
	state.toolDecision = value
	state.toolDecisionReport = report
	return state, nil
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
	if err := request.Validate(time.Now()); err != nil {
		return toolLoopState{}, runtimeFailure(
			"tool_loop_authorize",
			fmt.Errorf("%w: invalid authorized request", ErrToolAuthorizationFailed),
		)
	}
	// 只有完整请求通过校验后，才保存到状态供后续工具执行阶段使用。
	state.toolCallRequest = request
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
	var err error
	state, err = r.checkToolCallLimit(ctx, state)
	if err != nil {
		return state, err
	}
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
	callStarted := time.Now()
	result, err := r.executor.Execute(ctx, state.toolCallRequest)
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
			time.Since(callStarted),
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
		Record: record,
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

// encodeToolHistory 将内部工具结果转换成模型可见的稳定 JSON。
//
// 使用独立的 Prompt 类型，避免把内部结构字段或未来新增的敏感字段
// 意外暴露给模型。
func encodeToolHistory(history []toolHistoryEntry) (string, error) {
	type promptSourceRef struct {
		Source   string `json:"source"`
		Location string `json:"location"`
	}
	type promptHistoryEntry struct {
		ToolName        string            `json:"tool_name"`
		ToolVersion     string            `json:"tool_version"`
		Status          string            `json:"status"`
		ErrorCode       string            `json:"error_code,omitempty"`
		ArgumentsDigest string            `json:"arguments_digest"`
		Data            json.RawMessage   `json:"data,omitempty"`
		Source          []promptSourceRef `json:"source,omitempty"`
	}

	encodedHistory := make([]promptHistoryEntry, 0, len(history))
	for _, entry := range history {
		// 只映射允许进入 Prompt 的字段，不直接序列化内部 toolHistoryEntry。
		promptEntry := promptHistoryEntry{
			ToolName:        entry.Record.ToolName,
			ToolVersion:     entry.Record.ToolVersion,
			Status:          string(entry.Record.Status),
			ErrorCode:       entry.Record.ErrorCode,
			ArgumentsDigest: entry.Record.ArgumentsDigest,
		}
		if len(entry.Data) > 0 {
			// 工具结果必须保持合法 JSON，避免生成不可解析的历史内容。
			if !json.Valid(entry.Data) {
				return "", fmt.Errorf("%w: invalid tool data", ErrToolHistoryEncodingFailed)
			}
			promptEntry.Data = append(json.RawMessage(nil), entry.Data...)
		}
		if len(entry.Source) > 0 {
			// Source 只保留来源和位置，避免把内部对象字段扩散到模型输入。
			promptEntry.Source = make([]promptSourceRef, 0, len(entry.Source))
			for _, source := range entry.Source {
				promptEntry.Source = append(promptEntry.Source, promptSourceRef{
					Source:   source.Source,
					Location: source.Location,
				})
			}
		}
		encodedHistory = append(encodedHistory, promptEntry)
	}

	data, err := json.Marshal(encodedHistory)
	if err != nil {
		return "", fmt.Errorf("%w: marshal tool history", ErrToolHistoryEncodingFailed)
	}
	// 返回稳定 JSON 字符串，供 Prompt 模板通过 ToolHistory 变量使用。
	return string(data), nil
}
